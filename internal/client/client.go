// Package client talks to a Moorage server's API on behalf of the CLI.
package client

import (
	"bufio"
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
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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
	// Browsers and curl send *.localhost to this machine; resolvers do not
	// all do so.
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(host, ".localhost") {
			addr = net.JoinHostPort("127.0.0.1", port)
		}
		return dialer.DialContext(ctx, network, addr)
	}
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
	return allPages[apitypes.App](ctx, c, "/api/v1/apps")
}

// Releases lists an environment's releases, newest first.
func (c *Client) Releases(ctx context.Context, app, env string) ([]apitypes.Release, error) {
	return allPages[apitypes.Release](ctx, c, envPath(app, env, "releases"))
}

// allPages reads a paged list to its end.
func allPages[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T
	cursor := ""
	for {
		var page apitypes.Page[T]
		p := path
		if cursor != "" {
			p += "?cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, p, nil, "", &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			return all, nil
		}
		cursor = page.NextCursor
	}
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

// post sends a JSON document and decodes the answer into out, if any.
func (c *Client) post(ctx context.Context, method, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, bytes.NewReader(body), "application/json", out)
}

// Me says who the client's credentials are.
func (c *Client) Me(ctx context.Context) (apitypes.Me, error) {
	var out apitypes.Me
	err := c.do(ctx, http.MethodGet, "/api/v1/me", nil, "", &out)
	return out, err
}

// Logout revokes the token in use.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/auth/logout", nil, "", nil)
}

// StartDevice begins a sign-in to approve in a browser.
func (c *Client) StartDevice(ctx context.Context, client string) (apitypes.DeviceAuthorization, error) {
	var out apitypes.DeviceAuthorization
	err := c.post(ctx, http.MethodPost, "/api/v1/auth/device", apitypes.DeviceRequest{Client: client}, &out)
	return out, err
}

// DeviceToken asks whether a sign-in has been approved. Until then it fails
// with an *APIError whose Code says why.
func (c *Client) DeviceToken(ctx context.Context, deviceCode string) (apitypes.DeviceToken, error) {
	var out apitypes.DeviceToken
	err := c.post(ctx, http.MethodPost, "/api/v1/auth/device/token", apitypes.DeviceTokenRequest{DeviceCode: deviceCode}, &out)
	return out, err
}

// Tokens lists the caller's API tokens.
func (c *Client) Tokens(ctx context.Context) ([]apitypes.Token, error) {
	var out []apitypes.Token
	err := c.do(ctx, http.MethodGet, "/api/v1/me/tokens", nil, "", &out)
	return out, err
}

// CreateToken creates an API token and returns it with its secret.
func (c *Client) CreateToken(ctx context.Context, t apitypes.NewToken) (apitypes.CreatedToken, error) {
	var out apitypes.CreatedToken
	err := c.post(ctx, http.MethodPost, "/api/v1/me/tokens", t, &out)
	return out, err
}

// RevokeToken revokes the caller's token with this name or ID.
func (c *Client) RevokeToken(ctx context.Context, which string) (apitypes.Token, error) {
	var out apitypes.Token
	err := c.do(ctx, http.MethodDelete, "/api/v1/me/tokens/"+url.PathEscape(which), nil, "", &out)
	return out, err
}

// Users lists the users.
func (c *Client) Users(ctx context.Context) ([]apitypes.User, error) {
	return allPages[apitypes.User](ctx, c, "/api/v1/users")
}

// CreateUser creates a user, and returns their invitation.
func (c *Client) CreateUser(ctx context.Context, u apitypes.NewUser) (apitypes.Invitation, error) {
	var out apitypes.Invitation
	err := c.post(ctx, http.MethodPost, "/api/v1/users", u, &out)
	return out, err
}

// ChangeUser changes a user, by email address or ID.
func (c *Client) ChangeUser(ctx context.Context, which string, ch apitypes.UserChange) (apitypes.User, error) {
	var out apitypes.User
	err := c.post(ctx, http.MethodPatch, "/api/v1/users/"+url.PathEscape(which), ch, &out)
	return out, err
}

// InviteUser makes a new one-time link for a user to choose a password.
func (c *Client) InviteUser(ctx context.Context, which string) (apitypes.Invitation, error) {
	var out apitypes.Invitation
	err := c.do(ctx, http.MethodPost, "/api/v1/users/"+url.PathEscape(which)+"/invitation", nil, "", &out)
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
	stats, err := c.upload(ctx, app, env, dir, &plan, "", skip, &d)
	return d, stats, err
}

// DeploySource uploads the source under dir for the server to build and
// deploy, and returns the deployment as soon as it is queued; follow it
// with FollowDeployment.
func (c *Client) DeploySource(ctx context.Context, app, env, dir string, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	var d apitypes.Deployment
	stats, err := c.upload(ctx, app, env, dir, nil, "", skip, &d)
	return d, stats, err
}

