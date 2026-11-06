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

	"github.com/coder/websocket"

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

// TurnOffSSO turns off signing in through a provider, which gives passwords back.
func (c *Client) TurnOffSSO(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/server/sso", nil, "", nil)
}

// SSO says how people sign in through a provider.
func (c *Client) SSO(ctx context.Context) (apitypes.SSOConfig, error) {
	var out apitypes.SSOConfig
	err := c.do(ctx, http.MethodGet, "/api/v1/server/sso", nil, "", &out)
	return out, err
}

// ConfigureSSO sets it up.
func (c *Client) ConfigureSSO(ctx context.Context, in apitypes.SSOConfig) (apitypes.SSOConfig, error) {
	var out apitypes.SSOConfig
	err := c.post(ctx, http.MethodPut, "/api/v1/server/sso", in, &out)
	return out, err
}

// ServerConfig reads the server's settings, with no secrets in them.
func (c *Client) ServerConfig(ctx context.Context) (apitypes.ServerConfig, error) {
	var out apitypes.ServerConfig
	err := c.do(ctx, http.MethodGet, "/api/v1/server/config", nil, "", &out)
	return out, err
}

// ImportServerConfig makes the server match a configuration, or says what that would do.
func (c *Client) ImportServerConfig(ctx context.Context, in apitypes.ServerConfig, dryRun bool) (apitypes.ConfigReport, error) {
	var out apitypes.ConfigReport
	path := "/api/v1/server/config/import"
	if dryRun {
		path += "?dry_run=true"
	}
	err := c.post(ctx, http.MethodPost, path, in, &out)
	return out, err
}

// Backups says where backups go, and what has been backed up.
func (c *Client) Backups(ctx context.Context) (apitypes.Backups, error) {
	var out apitypes.Backups
	err := c.do(ctx, http.MethodGet, "/api/v1/server/backups", nil, "", &out)
	return out, err
}

// StartBackup starts a backup.
func (c *Client) StartBackup(ctx context.Context) (apitypes.Backup, error) {
	var out apitypes.Backup
	err := c.do(ctx, http.MethodPost, "/api/v1/server/backups", nil, "", &out)
	return out, err
}

// ConfigureBackups says where backups go and how often.
func (c *Client) ConfigureBackups(ctx context.Context, in apitypes.BackupConfig) (apitypes.Backups, error) {
	var out apitypes.Backups
	err := c.post(ctx, http.MethodPut, "/api/v1/server/backups/config", in, &out)
	return out, err
}

// Notifications is what the caller has been told.
func (c *Client) Notifications(ctx context.Context) (apitypes.Inbox, error) {
	var out apitypes.Inbox
	err := c.do(ctx, http.MethodGet, "/api/v1/notifications", nil, "", &out)
	return out, err
}

// ReadNotifications marks all the caller has been told as read.
func (c *Client) ReadNotifications(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/v1/notifications/read", nil, "", nil)
}

// NotificationChannels lists where notifications can go.
func (c *Client) NotificationChannels(ctx context.Context) ([]apitypes.NotificationChannel, error) {
	var out []apitypes.NotificationChannel
	err := c.do(ctx, http.MethodGet, "/api/v1/notifications/channels", nil, "", &out)
	return out, err
}

// AddNotificationChannel adds a channel.
func (c *Client) AddNotificationChannel(ctx context.Context, in apitypes.NewNotificationChannel) (apitypes.CreatedChannel, error) {
	var out apitypes.CreatedChannel
	err := c.post(ctx, http.MethodPost, "/api/v1/notifications/channels", in, &out)
	return out, err
}

// RemoveNotificationChannel removes a channel, by its name or ID.
func (c *Client) RemoveNotificationChannel(ctx context.Context, channel string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/notifications/channels/"+url.PathEscape(channel), nil, "", nil)
}

