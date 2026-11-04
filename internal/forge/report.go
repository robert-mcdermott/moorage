package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// The states of a commit, as a check on a pull request shows them.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
)

// Remote is a repository on a forge, as far as telling it things goes.
type Remote struct {
	Kind, APIURL, Repo, Token string
	Client                    *http.Client
}

// Where finds a forge's kind, the address of its API and the repository's path there, from the repository's address.
func Where(kind, repoURL, apiURL string) (Remote, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Remote{}, &InvalidError{"A repository is given by its https address, such as https://github.com/acme/shop.git"}
	}
	if u.User != nil {
		return Remote{}, &InvalidError{"Leave the token out of the address: it goes where it can be kept sealed."}
	}
	repo := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if kind == "" {
		switch u.Hostname() {
		case "github.com":
			kind = GitHub
		case "gitlab.com":
			kind = GitLab
		default:
			return Remote{}, &InvalidError{"Say which kind of forge this is: github, gitlab, gitea or generic."}
		}
	}
	switch kind {
	case GitHub, GitLab, Gitea, Generic:
	default:
		return Remote{}, &InvalidError{fmt.Sprintf("%q is not a kind of forge: github, gitlab, gitea or generic.", kind)}
	}
	if apiURL == "" {
		base := u.Scheme + "://" + u.Host
		switch {
		case kind == GitHub && u.Hostname() == "github.com":
			apiURL = "https://api.github.com"
		case kind == GitHub:
			apiURL = base + "/api/v3"
		case kind == GitLab:
			apiURL = base + "/api/v4"
		case kind == Gitea:
			apiURL = base + "/api/v1"
		}
	}
	return Remote{Kind: kind, APIURL: strings.TrimSuffix(apiURL, "/"), Repo: repo}, nil
}

// sentSafely says whether a token may be sent to this address: over https, or to something on the same network, as a
// forge of one's own may have no certificate.
func sentSafely(repoURL string) bool {
	u, err := url.Parse(repoURL)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := strings.ToLower(u.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
	}
	if host == "localhost" || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return true
	}
	for _, private := range []string{".localhost", ".local", ".lan", ".internal", ".home.arpa", ".test"} {
		if strings.HasSuffix(host, private) {
			return true
		}
	}
	return false
}

// Reports says whether anything can be told to this forge.
func (r Remote) Reports() bool { return r.Token != "" && r.Kind != Generic && r.APIURL != "" }

// SetStatus sets the check on a commit.
func (r Remote) SetStatus(ctx context.Context, sha, state, target, description string) error {
	if !r.Reports() {
		return nil
	}
	if len(description) > 120 { // what they take varies, and this is less than all of them
		description = description[:117] + "..."
	}
	if r.Kind == GitLab {
		if state == StateFailure {
			state = "failed"
		}
		_, err := r.send(ctx, http.MethodPost, "/projects/"+url.PathEscape(r.Repo)+"/statuses/"+sha, map[string]string{
			"state": state, "name": "moorage", "target_url": target, "description": description,
		})
		return err
	}
	_, err := r.send(ctx, http.MethodPost, "/repos/"+r.Repo+"/statuses/"+sha, map[string]string{
		"state": state, "context": "moorage", "target_url": target, "description": description,
	})
	return err
}

// Comment says something on a pull request, in place of what it said before, and says which comment that is.
func (r Remote) Comment(ctx context.Context, number int, id, text string) (string, error) {
	if !r.Reports() {
		return id, nil
	}
	var path string
	method := http.MethodPost
	switch r.Kind {
	case GitLab:
		path = "/projects/" + url.PathEscape(r.Repo) + "/merge_requests/" + strconv.Itoa(number) + "/notes"
		if id != "" {
			path, method = path+"/"+id, http.MethodPut
		}
	default:
		path = "/repos/" + r.Repo + "/issues/" + strconv.Itoa(number) + "/comments"
		if id != "" {
			path, method = "/repos/"+r.Repo+"/issues/comments/"+id, http.MethodPatch
		}
	}
	answer, err := r.send(ctx, method, path, map[string]string{"body": text})
	if err != nil {
		return id, err
	}
	var made struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(answer, &made); err == nil && made.ID != "" {
		return made.ID.String(), nil
	}
	return id, nil
}

func (r Remote) send(ctx context.Context, method, path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.APIURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "moorage")
	switch r.Kind {
	case GitLab:
		req.Header.Set("PRIVATE-TOKEN", r.Token)
	case Gitea:
		req.Header.Set("Authorization", "token "+r.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		// The address of the request says nothing secret, and the error may carry it: leave it at what went wrong.
		var plain *url.Error
		if errors.As(err, &plain) {
			return nil, fmt.Errorf("the forge cannot be reached: %w", plain.Err)
		}
		return nil, fmt.Errorf("the forge cannot be reached: %w", err)
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the forge said %s", res.Status)
	}
	return answer, nil
}
