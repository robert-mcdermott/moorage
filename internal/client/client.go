// Package client talks to a Moorage server's API on behalf of the CLI.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/archive"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

// Client is a connection to one server.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New returns a client for the server at serverURL, authenticating with
// token. caFile, if set, names a PEM file of CA certificates to trust in
// addition to the system's (for a server using Moorage's internal CA).
func New(serverURL, token, caFile string) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server address %q must be an http:// or https:// URL", serverURL)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s contains no PEM certificate", caFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{base: u, token: token, http: &http.Client{Transport: transport}}, nil
}

// Server returns the server's address.
func (c *Client) Server() string { return c.base.String() }

// Apps lists the apps.
func (c *Client) Apps(ctx context.Context) ([]apitypes.App, error) {
	var apps []apitypes.App
	err := c.do(ctx, http.MethodGet, "/api/v1/apps", nil, "", &apps)
	return apps, err
}

// Releases lists an environment's releases, newest first.
func (c *Client) Releases(ctx context.Context, app, env string) ([]apitypes.Release, error) {
	var rels []apitypes.Release
	err := c.do(ctx, http.MethodGet, envPath(app, env, "releases"), nil, "", &rels)
	return rels, err
}

// Rollback publishes an earlier release again; version 0 means the release
// before the current one.
func (c *Client) Rollback(ctx context.Context, app, env string, version int) (apitypes.Deployment, error) {
	body, _ := json.Marshal(apitypes.RollbackRequest{Version: version})
	var d apitypes.Deployment
	err := c.do(ctx, http.MethodPost, envPath(app, env, "rollback"), bytes.NewReader(body), "application/json", &d)
	return d, err
}

// PackStats describes what DeployStatic uploaded.
type PackStats struct {
	Files   int
	Bytes   int64
	Skipped []string
}

// DeployStatic uploads the files under dir as a static site and publishes
// them with rules. skip, if set, leaves paths out of the upload. The upload
// is streamed: the directory is packed while it is sent.
func (c *Client) DeployStatic(ctx context.Context, app, env, dir string, rules manifest.Static, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	var stats PackStats
	go func() {
		err := func() error {
			part, err := mw.CreateFormField(apitypes.StaticPart)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(rules); err != nil {
				return err
			}
			part, err = mw.CreateFormFile(apitypes.ArchivePart, "site.tar.gz")
			if err != nil {
				return err
			}
			stats.Files, stats.Bytes, stats.Skipped, err = archive.Write(part, dir, skip)
			if err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	var d apitypes.Deployment
	err := c.do(ctx, http.MethodPost, envPath(app, env, "deployments"), pr, mw.FormDataContentType(), &d)
	pr.Close()
	return d, stats, err
}

func envPath(app, env, rest string) string {
	return "/api/v1/apps/" + url.PathEscape(app) + "/envs/" + url.PathEscape(env) + "/" + rest
}

// APIError is an error answered by the server.
type APIError struct {
	Status int
	apitypes.Problem
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "the server did not accept the token: pass --token or set MOOR_TOKEN (on the server, the token is in keys/admin-token in the data directory)"
	}
	if e.Detail != "" {
		return e.Detail
	}
	return fmt.Sprintf("the server answered %d %s", e.Status, http.StatusText(e.Status))
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			return fmt.Errorf("cannot reach the server at %s: %w", c.base, uerr.Err)
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		apiErr := &APIError{Status: res.StatusCode}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(data, &apiErr.Problem) != nil || apiErr.Detail == "" {
			apiErr.Detail = strings.TrimSpace(string(data))
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}