// TestNotificationChannel sends a channel a test, by its name or ID.
func (c *Client) TestNotificationChannel(ctx context.Context, channel string) (apitypes.NotificationChannel, error) {
	var out apitypes.NotificationChannel
	err := c.do(ctx, http.MethodPost, "/api/v1/notifications/channels/"+url.PathEscape(channel)+"/test", nil, "", &out)
	return out, err
}

// NotificationRules lists the rules the caller can see.
func (c *Client) NotificationRules(ctx context.Context) ([]apitypes.NotificationRule, error) {
	var out []apitypes.NotificationRule
	err := c.do(ctx, http.MethodGet, "/api/v1/notifications/rules", nil, "", &out)
	return out, err
}

// AddNotificationRule adds a rule.
func (c *Client) AddNotificationRule(ctx context.Context, in apitypes.NewNotificationRule) (apitypes.NotificationRule, error) {
	var out apitypes.NotificationRule
	err := c.post(ctx, http.MethodPost, "/api/v1/notifications/rules", in, &out)
	return out, err
}

// RemoveNotificationRule removes a rule.
func (c *Client) RemoveNotificationRule(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/notifications/rules/"+url.PathEscape(id), nil, "", nil)
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

// DeploymentLog passes on a deployment's log as it stands, without waiting for the deployment to end.
func (c *Client) DeploymentLog(ctx context.Context, id string, fn func(apitypes.LogLine) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+"/api/v1/deployments/"+url.PathEscape(id)+"/log", nil)
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
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
	}
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

// LogQuery says which lines of an environment's logs to read.
type LogQuery struct {
	Process, Instance, Source, Grep, Since, Until string
	Problems                                      bool
	Tail                                          int
	Follow                                        bool
}

// Logs calls fn with the newest lines of an environment's logs and, with Follow, with
// new lines until ctx ends.
func (c *Client) Logs(ctx context.Context, app, env string, lq LogQuery, fn func(apitypes.LogLine) error) error {
	q := url.Values{"tail": {strconv.Itoa(lq.Tail)}}
	if lq.Follow {
		q.Set("follow", "1")
	}
	if lq.Problems {
		q.Set("problems", "1")
	}
	for name, value := range map[string]string{
		"process": lq.Process, "instance": lq.Instance, "source": lq.Source, "grep": lq.Grep, "since": lq.Since, "until": lq.Until,
	} {
		if value != "" {
			q.Set(name, value)
		}
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
	if failed := problemOf(res); failed != nil {
		return nil, failed
	}
	return res, nil
}

// problemOf turns a response that says something went wrong into an error.
func problemOf(res *http.Response) error {
	if res.StatusCode < 400 {
		return nil
	}
	defer res.Body.Close()
	apiErr := &APIError{Status: res.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if json.Unmarshal(data, &apiErr.Problem) != nil || apiErr.Detail == "" {
		apiErr.Detail = strings.TrimSpace(string(data))
	}
	return apiErr
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

// ChangePassword changes the caller's password.
func (c *Client) ChangePassword(ctx context.Context, current, next string) error {
	return c.post(ctx, http.MethodPost, "/api/v1/me/password", apitypes.PasswordChange{Current: current, New: next}, nil)
}

// Projects lists the projects the caller has a role in.
func (c *Client) Projects(ctx context.Context) ([]apitypes.Project, error) {
	var out []apitypes.Project
	err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, "", &out)
	return out, err
}

// Project returns a project, with its apps and members.
func (c *Client) Project(ctx context.Context, name string) (apitypes.Project, error) {
	var out apitypes.Project
	err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(name), nil, "", &out)
	return out, err
}

// CreateProject creates a project.
func (c *Client) CreateProject(ctx context.Context, name string) (apitypes.Project, error) {
	var out apitypes.Project
	err := c.post(ctx, http.MethodPost, "/api/v1/projects", apitypes.NewProject{Name: name}, &out)
	return out, err
}

// ChangeProject changes who may do what in a project without being a member.
func (c *Client) ChangeProject(ctx context.Context, name string, change apitypes.ProjectChange) (apitypes.Project, error) {
	var out apitypes.Project
	err := c.post(ctx, http.MethodPatch, "/api/v1/projects/"+url.PathEscape(name), change, &out)
	return out, err
}

// Environments lists an app's environments.
func (c *Client) Environments(ctx context.Context, app string) ([]apitypes.Environment, error) {
	var out []apitypes.Environment
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(app)+"/envs", nil, "", &out)
	return out, err
}