// DeployImage asks the server to run an image, with the files of dir that skip
// leaves (no more than the app's manifest), and returns the deployment as soon
// as it is queued; follow it with FollowDeployment.
func (c *Client) DeployImage(ctx context.Context, app, env, image, dir string, skip func(string, fs.DirEntry) bool) (apitypes.Deployment, PackStats, error) {
	var d apitypes.Deployment
	stats, err := c.upload(ctx, app, env, dir, nil, image, skip, &d)
	return d, stats, err
}

// Registries lists the container registries the server has credentials for.
func (c *Client) Registries(ctx context.Context) ([]apitypes.Registry, error) {
	var out []apitypes.Registry
	err := c.do(ctx, http.MethodGet, "/api/v1/registries", nil, "", &out)
	return out, err
}

// RegistryLogin stores a container registry's credentials on the server.
func (c *Client) RegistryLogin(ctx context.Context, host string, login apitypes.RegistryLogin) (apitypes.Registry, error) {
	var out apitypes.Registry
	err := c.post(ctx, http.MethodPut, "/api/v1/registries/"+url.PathEscape(host), login, &out)
	return out, err
}

// RegistryLogout removes a container registry's credentials from the server.
func (c *Client) RegistryLogout(ctx context.Context, host string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/registries/"+url.PathEscape(host), nil, "", nil)
}

// upload sends dir as a deploy: with a plan, a plan part and an archive
// part; without one, a source part.
func (c *Client) upload(ctx context.Context, app, env, dir string, plan *manifest.Plan, image string, skip func(string, fs.DirEntry) bool, out any) (PackStats, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	var stats PackStats
	go func() {
		err := func() error {
			field := apitypes.SourcePart
			if image != "" {
				if err := mw.WriteField(apitypes.ImagePart, image); err != nil {
					return err
				}
			}
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
	var page apitypes.Page[apitypes.Deployment]
	err := c.do(ctx, http.MethodGet, envPath(app, env, "deployments")+"?limit="+strconv.Itoa(limit), nil, "", &page)
	return page.Items, err
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

// Events lists what happened, newest first: to an app and an environment, if
// given. limit bounds them.
func (c *Client) Events(ctx context.Context, app, env string, limit int) ([]apitypes.Event, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if app != "" {
		q.Set("app", app)
	}
	if env != "" {
		q.Set("env", env)
	}
	var page apitypes.Page[apitypes.Event]
	err := c.do(ctx, http.MethodGet, "/api/v1/events?"+q.Encode(), nil, "", &page)
	return page.Items, err
}

// FollowEvents calls fn with what happens, after the event named by after, until
// the server stops, fn fails or ctx ends.
func (c *Client) FollowEvents(ctx context.Context, app, env, after string, fn func(apitypes.Event) error) error {
	q := url.Values{}
	if app != "" {
		q.Set("app", app)
	}
	if env != "" {
		q.Set("env", env)
	}
	if after != "" {
		q.Set("after", after)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+"/api/v1/events/stream?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.send(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e apitypes.Event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
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

// Environment says how an environment stands.
func (c *Client) Environment(ctx context.Context, app, env string) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.do(ctx, http.MethodGet, envDoc(app, env), nil, "", &out)
	return out, err
}

// Scale sets how many instances of the named processes an environment runs.
func (c *Client) Scale(ctx context.Context, app, env string, formation map[string]int) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPatch, envDoc(app, env), apitypes.EnvironmentChange{Formation: formation}, &out)
	return out, err
}

// Redeploy builds what the current release was built from again.
func (c *Client) Redeploy(ctx context.Context, app, env string) (apitypes.Deployment, error) {
	var out apitypes.Deployment
	err := c.do(ctx, http.MethodPost, envPath(app, env, "redeploy"), nil, "", &out)
	return out, err
}

// Release returns a release, with what it runs.
func (c *Client) Release(ctx context.Context, app, env string, version int) (apitypes.ReleaseDetail, error) {
	var out apitypes.ReleaseDetail
	err := c.do(ctx, http.MethodGet, envPath(app, env, "releases/"+strconv.Itoa(version)), nil, "", &out)
	return out, err
}

// ReleaseDiff says what changed from one release to another.
func (c *Client) ReleaseDiff(ctx context.Context, app, env string, from, to int) (apitypes.ReleaseDiff, error) {
	var out apitypes.ReleaseDiff
	err := c.do(ctx, http.MethodGet, envPath(app, env, "releases/"+strconv.Itoa(from)+"/diff/"+strconv.Itoa(to)), nil, "", &out)
	return out, err
}

func envDoc(app, env string) string {
	return strings.TrimSuffix(envPath(app, env, ""), "/")
}
