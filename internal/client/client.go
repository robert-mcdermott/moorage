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
	"strconv"
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
	return c.RollbackWith(ctx, app, env, apitypes.RollbackRequest{Version: version})
}

// RollbackWith publishes an earlier release again, as req says.
func (c *Client) RollbackWith(ctx context.Context, app, env string, req apitypes.RollbackRequest) (apitypes.Deployment, error) {
	body, _ := json.Marshal(req)
	var d apitypes.Deployment
	err := c.do(ctx, http.MethodPost, envPath(app, env, "rollback"), bytes.NewReader(body), "application/json", &d)
	return d, err
}

// Vars lists an environment's variables (secrets without their values).
func (c *Client) Vars(ctx context.Context, app, env string) ([]apitypes.Var, error) {
	var out []apitypes.Var
	err := c.do(ctx, http.MethodGet, envPath(app, env, "vars"), nil, "", &out)
	return out, err
}

// ServerInfo describes the server, with the prefixes of the git remotes.
func (c *Client) ServerInfo(ctx context.Context) (apitypes.ServerInfo, error) {
	var out apitypes.ServerInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/server", nil, "", &out)
	return out, err
}

// SSHKeys lists the keys that may push over SSH.
func (c *Client) SSHKeys(ctx context.Context) ([]apitypes.SSHKey, error) {
	var out []apitypes.SSHKey
	err := c.do(ctx, http.MethodGet, "/api/v1/me/ssh-keys", nil, "", &out)
	return out, err
}

// AddSSHKey registers a public key, in authorized_keys format.
func (c *Client) AddSSHKey(ctx context.Context, k apitypes.NewSSHKey) (apitypes.SSHKey, error) {
	body, _ := json.Marshal(k)
	var out apitypes.SSHKey
	err := c.do(ctx, http.MethodPost, "/api/v1/me/ssh-keys", bytes.NewReader(body), "application/json", &out)
	return out, err
}

// RemoveSSHKey removes the key with this name or fingerprint.
func (c *Client) RemoveSSHKey(ctx context.Context, which string) (apitypes.SSHKey, error) {
	var out apitypes.SSHKey
	err := c.do(ctx, http.MethodDelete, "/api/v1/me/ssh-keys/"+url.PathEscape(which), nil, "", &out)
	return out, err
}

// ChangeVars sets and unsets variables; unless ch.NoRestart, it returns once
// the change is released.
func (c *Client) ChangeVars(ctx context.Context, app, env string, ch apitypes.VarsChange) (apitypes.VarsResult, error) {
	body, _ := json.Marshal(ch)
	var out apitypes.VarsResult
	err := c.do(ctx, http.MethodPatch, envPath(app, env, "vars"), bytes.NewReader(body), "application/json", &out)
	return out, err
}

// PackStats describes what Deploy uploaded.
type PackStats struct {
	Files   int
	Bytes   int64
	Skipped []string
}

// Deploy uploads the files under dir and publishes them following plan: a
// static site, or an app whose processes are run. skip, if set, leaves
// paths out of the upload. The upload is streamed: the directory is packed
// while it is sent. For an app, Deploy returns once the new release is
// running and healthy.
func (c *Client) Deploy(ctx context.Context, app, env, dir string, plan manifest.Plan, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	var d apitypes.Deployment
	stats, err := c.upload(ctx, app, env, dir, &plan, skip, &d)
	return d, stats, err
}

// DeploySource uploads the source under dir for the server to build and
// deploy, and returns the deployment as soon as it is queued; follow it
// with FollowDeployment.
func (c *Client) DeploySource(ctx context.Context, app, env, dir string, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	var d apitypes.Deployment
	stats, err := c.upload(ctx, app, env, dir, nil, skip, &d)
	return d, stats, err
}