// Previews lists the previews of every app the caller can see.
func (c *Client) Previews(ctx context.Context) ([]apitypes.Environment, error) {
	var out []apitypes.Environment
	err := c.do(ctx, http.MethodGet, "/api/v1/previews", nil, "", &out)
	return out, err
}

// CreateEnvironment makes a named environment.
func (c *Client) CreateEnvironment(ctx context.Context, app string, req apitypes.NewEnvironment) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/envs", req, &out)
	return out, err
}

// DeleteEnvironment removes an environment, with its data, logs and releases.
func (c *Client) DeleteEnvironment(ctx context.Context, app, env string) error {
	return c.do(ctx, http.MethodDelete, envDoc(app, env), nil, "", nil)
}

// CreatePreview makes a preview of a branch that was pushed.
func (c *Client) CreatePreview(ctx context.Context, app, branch string) (apitypes.Deployment, error) {
	var out apitypes.Deployment
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/previews", apitypes.NewPreview{Branch: branch}, &out)
	return out, err
}

// PinEnvironment keeps a preview from expiring, or lets it.
func (c *Client) PinEnvironment(ctx context.Context, app, env string, pinned bool) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPatch, envDoc(app, env), apitypes.EnvironmentChange{Pinned: &pinned}, &out)
	return out, err
}

// Promote releases to an environment the build another one runs.
func (c *Client) Promote(ctx context.Context, app, from, to string) (apitypes.Deployment, error) {
	var out apitypes.Deployment
	err := c.post(ctx, http.MethodPost, envPath(app, to, "promote"), apitypes.Promotion{From: from}, &out)
	return out, err
}

// LockEnvironment stops deployments to an environment.
func (c *Client) LockEnvironment(ctx context.Context, app, env string) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPost, envPath(app, env, "lock"), struct{}{}, &out)
	return out, err
}

// UnlockEnvironment lets an environment take deployments again.
func (c *Client) UnlockEnvironment(ctx context.Context, app, env string) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPost, envPath(app, env, "unlock"), struct{}{}, &out)
	return out, err
}

// SleepEnvironment stops an environment's instances until a request wakes it.
func (c *Client) SleepEnvironment(ctx context.Context, app, env string) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPost, envPath(app, env, "sleep"), struct{}{}, &out)
	return out, err
}

// WakeEnvironment starts an environment that is asleep, and returns when it is ready.
func (c *Client) WakeEnvironment(ctx context.Context, app, env string) (apitypes.Environment, error) {
	var out apitypes.Environment
	err := c.post(ctx, http.MethodPost, envPath(app, env, "wake"), struct{}{}, &out)
	return out, err
}

// Metrics reads what an environment did over a range: live, 1h, 24h, 7d or 30d.
func (c *Client) Metrics(ctx context.Context, app, env, span string) (apitypes.Metrics, error) {
	var out apitypes.Metrics
	err := c.do(ctx, http.MethodGet, envPath(app, env, "metrics")+"?range="+url.QueryEscape(span), nil, "", &out)
	return out, err
}

// CronJobs lists an environment's cron jobs.
func (c *Client) CronJobs(ctx context.Context, app, env string) ([]apitypes.CronJob, error) {
	var out []apitypes.CronJob
	err := c.do(ctx, http.MethodGet, envPath(app, env, "cron"), nil, "", &out)
	return out, err
}

// RunCronJob runs a cron job now.
func (c *Client) RunCronJob(ctx context.Context, app, env, job string) (apitypes.Run, error) {
	var out apitypes.Run
	err := c.post(ctx, http.MethodPost, envPath(app, env, "cron/"+url.PathEscape(job)+"/run"), struct{}{}, &out)
	return out, err
}

