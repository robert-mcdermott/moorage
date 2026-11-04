package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/notify"
)

// hookReceiver is a webhook's other end, with a real server sending to it.
type hookReceiver struct {
	*httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	sigs []string
	raw  [][]byte
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	h := &hookReceiver{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		json.Unmarshal(body, &doc)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.got, h.raw = append(h.got, doc), append(h.raw, body)
		h.sigs = append(h.sigs, r.Header.Get("X-Moorage-Signature"))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookReceiver) ofType(kind string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, m := range h.got {
		if m["type"] == kind {
			out = append(out, m)
		}
	}
	return out
}

// TestNotifications has a failed deployment reach a webhook and the console, and then the news that
// it recovered, as set up in the API and the pages.
func TestNotifications(t *testing.T) {
	cfg := testConfig(t)
	cfg.Notifications.Allow = []string{"127.0.0.1/32"}
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	h := newHookReceiver(t)

	created, err := c.AddNotificationChannel(ctx, apitypes.NewNotificationChannel{Name: "ops", Kind: "webhook", URL: h.URL + "/secret/path"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" || created.Target == "" || strings.Contains(created.Target, "secret") {
		t.Fatalf("%+v", created)
	}
	if _, err := c.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", App: "shop", Events: []string{"deployment.failed"}}); err == nil {
		t.Fatal("a rule of an app that does not exist")
	}
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	rule, err := c.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", App: "shop", Events: []string{"deployment.failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Channel != "ops" || rule.App != "shop" {
		t.Fatalf("%+v", rule)
	}

	// What is sealed is not in the clear, and is not given back.
	channels, err := c.NotificationChannels(ctx)
	if err != nil || len(channels) != 1 || channels[0].Kind != "webhook" {
		t.Fatalf("%+v %v", channels, err)
	}
	if raw, _ := json.Marshal(channels); strings.Contains(string(raw), "secret") || strings.Contains(string(raw), created.Secret) {
		t.Fatalf("%s", raw)
	}
	db := testAuth(t, cfg).DB
	var sealed []byte
	if err := db.Reader.QueryRowContext(ctx, `SELECT sealed FROM notification_channels`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "secret/path") || strings.Contains(string(sealed), created.Secret) {
		t.Fatal("the channel is kept in the clear")
	}

	// A test goes, and says that it went.
	if _, err := c.TestNotificationChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if len(h.ofType("test")) != 1 {
		t.Fatalf("%+v", h.got)
	}

	// A deployment fails: the webhook is told, with a link and a signature its secret confirms, and
	// so is the console.
	if _, err := deployApp(t, c, "shop", appPlan(map[string]string{"TESTAPP_FAIL": "1"})); err == nil {
		t.Fatal("it was to fail")
	}
	waitUntil(t, "the webhook to be told", func() bool { return len(h.ofType("deployment.failed")) == 1 })
	m := h.ofType("deployment.failed")[0]
	if m["app"] != "shop" || !strings.Contains(m["link"].(string), "/deployments/dep_") {
		t.Fatalf("%v", m)
	}
	h.mu.Lock()
	last := len(h.raw) - 1
	signature, body := h.sigs[last], h.raw[last]
	h.mu.Unlock()
	stamp := regexp.MustCompile(`^t=(\d+),v1=`).FindStringSubmatch(signature)
	if stamp == nil {
		t.Fatalf("%q", signature)
	}
	if !notifySignatureMatches(created.Secret, stamp[1], body, signature) {
		t.Fatalf("the signature %q does not match", signature)
	}

	// The sidebar says so, and the page says what.
	_, page := owner.get("/")
	if !strings.Contains(page, `class="nav-count"`) {
		t.Errorf("no unread count:\n%s", page)
	}
	res, page := owner.get("/notifications")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "/deployments/dep_") || !strings.Contains(page, `class="unread"`) {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := owner.post("/notifications/read", url.Values{"csrf": {owner.token()}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("reading: %d\n%s", res.StatusCode, page)
	}
	if _, page := owner.get("/notifications"); strings.Contains(page, `class="unread"`) || strings.Contains(page, `class="nav-count"`) {
		t.Errorf("after reading:\n%s", page)
	}

	// The API has the same, and the local administrator has nothing of its own.
	token := tokenOf(t, owner, "read")
	mine := tokenClient(t, c, token)
	inbox, err := mine.Notifications(ctx)
	if err != nil || len(inbox.Items) != 1 || inbox.Unread != 0 || inbox.Items[0].ReadAt == nil || !strings.HasPrefix(inbox.Items[0].Link, "http") {
		t.Errorf("%+v %v", inbox, err)
	}
	if inbox, err := c.Notifications(ctx); err != nil || len(inbox.Items) != 0 {
		t.Errorf("%+v %v", inbox, err)
	}

	// It recovers: that goes to whatever heard it was wrong.
	if _, err := deployApp(t, c, "shop", appPlan(nil)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the word that it recovered", func() bool { return len(h.ofType(notify.Recovered)) == 1 })

	// Removing the channel takes its rules with it.
	if err := c.RemoveNotificationChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if rules, err := c.NotificationRules(ctx); err != nil || len(rules) != 0 {
		t.Errorf("%+v %v", rules, err)
	}
}

// TestNotificationAddressGuard refuses this machine, unless the server's configuration allows it.
func TestNotificationAddressGuard(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	ctx := context.Background()
	h := newHookReceiver(t)
	if _, err := c.AddNotificationChannel(ctx, apitypes.NewNotificationChannel{Name: "here", Kind: "slack", URL: h.URL}); err != nil {
		t.Fatal(err)
	}
	_, err := c.TestNotificationChannel(ctx, "here")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || !strings.Contains(err.Error(), "may not reach") {
		t.Fatalf("%v", err)
	}
	if len(h.got) != 0 {
		t.Fatal("it reached this machine")
	}
	channels, err := c.NotificationChannels(ctx)
	if err != nil || len(channels) != 1 || !strings.Contains(channels[0].Error, "may not reach") {
		t.Fatalf("%+v %v", channels, err)
	}
}

// TestNotificationPermissions: channels are for administrators, and a project's rules are for its
// maintainers, in the API and the pages.
func TestNotificationPermissions(t *testing.T) {
	cfg := testConfig(t)
	addrs, _ := start(t, cfg)
	c := apiClient(t, cfg, addrs)
	owner := signIn(t, cfg, addrs)
	ctx := context.Background()
	deploySite(t, c, "shop")
	deploySite(t, c, "other")
	if _, err := c.AddNotificationChannel(ctx, apitypes.NewNotificationChannel{Name: "ops", Kind: "slack", URL: "https://chat.example.com/services/SECRET"}); err != nil {
		t.Fatal(err)
	}

	// Two projects: Sam maintains one of them, and is nothing in the other.
	sam := addTeammate(t, cfg, addrs, owner, "sam@example.com")
	csrf := url.Values{"csrf": {owner.token()}}
	withCSRF := func(more url.Values) url.Values {
		for k, v := range csrf {
			more[k] = v
		}
		return more
	}
	if res, body := owner.post("/projects", withCSRF(url.Values{"name": {"web"}})); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if res, body := owner.post("/projects/web/members/set", withCSRF(url.Values{"email": {"sam@example.com"}, "role": {"maintainer"}})); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if res, body := owner.post("/projects/default/settings", withCSRF(url.Values{"everyone": {"none"}})); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	if err := testAuth(t, cfg).DB.MoveApp(ctx, "shop", "web"); err != nil {
		t.Fatal(err)
	}
	samClient := tokenClient(t, c, tokenOf(t, sam, "admin"))
	wantRefused := func(what string, err error, status int) {
		t.Helper()
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Errorf("%s: %v, want %d", what, err, status)
		}
	}

	// Sam sees a channel's name and kind, and nothing of where it goes.
	channels, err := samClient.NotificationChannels(ctx)
	if err != nil || len(channels) != 1 || channels[0].Name != "ops" || channels[0].Target != "" || channels[0].CreatedBy != "" {
		t.Fatalf("%+v %v", channels, err)
	}
	_, err = samClient.AddNotificationChannel(ctx, apitypes.NewNotificationChannel{Name: "mine", Kind: "slack", URL: "https://example.com/x"})
	wantRefused("adding a channel", err, http.StatusForbidden)
	_, err = samClient.TestNotificationChannel(ctx, "ops")
	wantRefused("testing a channel", err, http.StatusForbidden)
	wantRefused("removing a channel", samClient.RemoveNotificationChannel(ctx, "ops"), http.StatusForbidden)

	// Rules of their own project and its apps, and no others.
	events := []string{"deployment.failed"}
	ofProject, err := samClient.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", Project: "web", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	ofApp, err := samClient.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", App: "shop", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []apitypes.NewNotificationRule{{Channel: "ops", Events: events}, {Channel: "ops", Project: "default", Events: events}, {Channel: "ops", App: "other", Events: events}, {Channel: "ops", App: "nothing", Events: events}, {Channel: "ops", Project: "nothing", Events: events}} {
		_, err := samClient.AddNotificationRule(ctx, in)
		wantRefused(in.Project+"/"+in.App, err, http.StatusForbidden)
	}

	// What an admin set up elsewhere, they neither see nor remove.
	ofServer, err := c.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	ofOther, err := c.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", App: "other", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := samClient.NotificationRules(ctx)
	if err != nil || len(rules) != 2 {
		t.Fatalf("%+v %v", rules, err)
	}
	if all, err := c.NotificationRules(ctx); err != nil || len(all) != 4 {
		t.Fatalf("%+v %v", all, err)
	}
	wantRefused("removing a rule of the server", samClient.RemoveNotificationRule(ctx, ofServer.ID), http.StatusNotFound)
	wantRefused("removing a rule of another app", samClient.RemoveNotificationRule(ctx, ofOther.ID), http.StatusNotFound)

	// A token for one app of an administrator's is not the server's administrator.
	res, page := owner.post("/account/tokens/create", url.Values{"csrf": {owner.token()}, "name": {"for one app"}, "scope": {"admin"}, "app": {"shop"}})
	oneApp := madeToken.FindString(page)
	if res.StatusCode != http.StatusCreated || oneApp == "" {
		t.Fatalf("making a token: %d\n%s", res.StatusCode, page)
	}
	ownerOfShop := tokenClient(t, c, oneApp)
	if list, err := ownerOfShop.NotificationChannels(ctx); err != nil || len(list) != 1 || list[0].Target != "" {
		t.Errorf("%+v %v", list, err)
	}

	// A token's scope counts too.
	readOnly := tokenClient(t, c, tokenOf(t, sam, "read"))
	_, err = readOnly.AddNotificationRule(ctx, apitypes.NewNotificationRule{Channel: "ops", Project: "web", Events: events})
	wantRefused("adding a rule with a read token", err, http.StatusForbidden)

	// The pages: Settings is for administrators.
	if res, _ := sam.get("/settings/notifications"); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}
	samCSRF := url.Values{"csrf": {sam.token()}}
	if res, _ := sam.post("/settings/notifications/channels", samCSRF); res.StatusCode != http.StatusForbidden {
		t.Errorf("%d", res.StatusCode)
	}

	// The project's page lets Sam add and remove its own, and refuses anything else by address.
	res, page = sam.get("/projects/web")
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "All of web") || !strings.Contains(page, "The app shop") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := sam.post("/projects/web/notifications", url.Values{"csrf": {sam.token()}, "channel": {"ops"}, "app": {"other"}, "events": {"deployment.failed"}}); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a rule of another project's app: %d\n%s", res.StatusCode, page)
	}
	if res, _ := sam.post("/projects/web/notifications/remove", url.Values{"csrf": {sam.token()}, "rule": {ofServer.ID}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("removing the server's rule from a project's page: %d", res.StatusCode)
	}
	if res, _ := sam.post("/projects/web/notifications/remove", url.Values{"csrf": {sam.token()}, "rule": {ofOther.ID}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("removing another app's rule from a project's page: %d", res.StatusCode)
	}
	if res, body := sam.post("/projects/web/notifications/remove", url.Values{"csrf": {sam.token()}, "rule": {ofProject.ID}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	if _, body := sam.get("/projects/web?done=rule-removed"); !strings.Contains(body, "The rule is removed") || strings.Contains(body, "All of web") {
		t.Errorf("%s", body)
	}
	if res, body := sam.post("/projects/web/notifications", url.Values{"csrf": {sam.token()}, "channel": {"ops"}, "events": {"cron.failed"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d\n%s", res.StatusCode, body)
	}
	if _, body := sam.get("/projects/web"); !strings.Contains(body, "All of web") || !strings.Contains(body, "A cron job fails") {
		t.Errorf("%s", body)
	}
	if err := samClient.RemoveNotificationRule(ctx, ofApp.ID); err != nil {
		t.Error(err)
	}

	// What it takes in the pages to add a channel, and what is shown of it after.
	token := owner.token()
	res, page = owner.post("/settings/notifications/channels", url.Values{"csrf": {token}, "name": {"hook"}, "kind": {"webhook"}, "url": {"https://hooks.example.com/SECRET-PATH"}})
	if res.StatusCode != http.StatusCreated || !regexp.MustCompile(`[0-9a-f]{64}`).MatchString(page) || !strings.Contains(page, "hooks.example.com") || strings.Contains(page, "SECRET-PATH") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if _, page := owner.get("/settings/notifications"); regexp.MustCompile(`[0-9a-f]{64}`).MatchString(page) || strings.Contains(page, "SECRET") {
		t.Errorf("shown again:\n%s", page)
	}
	if res, page := owner.post("/settings/notifications/channels", url.Values{"csrf": {token}, "name": {"hook"}, "kind": {"slack"}, "url": {"https://example.com/x"}}); res.StatusCode != http.StatusConflict || !strings.Contains(page, "already a channel") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := owner.post("/settings/notifications/channels", url.Values{"csrf": {token}, "name": {"mail"}, "kind": {"email"}, "host": {"smtp.example.com"}, "from": {"a@example.com"}, "to": {"b@example.com, c@example.com"}, "password": {"not-shown"}, "username": {"sender"}}); res.StatusCode != http.StatusCreated || !strings.Contains(page, "b@example.com, c@example.com") || strings.Contains(page, "not-shown") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if res, page := owner.post("/settings/notifications/rules", url.Values{"csrf": {token}, "channel": {"hook"}, "scope": {"project:web"}, "events": {"deployment.failed", "server.disk_low"}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
	if rules, err := c.NotificationRules(ctx); err != nil || len(rules) != 4 {
		t.Errorf("%+v %v", rules, err)
	}
	if res, page := owner.post("/settings/notifications/rules", url.Values{"csrf": {token}, "channel": {"hook"}}); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "something to ask for") {
		t.Errorf("%d\n%s", res.StatusCode, page)
	}
}

// notifySignatureMatches works it out as a receiver would, from the secret and what it was sent.
func notifySignatureMatches(secret, stamp string, body []byte, signature string) bool {
	unix, err := strconv.ParseInt(stamp, 10, 64)
	return err == nil && notify.Sign(secret, time.Unix(unix, 0), body) == signature
}

func tokenClient(t *testing.T, c *client.Client, token string) *client.Client {
	t.Helper()
	out, err := client.New(c.Server(), token, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
