package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// partSize is how much of a backup goes up at a time. A service takes 10,000 parts of an upload, so
// that this allows for about 160 GB.
const partSize = 16 << 20

// S3 keeps backups in a bucket of an S3-compatible service. It speaks only what backups need, which
// spares the server a large dependency: uploading in parts, listing, reading and deleting.
type S3 struct {
	Bucket, Prefix string
	Credentials
	// Client, if set, overrides how requests are sent (tests).
	Client *http.Client
	// Now, if set, overrides the time (tests).
	Now func() time.Time
}

func (s *S3) String() string {
	if s.Prefix == "" {
		return "s3://" + s.Bucket
	}
	return "s3://" + s.Bucket + "/" + s.Prefix
}

func (s *S3) region() string {
	if s.Region == "" {
		return "us-east-1"
	}
	return s.Region
}

// url says where a key is. A service of one's own usually has buckets as paths, and AWS as hostnames.
func (s *S3) url(key string, query url.Values) *url.URL {
	u := &url.URL{Scheme: "https", Host: "s3." + s.region() + ".amazonaws.com", Path: "/" + s.Bucket + "/" + key}
	if s.Endpoint != "" {
		if parsed, err := url.Parse(s.Endpoint); err == nil && parsed.Host != "" {
			u.Scheme, u.Host = parsed.Scheme, parsed.Host
		}
	} else {
		u.Host, u.Path = s.Bucket+".s3."+s.region()+".amazonaws.com", "/"+key
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u
}

func (s *S3) key(name string) string {
	if s.Prefix == "" {
		return name
	}
	return s.Prefix + "/" + name
}

func (s *S3) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sign(req, body, s.AccessKey, s.SecretKey, s.region(), "s3", now().UTC())
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, urlErr.Err
		}
		return nil, err
	}
	return res, nil
}

// failure says what a service answered, which says more in its body than in its status.
func failure(what string, res *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var problem struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(data, &problem) == nil && problem.Code != "" {
		return fmt.Errorf("%s: %s: %s", what, problem.Code, problem.Message)
	}
	return fmt.Errorf("%s: %s", what, res.Status)
}

// sign adds what AWS Signature Version 4 asks. The body's hash goes in, so that a request cannot be
// changed on its way.
func sign(req *http.Request, body []byte, accessKey, secretKey, region, service string, at time.Time) {
	stamp, day := at.Format("20060102T150405Z"), at.Format("20060102")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	authorization, _ := signature(req, payload, accessKey, secretKey, region, service, stamp, day)
	req.Header.Set("Authorization", authorization)
}

func signature(req *http.Request, payload, accessKey, secretKey, region, service, stamp, day string) (string, string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "content-type" {
			headers[lower] = strings.Join(values, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + strings.TrimSpace(headers[name]) + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{req.Method, encodePath(req.URL.EscapedPath()), canonicalQuery(req.URL.Query()), canonical.String(), signed, payload}, "\n")
	hashed := sha256.Sum256([]byte(request))
	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hex.EncodeToString(hashed[:])}, "\n")
	key := hmacOf([]byte("AWS4"+secretKey), day)
	key = hmacOf(key, region)
	key = hmacOf(key, service)
	key = hmacOf(key, "aws4_request")
	sig := hex.EncodeToString(hmacOf(key, toSign))
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig, sig
}

func encodePath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func canonicalQuery(q url.Values) string {
	var parts []string
	for name, values := range q {
		for _, v := range values {
			parts = append(parts, awsEscape(name)+"="+awsEscape(v))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func awsEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

func hmacOf(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Put uploads in parts as what r gives arrives, so that a backup need fit neither in memory nor on a disk
// first. One that fails is aborted, and leaves nothing behind.
func (s *S3) Put(ctx context.Context, name string, r io.Reader) error {
	key := s.key(name)
	first := make([]byte, partSize)
	n, err := io.ReadFull(r, first)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		// Small enough for one request.
		res, err := s.do(ctx, http.MethodPut, s.url(key, nil), first[:n])
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode/100 != 2 {
			return failure("upload", res)
		}
		return nil
	}
	if err != nil {
		return err
	}
	res, err := s.do(ctx, http.MethodPost, s.url(key, url.Values{"uploads": {""}}), []byte{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return failure("start an upload", res)
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&started); err != nil {
		return fmt.Errorf("start an upload: %w", err)
	}
	type part struct {
		XMLName xml.Name `xml:"Part"`
		Number  int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []part
	abort := func() {
		if res, err := s.do(context.WithoutCancel(ctx), http.MethodDelete, s.url(key, url.Values{"uploadId": {started.UploadID}}), []byte{}); err == nil {
			res.Body.Close()
		}
	}
	buf := first
	for number := 1; ; number++ {
		query := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {started.UploadID}}
		res, err := s.do(ctx, http.MethodPut, s.url(key, query), buf[:n])
		if err != nil {
			abort()
			return err
		}
		if res.StatusCode/100 != 2 {
			err := failure("upload a part", res)
			res.Body.Close()
			abort()
			return err
		}
		res.Body.Close()
		parts = append(parts, part{Number: number, ETag: res.Header.Get("ETag")})
		n, err = io.ReadFull(r, buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			abort()
			return err
		}
	}
	body, err := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part
	}{Parts: parts})
	if err != nil {
		abort()
		return err
	}
	done, err := s.do(ctx, http.MethodPost, s.url(key, url.Values{"uploadId": {started.UploadID}}), body)
	if err != nil {
		abort()
		return err
	}
	defer done.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(done.Body, 1<<16))
	// A service can answer 200 and still say in the body that this failed.
	if done.StatusCode/100 != 2 || bytes.Contains(answer, []byte("<Error>")) {
		abort()
		done.Body = io.NopCloser(bytes.NewReader(answer))
		return failure("finish an upload", done)
	}
	return nil
}

// Get reads an object.
func (s *S3) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	res, err := s.do(ctx, http.MethodGet, s.url(s.key(name), nil), []byte{})
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()
		return nil, failure("read "+name, res)
	}
	return res.Body, nil
}

// List lists the backups there, newest first.
func (s *S3) List(ctx context.Context) ([]Stored, error) {
	var out []Stored
	token := ""
	for {
		query := url.Values{"list-type": {"2"}}
		if s.Prefix != "" {
			query.Set("prefix", s.Prefix+"/")
		}
		if token != "" {
			query.Set("continuation-token", token)
		}
		u := s.url("", query)
		res, err := s.do(ctx, http.MethodGet, u, []byte{})
		if err != nil {
			return nil, err
		}
		if res.StatusCode/100 != 2 {
			defer res.Body.Close()
			return nil, failure("list the bucket", res)
		}
		var page struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list the bucket: %w", err)
		}
		for _, c := range page.Contents {
			name := strings.TrimPrefix(c.Key, s.Prefix+"/")
			if !strings.Contains(name, "/") && isBackupName(name) {
				out = append(out, Stored{name, c.Size, c.LastModified})
			}
		}
		if !page.Truncated || page.Next == "" {
			break
		}
		token = page.Next
	}
	sortStored(out)
	return out, nil
}

// Delete removes an object.
func (s *S3) Delete(ctx context.Context, name string) error {
	res, err := s.do(ctx, http.MethodDelete, s.url(s.key(name), nil), []byte{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return failure("delete "+name, res)
	}
	return nil
}