// Runs lists an environment's runs, newest first; a job's, if it is named.
func (c *Client) Runs(ctx context.Context, app, env, job string) ([]apitypes.Run, error) {
	var out []apitypes.Run
	path := envPath(app, env, "runs")
	if job != "" {
		path += "?job=" + url.QueryEscape(job)
	}
	err := c.do(ctx, http.MethodGet, path, nil, "", &out)
	return out, err
}

// StartRun runs a command in an environment's current release.
func (c *Client) StartRun(ctx context.Context, app, env string, req apitypes.NewRun) (apitypes.Run, error) {
	var out apitypes.Run
	err := c.post(ctx, http.MethodPost, envPath(app, env, "runs"), req, &out)
	return out, err
}

// Run says how a run went.
func (c *Client) Run(ctx context.Context, app, id string) (apitypes.Run, error) {
	var out apitypes.Run
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(app)+"/runs/"+url.PathEscape(id), nil, "", &out)
	return out, err
}

// StopRun stops a run that is going on.
func (c *Client) StopRun(ctx context.Context, app, id string) (apitypes.Run, error) {
	var out apitypes.Run
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/runs/"+url.PathEscape(id)+"/stop", struct{}{}, &out)
	return out, err
}

// FollowRun passes what a run says to fn as it comes, and returns the run as it ended.
func (c *Client) FollowRun(ctx context.Context, app, id string, fn func(apitypes.LogLine) error) (apitypes.Run, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+"/api/v1/apps/"+url.PathEscape(app)+"/runs/"+url.PathEscape(id)+"/log?follow=true", nil)
	if err != nil {
		return apitypes.Run{}, err
	}
	res, err := c.send(req)
	if err != nil {
		return apitypes.Run{}, err
	}
	defer res.Body.Close()
	dec := json.NewDecoder(res.Body)
	for {
		var l apitypes.LogLine
		if err := dec.Decode(&l); err != nil {
			if !errors.Is(err, io.EOF) {
				return apitypes.Run{}, err
			}
			break
		}
		if err := fn(l); err != nil {
			return apitypes.Run{}, err
		}
	}
	return c.Run(ctx, app, id)
}

// RestartProcess replaces the instances of one process.
func (c *Client) RestartProcess(ctx context.Context, app, env, process string) ([]apitypes.Instance, error) {
	var out []apitypes.Instance
	err := c.post(ctx, http.MethodPost, envPath(app, env, "restart")+"?process="+url.QueryEscape(process), struct{}{}, &out)
	return out, err
}

// Domains lists an environment's domains.
func (c *Client) Domains(ctx context.Context, app, env string) ([]apitypes.Domain, error) {
	var out []apitypes.Domain
	err := c.do(ctx, http.MethodGet, envPath(app, env, "domains"), nil, "", &out)
	return out, err
}

// AddDomain attaches a domain to an environment.
func (c *Client) AddDomain(ctx context.Context, app, env string, req apitypes.NewDomain) (apitypes.Domain, error) {
	var out apitypes.Domain
	err := c.post(ctx, http.MethodPost, envPath(app, env, "domains"), req, &out)
	return out, err
}

// VerifyDomain looks now at where a domain points.
func (c *Client) VerifyDomain(ctx context.Context, app, env, address string) (apitypes.Domain, error) {
	var out apitypes.Domain
	err := c.post(ctx, http.MethodPost, envPath(app, env, "domains/verify"), apitypes.NewDomain{Address: address}, &out)
	return out, err
}

// RemoveDomain detaches a domain.
func (c *Client) RemoveDomain(ctx context.Context, app, env, address string) error {
	return c.post(ctx, http.MethodPost, envPath(app, env, "domains/remove"), apitypes.NewDomain{Address: address}, nil)
}