// upload sends dir as a deploy: with a plan, a plan part and an archive
// part; without one, a source part.
func (c *Client) upload(ctx context.Context, app, env, dir string, plan *manifest.Plan, skip func(string, fs.DirEntry) bool, out any) (PackStats, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	var stats PackStats
	go func() {
		err := func() error {
			field := apitypes.SourcePart
			if plan != nil {
				part, err := mw.CreateFormField(apitypes.PlanPart)
				if err != nil {
					return err
				}
				if err := json.NewEncoder(part).Encode(plan); err != nil {
					return err
				}
				field = apitypes.ArchivePart
			}
			part, err := mw.CreateFormFile(field, "upload.tar.gz")
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
	err := c.do(ctx, http.MethodPost, envPath(app, env, "deployments"), pr, mw.FormDataContentType(), out)
	pr.Close()
	return stats, err
}

// Deployment returns a deployment.
func (c *Client) Deployment(ctx context.Context, id string) (apitypes.Deployment, error) {
	var d apitypes.Deployment
	err := c.do(ctx, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(id), nil, "", &d)
	return d, err
}

// Deployments lists an environment's latest deployments, newest first.
func (c *Client) Deployments(ctx context.Context, app, env string, limit int) ([]apitypes.Deployment, error) {
	var out []apitypes.Deployment
	err := c.do(ctx, http.MethodGet, envPath(app, env, "deployments")+"?limit="+strconv.Itoa(limit), nil, "", &out)
	return out, err
}

// FollowDeployment calls fn with each line of a deployment's log, as it is
// written, until the deployment ends (or ctx does), and returns the
// deployment as it ended.
func (c *Client) FollowDeployment(ctx context.Context, id string, fn func(apitypes.LogLine) error) (apitypes.Deployment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+"/api/v1/deployments/"+url.PathEscape(id)+"/log?follow=true", nil)
	if err != nil {
		return apitypes.Deployment{}, err
	}
	res, err := c.send(req)
	if err != nil {
		return apitypes.Deployment{}, err
	}
	dec := json.NewDecoder(res.Body)
	for {
		var l apitypes.LogLine
		if err := dec.Decode(&l); err != nil {
			res.Body.Close()
			if !errors.Is(err, io.EOF) {
				return apitypes.Deployment{}, err
			}
			break
		}
		if err := fn(l); err != nil {
			res.Body.Close()
			return apitypes.Deployment{}, err
		}
	}
	return c.Deployment(ctx, id)
}

// DeployStatic deploys the files under dir as a static site with rules.
func (c *Client) DeployStatic(ctx context.Context, app, env, dir string, rules manifest.Static, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	return c.Deploy(ctx, app, env, dir, manifest.Plan{Kind: manifest.KindStatic, Static: &rules}, skip)
}

// Instances lists an environment's instances.
func (c *Client) Instances(ctx context.Context, app, env string) ([]apitypes.Instance, error) {
	var out []apitypes.Instance
	err := c.do(ctx, http.MethodGet, envPath(app, env, "instances"), nil, "", &out)
	return out, err
}

// Restart replaces an environment's instances without downtime and returns
// the new ones.
func (c *Client) Restart(ctx context.Context, app, env string) ([]apitypes.Instance, error) {
	var out []apitypes.Instance
	err := c.do(ctx, http.MethodPost, envPath(app, env, "restart"), nil, "", &out)
	return out, err
}

// Logs calls fn with the last tail lines of an environment's log and, with
// follow, with new lines until ctx ends. process, if set, keeps one process.
func (c *Client) Logs(ctx context.Context, app, env, process string, tail int, follow bool, fn func(apitypes.LogLine) error) error {
	q := url.Values{"tail": {strconv.Itoa(tail)}}
	if follow {
		q.Set("follow", "1")
	}
	if process != "" {
		q.Set("process", process)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+envPath(app, env, "logs")+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.send(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var l apitypes.LogLine
		if err := dec.Decode(&l); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
	}
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
	if e.Detail != "" && len(e.Output) > 0 {
		return e.Detail + "\n  " + strings.Join(e.Output, "\n  ")
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
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.send(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// send sends an authenticated request and turns error responses into
// errors.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Err != nil {
			return nil, fmt.Errorf("cannot reach the server at %s: %w", c.base, uerr.Err)
		}
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		apiErr := &APIError{Status: res.StatusCode}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(data, &apiErr.Problem) != nil || apiErr.Detail == "" {
			apiErr.Detail = strings.TrimSpace(string(data))
		}
		return nil, apiErr
	}
	return res, nil
}