// Access says who may reach an environment.
func (c *Client) Access(ctx context.Context, app, env string) (apitypes.Access, error) {
	var out apitypes.Access
	err := c.do(ctx, http.MethodGet, envPath(app, env, "access"), nil, "", &out)
	return out, err
}

// SetAccess changes who may reach an environment.
func (c *Client) SetAccess(ctx context.Context, app, env string, change apitypes.AccessChange) (apitypes.Access, error) {
	var out apitypes.Access
	err := c.post(ctx, http.MethodPut, envPath(app, env, "access"), change, &out)
	return out, err
}

// ShareLinks lists an environment's share links.
func (c *Client) ShareLinks(ctx context.Context, app, env string) ([]apitypes.ShareLink, error) {
	var out []apitypes.ShareLink
	err := c.do(ctx, http.MethodGet, envPath(app, env, "share-links"), nil, "", &out)
	return out, err
}

// CreateShareLink makes a link that lets someone reach an environment.
func (c *Client) CreateShareLink(ctx context.Context, app, env string, req apitypes.NewShareLink) (apitypes.ShareLink, error) {
	var out apitypes.ShareLink
	err := c.post(ctx, http.MethodPost, envPath(app, env, "share-links"), req, &out)
	return out, err
}

// RevokeShareLink ends a share link.
func (c *Client) RevokeShareLink(ctx context.Context, app, env, id string) error {
	return c.do(ctx, http.MethodDelete, envPath(app, env, "share-links/"+url.PathEscape(id)), nil, "", nil)
}

// DeleteProject deletes a project that has no apps.
func (c *Client) DeleteProject(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/projects/"+url.PathEscape(name), nil, "", nil)
}

// SetMember gives someone a role in a project.
func (c *Client) SetMember(ctx context.Context, project, user, role string) (apitypes.Project, error) {
	var out apitypes.Project
	err := c.post(ctx, http.MethodPut, "/api/v1/projects/"+url.PathEscape(project)+"/members/"+url.PathEscape(user), apitypes.MemberChange{Role: role}, &out)
	return out, err
}

// RemoveMember takes someone out of a project.
func (c *Client) RemoveMember(ctx context.Context, project, user string) (apitypes.Project, error) {
	var out apitypes.Project
	err := c.do(ctx, http.MethodDelete, "/api/v1/projects/"+url.PathEscape(project)+"/members/"+url.PathEscape(user), nil, "", &out)
	return out, err
}

// CreateApp creates an app in a project.
func (c *Client) CreateApp(ctx context.Context, name, project string) (apitypes.App, error) {
	var out apitypes.App
	err := c.post(ctx, http.MethodPost, "/api/v1/apps", apitypes.NewApp{Name: name, Project: project}, &out)
	return out, err
}

// MoveApp puts an app in another project.
func (c *Client) MoveApp(ctx context.Context, name, project string) (apitypes.App, error) {
	var out apitypes.App
	err := c.post(ctx, http.MethodPatch, "/api/v1/apps/"+url.PathEscape(name), apitypes.AppChange{Project: project}, &out)
	return out, err
}

// RevealVar reads a secret's value, which is recorded.
func (c *Client) RevealVar(ctx context.Context, app, env, name string) (apitypes.RevealedVar, error) {
	var out apitypes.RevealedVar
	err := c.do(ctx, http.MethodPost, envPath(app, env, "vars/"+url.PathEscape(name)+"/reveal"), nil, "", &out)
	return out, err
}

// AuditQuery says which records of who did what to read.
type AuditQuery struct {
	Actor, Action, Target string
	Limit                 int
}

// Audit lists who did what, newest first.
func (c *Client) Audit(ctx context.Context, f AuditQuery) ([]apitypes.AuditEntry, error) {
	q := url.Values{"limit": {strconv.Itoa(f.Limit)}}
	for name, value := range map[string]string{"actor": f.Actor, "action": f.Action, "target": f.Target} {
		if value != "" {
			q.Set(name, value)
		}
	}
	var page apitypes.Page[apitypes.AuditEntry]
	err := c.do(ctx, http.MethodGet, "/api/v1/audit?"+q.Encode(), nil, "", &page)
	return page.Items, err
}

// AppInfo returns an app, its project, and how much room it takes.
func (c *Client) AppInfo(ctx context.Context, name string) (apitypes.AppInfo, error) {
	var out apitypes.AppInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(name), nil, "", &out)
	return out, err
}

// RenameApp gives an app another name.
func (c *Client) RenameApp(ctx context.Context, name, to string) (apitypes.App, error) {
	var out apitypes.App
	err := c.post(ctx, http.MethodPatch, "/api/v1/apps/"+url.PathEscape(name), apitypes.AppChange{Name: to}, &out)
	return out, err
}

// DeleteApp removes an app.
func (c *Client) DeleteApp(ctx context.Context, name string, keepData bool) error {
	path := "/api/v1/apps/" + url.PathEscape(name)
	if keepData {
		path += "?keep_data=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, "", nil)
}

// Forge says where an app's code is kept.
func (c *Client) Forge(ctx context.Context, app string) (apitypes.ForgeConnection, error) {
	var out apitypes.ForgeConnection
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(app)+"/forge", nil, "", &out)
	return out, err
}

// ConnectForge connects an app to a repository on a forge.
func (c *Client) ConnectForge(ctx context.Context, app string, in apitypes.ForgeConnect) (apitypes.ForgeConnection, error) {
	var out apitypes.ForgeConnection
	err := c.post(ctx, http.MethodPut, "/api/v1/apps/"+url.PathEscape(app)+"/forge", in, &out)
	return out, err
}

// DisconnectForge lets go of an app's repository.
func (c *Client) DisconnectForge(ctx context.Context, app string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/apps/"+url.PathEscape(app)+"/forge", nil, "", nil)
}

// ForgePulls lists an app's open pull requests.
func (c *Client) ForgePulls(ctx context.Context, app string) ([]apitypes.ForgePull, error) {
	var out []apitypes.ForgePull
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(app)+"/forge/pulls", nil, "", &out)
	return out, err
}

// ApproveForgePull has a pull request built, at the commit that someone looked at.
func (c *Client) ApproveForgePull(ctx context.Context, app string, number int, commit string) (apitypes.ForgePull, error) {
	var out apitypes.ForgePull
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/forge/pulls/"+strconv.Itoa(number)+"/approve", apitypes.ForgeApproval{Commit: commit}, &out)
	return out, err
}

// SyncForge has the server look at an app's repository now.
func (c *Client) SyncForge(ctx context.Context, app string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/forge/sync", nil, "", nil)
}

// TerminalOptions say what a terminal is for.
type TerminalOptions struct {
	Command string
	// Instance is a running container to run it in, rather than a new one-off instance.
	Instance string
	Timeout  string
	// TTY gives it a terminal of Rows and Cols, of the kind Term. Without one, it has pipes.
	TTY        bool
	Rows, Cols int
	Term       string
}

// Terminal is a command with someone at the other end of it: Read has what it shows, and Write types at it.
type Terminal struct {
	conn *websocket.Conn
	ctx  context.Context
	left []byte
	// Run is the run that this is.
	Run string
	end *apitypes.TerminalMessage
}

// Terminal runs a command in an environment, for someone to type at.
func (c *Client) Terminal(ctx context.Context, app, env string, o TerminalOptions) (*Terminal, error) {
	q := url.Values{"command": {o.Command}}
	if o.Instance != "" {
		q.Set("instance", o.Instance)
	}
	if o.Timeout != "" {
		q.Set("timeout", o.Timeout)
	}
	if o.TTY {
		q.Set("tty", "1")
		q.Set("rows", strconv.Itoa(o.Rows))
		q.Set("cols", strconv.Itoa(o.Cols))
		q.Set("term", o.Term)
	}
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	conn, res, err := websocket.Dial(ctx, c.base.String()+envPath(app, env, "terminal")+"?"+q.Encode(), &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: header, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			if failed := problemOf(res); failed != nil {
				return nil, failed
			}
		}
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	return &Terminal{conn: conn, ctx: ctx}, nil
}

// Read reads what the command shows, until it is over.
func (t *Terminal) Read(p []byte) (int, error) {
	for len(t.left) == 0 {
		kind, data, err := t.conn.Read(t.ctx)
		if err != nil {
			if t.end != nil {
				return 0, io.EOF
			}
			return 0, err
		}
		if kind == websocket.MessageBinary {
			t.left = data
			continue
		}
		var said apitypes.TerminalMessage
		if json.Unmarshal(data, &said) != nil {
			continue
		}
		if said.Run != "" {
			t.Run = said.Run
		}
		if said.Ended {
			t.end = &said
			return 0, io.EOF
		}
	}
	n := copy(p, t.left)
	t.left = t.left[n:]
	return n, nil
}

// Write types at the command.
func (t *Terminal) Write(p []byte) (int, error) {
	if err := t.conn.Write(t.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *Terminal) say(m apitypes.TerminalMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return t.conn.Write(t.ctx, websocket.MessageText, data)
}

// Resize says that the terminal is another size now.
func (t *Terminal) Resize(rows, cols int) error {
	return t.say(apitypes.TerminalMessage{Rows: rows, Cols: cols})
}

// CloseInput says that there is no more to type.
func (t *Terminal) CloseInput() error { return t.say(apitypes.TerminalMessage{EOF: true}) }

// Ended says how it ended, once Read has said that it is over.
func (t *Terminal) Ended() (code *int, problem string, over bool) {
	if t.end == nil {
		return nil, "", false
	}
	return t.end.ExitCode, t.end.Error, true
}

// Close goes away, which stops the command if it is still going.
func (t *Terminal) Close() error { return t.conn.CloseNow() }

func dataPath(app, env, more, at string, extra url.Values) string {
	q := url.Values{"path": {at}}
	for k, v := range extra {
		q[k] = v
	}
	return envPath(app, env, "files"+more) + "?" + q.Encode()
}

// DataFiles lists a directory within an environment's data directory.
func (c *Client) DataFiles(ctx context.Context, app, env, at string) (apitypes.DataListing, error) {
	var out apitypes.DataListing
	err := c.do(ctx, http.MethodGet, dataPath(app, env, "", at, nil), nil, "", &out)
	return out, err
}

// GetDataFile reads a file within an environment's data directory. Whoever asks closes what it answers with.
func (c *Client) GetDataFile(ctx context.Context, app, env, at string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+dataPath(app, env, "/content", at, nil), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.send(req)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// PutDataFile writes a file within an environment's data directory.
func (c *Client) PutDataFile(ctx context.Context, app, env, at string, from io.Reader, replace bool) (apitypes.DataFile, error) {
	var out apitypes.DataFile
	extra := url.Values{}
	if replace {
		extra.Set("replace", "true")
	}
	err := c.do(ctx, http.MethodPut, dataPath(app, env, "/content", at, extra), from, "application/octet-stream", &out)
	return out, err
}

// MakeDataDir makes a directory within an environment's data directory.
func (c *Client) MakeDataDir(ctx context.Context, app, env, at string) error {
	return c.do(ctx, http.MethodPost, dataPath(app, env, "/directory", at, nil), nil, "", nil)
}

// RemoveData removes something within an environment's data directory.
func (c *Client) RemoveData(ctx context.Context, app, env, at string, recursive bool) error {
	extra := url.Values{}
	if recursive {
		extra.Set("recursive", "true")
	}
	return c.do(ctx, http.MethodDelete, dataPath(app, env, "", at, extra), nil, "", nil)
}

// TelemetrySinks lists where what apps say and do is sent.
func (c *Client) TelemetrySinks(ctx context.Context) ([]apitypes.TelemetrySink, error) {
	var out []apitypes.TelemetrySink
	err := c.do(ctx, http.MethodGet, "/api/v1/server/telemetry", nil, "", &out)
	return out, err
}

// SetTelemetrySink keeps a sink, by its name.
func (c *Client) SetTelemetrySink(ctx context.Context, in apitypes.NewTelemetrySink) (apitypes.TelemetrySink, error) {
	var out apitypes.TelemetrySink
	err := c.post(ctx, http.MethodPost, "/api/v1/server/telemetry", in, &out)
	return out, err
}

// RemoveTelemetrySink lets go of a sink.
func (c *Client) RemoveTelemetrySink(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/server/telemetry/"+url.PathEscape(name), nil, "", nil)
}

// TestTelemetrySink sends a line to a sink now.
func (c *Client) TestTelemetrySink(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/server/telemetry/"+url.PathEscape(name)+"/test", nil, "", nil)
}

// Thresholds lists when someone is told about how an environment is doing.
func (c *Client) Thresholds(ctx context.Context, app, env string) ([]apitypes.Threshold, error) {
	var out []apitypes.Threshold
	err := c.do(ctx, http.MethodGet, envPath(app, env, "thresholds"), nil, "", &out)
	return out, err
}

// AddThreshold says when to tell someone about how an environment is doing.
func (c *Client) AddThreshold(ctx context.Context, app, env string, in apitypes.NewThreshold) (apitypes.Threshold, error) {
	var out apitypes.Threshold
	err := c.post(ctx, http.MethodPost, envPath(app, env, "thresholds"), in, &out)
	return out, err
}

// RemoveThreshold lets go of a threshold.
func (c *Client) RemoveThreshold(ctx context.Context, app, env, id string) error {
	return c.do(ctx, http.MethodDelete, envPath(app, env, "thresholds/"+url.PathEscape(id)), nil, "", nil)
}

// Services lists an environment's services.
func (c *Client) Services(ctx context.Context, app, env string) ([]apitypes.Service, error) {
	var out []apitypes.Service
	err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services", nil, "", &out)
	return out, err
}

// ServiceLog reads the newest of what a service has said.
func (c *Client) ServiceLog(ctx context.Context, app, env, name string, tail int) (string, error) {
	var out apitypes.ServiceLog
	path := fmt.Sprintf("/api/v1/apps/%s/envs/%s/services/%s/logs?tail=%d", url.PathEscape(app), url.PathEscape(env), url.PathEscape(name), tail)
	err := c.do(ctx, http.MethodGet, path, nil, "", &out)
	return out.Log, err
}

// RemoveService removes a service, with what it holds.
func (c *Client) RemoveService(ctx context.Context, app, env, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name), nil, "", nil)
}

// UpdateService takes the newest image of a service's version.
func (c *Client) UpdateService(ctx context.Context, app, env, name string) (string, error) {
	var out apitypes.ServiceChange
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name)+"/update", struct{}{}, &out)
	return out.Log, err
}

// UpgradeService moves a service to a newer major version.
func (c *Client) UpgradeService(ctx context.Context, app, env, name, version, image string) (string, error) {
	var out apitypes.ServiceChange
	err := c.post(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name)+"/upgrade", apitypes.ServiceUpgrade{Version: version, Image: image}, &out)
	return out.Log, err
}

// FinishServiceUpgrade lets go of what a service held before its last upgrade.
func (c *Client) FinishServiceUpgrade(ctx context.Context, app, env, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name)+"/old-volume", nil, "", nil)
}

// BackService puts a service back on what it held before its last upgrade or restore.
func (c *Client) BackService(ctx context.Context, app, env, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name)+"/back", nil, "", nil)
}

// RestoreService puts a service back to its latest dump.
func (c *Client) RestoreService(ctx context.Context, app, env, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+url.PathEscape(app)+"/envs/"+url.PathEscape(env)+"/services/"+url.PathEscape(name)+"/restore", nil, "", nil)
}
