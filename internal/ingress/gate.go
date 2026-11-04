package ingress

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Gate modes.
const (
	GatePublic   = "public"
	GatePassword = "password"
	GateLogin    = "login"
	GateShare    = "share"
)

// How long being let in lasts. Those signed in are checked again every so often, so that losing
// their account or their role ends it.
const (
	passwordCookieLife = 7 * 24 * time.Hour
	loginCookieLife    = 12 * time.Hour
	shareCookieLife    = 7 * 24 * time.Hour
	stateCookieLife    = 10 * time.Minute
	defaultGateRecheck = time.Minute
	basicRemember      = 5 * time.Minute
	maxGateForm        = 64 << 10
)

// Gatekeeper is what the router needs to let people through a gate.
type Gatekeeper struct {
	Auth *auth.Service
	DB   *store.DB
	// Key signs gate cookies.
	Key []byte
	// Secure says that browsers reach apps over HTTPS.
	Secure bool
	// ConsoleURL is where people sign in.
	ConsoleURL string
	// Recheck is how often it looks again at whom it let in, a minute unless tests say.
	Recheck time.Duration

	// Failures are held against an address. Those of a password are also held against the
	// environment as a whole, which can take many more of them and for less long: what an
	// attacker can do with that is hold up others' passwords for a minute, and they cannot
	// do it with links or tokens, which are not there to be guessed.
	byClient, byEnv *auth.Limiter
	// Checking a password is slow on purpose. checking lets one at a time from each address,
	// and slots few at a time from anywhere, so that a burst is turned away, and not queued.
	checking sync.Map
	slots    chan struct{}

	checks sync.Map // what a cookie's subject was found to be, by env and subject
	basic  sync.Map // passwords that were right lately, by keyed hash
}

// SetGatekeeper turns on the gates. Until then an environment with one lets nobody in.
func (rt *Router) SetGatekeeper(g *Gatekeeper) {
	g.byClient, g.byEnv = auth.NewLimiter(), auth.NewLimiterFor(100, time.Minute)
	g.slots = make(chan struct{}, 2)
	if g.Recheck == 0 {
		g.Recheck = defaultGateRecheck
	}
	rt.gate.Store(g)
}

// gateRoute is who may reach a route.
type gateRoute struct {
	mode       string
	envID      string
	generation int
	password   string
	allow      []netip.Prefix
	// allowBroken says the list of addresses could not be read: nobody is let in by it.
	allowBroken bool
	anyUser     bool
}

// gateOf works out who may reach an environment.
func (rt *Router) gateOf(r store.Route, plan manifest.Plan) gateRoute {
	g := gateRoute{envID: r.EnvID, generation: r.Access.Generation, password: r.Access.PasswordHash, anyUser: r.Access.AnyUser}
	g.mode, _ = rt.cfg.GateFor(r.Access.Mode, plan.AccessFor(r.Env, r.Kind), r.Kind)
	for _, cidr := range r.Access.Allow {
		prefix, err := ParseAllow(cidr)
		if err != nil {
			g.allowBroken = true
			continue
		}
		g.allow = append(g.allow, prefix)
	}
	return g
}

// ParseAllow reads an address or a prefix of an allow-list.
func ParseAllow(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, errors.New("not an address or a CIDR prefix")
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// guarded says whether a route turns anyone away, so that what it serves is not for everyone.
func (g gateRoute) guarded() bool {
	return g.mode != GatePublic || len(g.allow) > 0 || g.allowBroken
}

// allowed says whether a client's address is on a route's list, if it has one.
func (g gateRoute) allowed(client string) bool {
	if g.allowBroken {
		return false
	}
	if len(g.allow) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(client)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range g.allow {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientAddress is who asked: the address it came from, or the nearest that is not a proxy we
// trust. What a client said before that is its own affair, and proves nothing.
func (rt *Router) clientAddress(r *http.Request) string {
	return auth.ClientIP(r, rt.trusted)
}

// limitKey is an address, as far as it can be held to account: a whole IPv6 /64 is one customer's.
func limitKey(client string) string {
	addr, err := netip.ParseAddr(client)
	if err != nil {
		return client
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// identity is who a gate let in, and how, which the app hears of.
type identity struct {
	user store.User
	// kind and subject are what let them in; kind is empty for a token or a password from a script.
	kind, subject string
	// bearer is the token that let them in, and client from where, to look at it again.
	bearer, client string
	// consumed says that the request's Authorization header was for the gate, not the app.
	consumed bool
}

type identityKey struct{}

// cookieName is a gate's cookie's name, of its own environment's, since several may share a
// hostname. With HTTPS, its prefix makes browsers refuse one that another hostname tries to set.
func cookieName(secure bool, base, envID string) string {
	name := base + "_" + envID
	if secure {
		return "__Host-" + name
	}
	return name
}

const (
	gateCookieBase  = "moorage_gate"
	stateCookieBase = "moorage_gatestate"
)

func (g *Gatekeeper) sign(payload string) string {
	mac := hmac.New(sha256.New, g.Key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// cookieValue makes a gate cookie: what it lets in, signed. Its subject is what it was let in as:
// nothing for a password, a user's ID, or a share link's ID.
func (g *Gatekeeper) cookieValue(gr gateRoute, kind, subject string, expires time.Time) string {
	payload := strings.Join([]string{"v1", gr.envID, strconv.Itoa(gr.generation), strconv.FormatInt(expires.Unix(), 10), kind, subject}, "|")
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + g.sign(payload)
}

// readCookie returns what a gate cookie says, if it is ours, for this environment as it now
// stands, and has not run out. A password cookie is good only while the gate is a password,
// and a sign-in cookie while it is a sign-in; share links work with every gate.
func (g *Gatekeeper) readCookie(gr gateRoute, value string, now time.Time) (kind, subject string, ok bool) {
	encoded, signature, found := strings.Cut(value, ".")
	if !found {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	payload := string(raw)
	if !hmac.Equal([]byte(g.sign(payload)), []byte(signature)) {
		return "", "", false
	}
	parts := strings.Split(payload, "|")
	if len(parts) != 6 || parts[0] != "v1" || parts[1] != gr.envID || parts[2] != strconv.Itoa(gr.generation) {
		return "", "", false
	}
	expires, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || now.Unix() >= expires {
		return "", "", false
	}
	kind, subject = parts[4], parts[5]
	switch kind {
	case GateShare:
		return kind, subject, true
	case GatePassword:
		return kind, subject, gr.mode == GatePassword
	case GateLogin:
		return kind, subject, gr.mode == GateLogin
	}
	return "", "", false
}

func (g *Gatekeeper) setCookie(w http.ResponseWriter, gr gateRoute, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: cookieName(g.Secure, gateCookieBase, gr.envID), Value: value, Path: "/", Expires: expires,
		HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteLaxMode})
}

// recheck is what was last found about a cookie's subject.
type recheck struct {
	ok    bool
	user  store.User
	found time.Time
}

// checkLogin says whether a user may still be let in, looking again every so often: at their
// account, their role, and if a console session let them in, whether that has ended.
func (g *Gatekeeper) checkLogin(ctx context.Context, gr gateRoute, app, userID, sessionID string) (store.User, bool) {
	key := gr.envID + "/" + strconv.Itoa(gr.generation) + "/login/" + userID + "/" + sessionID
	if v, ok := g.checks.Load(key); ok {
		if c := v.(recheck); time.Since(c.found) < g.Recheck {
			return c.user, c.ok
		}
	}
	u, ok, err := g.Auth.GateUser(ctx, userID, app, gr.anyUser)
	if err != nil {
		return u, false
	}
	if ok && sessionID != "" {
		if ok, err = g.Auth.SessionAlive(ctx, userID, sessionID); err != nil {
			return u, false
		}
	}
	g.checks.Store(key, recheck{ok, u, time.Now()})
	return u, ok
}

// checkShare says whether a share link that let someone in has not since ended.
func (g *Gatekeeper) checkShare(ctx context.Context, gr gateRoute, id string) bool {
	key := gr.envID + "/share/" + id
	if v, ok := g.checks.Load(key); ok {
		if c := v.(recheck); time.Since(c.found) < g.Recheck {
			return c.ok
		}
	}
	l, err := g.DB.ShareLink(ctx, id)
	if err != nil {
		return false
	}
	ok := l.EnvID == gr.envID && l.Active(time.Now())
	g.checks.Store(key, recheck{ok: ok, found: time.Now()})
	return ok
}

// fromCookie says who a request's gate cookie lets in, if anyone.
func (g *Gatekeeper) fromCookie(r *http.Request, rte route) *identity {
	gr := rte.gate
	c, err := r.Cookie(cookieName(g.Secure, gateCookieBase, gr.envID))
	if err != nil {
		return nil
	}
	kind, subject, ok := g.readCookie(gr, c.Value, time.Now())
	if !ok {
		return nil
	}
	who := &identity{kind: kind, subject: subject, consumed: gr.mode == GatePassword}
	if !g.stillGood(r.Context(), rte, who) {
		return nil
	}
	return who
}

// stillGood says whether what let someone in has not since ended.
func (g *Gatekeeper) stillGood(ctx context.Context, rte route, who *identity) bool {
	if who.bearer != "" {
		p, err := g.Auth.Token(ctx, who.bearer, who.client)
		if err != nil || !p.Can(auth.ScopeGate) || p.UserID == "" || p.App != "" && p.App != rte.app {
			return false
		}
		_, ok := g.checkLogin(ctx, rte.gate, rte.app, p.UserID, "")
		return ok
	}
	switch who.kind {
	case GatePassword:
		return true
	case GateShare:
		return g.checkShare(ctx, rte.gate, who.subject)
	case GateLogin:
		userID, sessionID, _ := strings.Cut(who.subject, "/")
		u, ok := g.checkLogin(ctx, rte.gate, rte.app, userID, sessionID)
		who.user = u
		return ok
	}
	return false
}

// admit decides whether a request gets through an environment's gate. If not, it has answered.
func (rt *Router) admit(w http.ResponseWriter, r *http.Request, rte route) (*identity, bool) {
	gr := rte.gate
	if !gr.allowed(rt.clientAddress(r)) {
		accessDenied(rte.app).ServeHTTP(w, r)
		return nil, false
	}
	if gr.mode == GatePublic {
		return nil, true
	}
	g := rt.gate.Load()
	if g == nil {
		accessDenied(rte.app).ServeHTTP(w, r)
		return nil, false
	}
	if who := g.fromCookie(r, rte); who != nil {
		return who, true
	}
	switch gr.mode {
	case GatePassword:
		if password, ok := basicPassword(r); ok {
			if rt.basicAccepted(w, r, g, gr, password) {
				return &identity{consumed: true}, true
			}
			return nil, false
		}
		if wantsPage(r) {
			passwordPage(rte, r.URL.RequestURI(), "").ServeHTTP(w, r)
			return nil, false
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="`+rte.app+`"`)
		http.Error(w, rte.app+" asks for a password\n", http.StatusUnauthorized)
	case GateLogin:
		if id, handled := rt.bearerAccepted(w, r, g, gr, rte); handled {
			return id, id != nil
		}
		if wantsPage(r) {
			rt.toConsole(w, r, g, rte)
			return nil, false
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+rte.app+`"`)
		http.Error(w, rte.app+" asks you to sign in: an API token with the gate scope will do\n", http.StatusUnauthorized)
	default: // GateShare, and whatever else would be a surprise
		if wantsPage(r) {
			shareNeeded(rte.app).ServeHTTP(w, r)
			return nil, false
		}
		http.Error(w, rte.app+" can only be reached with a share link\n", http.StatusUnauthorized)
	}
	return nil, false
}

// stillAdmitted looks again at someone who was let in, for what goes on for a long time, such as a
// stream or a WebSocket: the gate as it now stands, and whatever let them in.
func (rt *Router) stillAdmitted(ctx context.Context, r *http.Request, rte route, who *identity) bool {
	now, _, ok := rt.find(rt.table.Load(), hostname(r.Host), r.URL.Path)
	if !ok || now.gate.envID != rte.gate.envID || now.gate.generation != rte.gate.generation || now.gate.mode != rte.gate.mode || !now.gate.allowed(rt.clientAddress(r)) {
		return false
	}
	if now.gate.mode == GatePublic || who == nil || who.kind == "" && who.bearer == "" {
		return true
	}
	g := rt.gate.Load()
	return g != nil && g.stillGood(ctx, now, who)
}

// watchAdmission ends a request that goes on after its way in has ended. It takes nothing from a
// request that is over before it first looks, which is nearly all of them.
func (rt *Router) watchAdmission(ctx context.Context, cancel context.CancelFunc, r *http.Request, rte route, who *identity) (stop func()) {
	every := defaultGateRecheck
	if g := rt.gate.Load(); g != nil {
		every = g.Recheck
	}
	var mu sync.Mutex
	stopped := false
	var timer *time.Timer
	timer = time.AfterFunc(every, func() {
		if !rt.stillAdmitted(ctx, r, rte, who) {
			cancel()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !stopped {
			timer.Reset(every)
		}
	})
	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		timer.Stop()
	}
}

// basicPassword reads a password from HTTP Basic, for clients that are not browsers.
func basicPassword(r *http.Request) (string, bool) {
	_, password, ok := r.BasicAuth()
	return password, ok
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
	http.Error(w, "Too many attempts: try again later\n", http.StatusTooManyRequests)
}

// limitedClient says whether an address has failed too often to try again, and if so answers.
func (g *Gatekeeper) limitedClient(w http.ResponseWriter, envID, client string) bool {
	if wait := g.byClient.Wait(envID + "/" + limitKey(client)); wait > 0 {
		tooMany(w, wait)
		return true
	}
	return false
}

// passwordRight says whether a password is the gate's, remembering for a while one that was, since
// checking is slow on purpose, and a script using HTTP Basic does it with every request. If there
// have been too many attempts, or too many at once, it has answered.
func (g *Gatekeeper) passwordRight(w http.ResponseWriter, r *http.Request, gr gateRoute, client, password string) (right, answered bool) {
	if gr.password == "" {
		return false, false
	}
	key := g.sign("basic|" + gr.envID + "|" + strconv.Itoa(gr.generation) + "|" + password)
	if at, ok := g.basic.Load(key); ok && time.Since(at.(time.Time)) < basicRemember {
		return true, false
	}
	clientKey := gr.envID + "/" + limitKey(client)
	if wait := max(g.byClient.Wait(clientKey), g.byEnv.Wait(gr.envID)); wait > 0 {
		tooMany(w, wait)
		return false, true
	}
	// One at a time from an address, and few at a time from anywhere: a failure only counts once it
	// has been found, and a burst must not all be checked before the first has been.
	if _, busy := g.checking.LoadOrStore(clientKey, true); busy {
		tooMany(w, time.Second)
		return false, true
	}
	defer g.checking.Delete(clientKey)
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		tooMany(w, time.Second)
		return false, true
	}
	ok, err := auth.VerifyPassword(r.Context(), gr.password, password)
	if err != nil || !ok {
		g.byClient.Fail(clientKey)
		g.byEnv.Fail(gr.envID)
		return false, false
	}
	g.byClient.Clear(clientKey)
	g.basic.Store(key, time.Now())
	return true, false
}

func (rt *Router) basicAccepted(w http.ResponseWriter, r *http.Request, g *Gatekeeper, gr gateRoute, password string) bool {
	right, answered := g.passwordRight(w, r, gr, rt.clientAddress(r), password)
	if right || answered {
		return right
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="password"`)
	http.Error(w, "That is not the password\n", http.StatusUnauthorized)
	return false
}

// bearerAccepted lets in a request with an API token of the gate's scope, for scripts. A header
// that is not one of ours is left for the app, and does not get anyone through.
func (rt *Router) bearerAccepted(w http.ResponseWriter, r *http.Request, g *Gatekeeper, gr gateRoute, rte route) (*identity, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(token, "moor_") {
		return nil, false
	}
	client := rt.clientAddress(r)
	if g.limitedClient(w, gr.envID, client) {
		return nil, true
	}
	refuse := func() (*identity, bool) {
		g.byClient.Fail(gr.envID + "/" + limitKey(client))
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+rte.app+`", error="invalid_token"`)
		http.Error(w, "That token does not reach this app\n", http.StatusUnauthorized)
		return nil, true
	}
	p, err := g.Auth.Token(r.Context(), token, client)
	if err != nil || !p.Can(auth.ScopeGate) || p.UserID == "" || p.App != "" && p.App != rte.app {
		return refuse()
	}
	u, ok := g.checkLogin(r.Context(), gr, rte.app, p.UserID, "")
	if !ok {
		return refuse()
	}
	return &identity{user: u, consumed: true, bearer: token, client: client}, true
}

// toConsole sends a browser to sign in, with what will show that it is the same browser that
// comes back: otherwise someone could have a visitor signed in as them.
func (rt *Router) toConsole(w http.ResponseWriter, r *http.Request, g *Gatekeeper, rte route) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "The server could not continue\n", http.StatusInternalServerError)
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{Name: cookieName(g.Secure, stateCookieBase, rte.gate.envID), Value: state, Path: "/", MaxAge: int(stateCookieLife.Seconds()),
		HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteLaxMode})
	q := url.Values{"host": {hostname(r.Host)}, "return": {safeReturn(rte.prefix, r.URL.RequestURI())}, "state": {state}}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	http.Redirect(w, r, g.ConsoleURL+"/auth/gate?"+q.Encode(), http.StatusFound)
}

// SafeLocalPath says whether a browser would stay on this site, going to a path: not with a second
// slash or a backslash, which it takes as another site, and not with anything it takes away
// before it looks, such as a tab.
func SafeLocalPath(s string) bool {
	if len(s) > 2048 || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) < 0
}

// safeReturn keeps a place to go back to, within what a route serves.
func safeReturn(prefix, s string) string {
	home := prefix + "/"
	if !SafeLocalPath(s) || prefix != "" && !strings.HasPrefix(s, home) {
		return home
	}
	if rest := strings.TrimPrefix(s, prefix); strings.HasPrefix(rest, "/.moorage/") {
		return home
	}
	return s
}

// RouteInfo is what the console needs to know of what a hostname and a path serve.
type RouteInfo struct {
	App, Env string
	AnyUser  bool
	// Target is what a one-time code is for, and Base where its answer goes.
	Target, Base string
}

// RouteFor says what a hostname and a path serve. Only what is in the routing table has an answer,
// so that nothing the console is told by a browser can send anyone anywhere else.
func (rt *Router) RouteFor(host, path string) (RouteInfo, bool) {
	rte, _, ok := rt.find(rt.table.Load(), host, path)
	if !ok {
		return RouteInfo{}, false
	}
	return RouteInfo{App: rte.app, Env: rte.env, AnyUser: rte.gate.anyUser, Target: host + rte.prefix, Base: rt.cfg.PublicURL(host) + rte.prefix}, true
}

// gateEndpoint answers what the gates themselves ask a browser to do. path is what comes after
// what the route serves.
func (rt *Router) gateEndpoint(w http.ResponseWriter, r *http.Request, rte route, path string) {
	g := rt.gate.Load()
	if g == nil {
		notFound.ServeHTTP(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer") // what is in these addresses is not for anyone else
	h.Set("X-Robots-Tag", "noindex")
	if !rte.gate.allowed(rt.clientAddress(r)) {
		accessDenied(rte.app).ServeHTTP(w, r)
		return
	}
	switch {
	case path == "/.moorage/gate/password":
		rt.passwordForm(w, r, g, rte)
	case path == "/.moorage/auth/callback":
		rt.signedIn(w, r, g, rte)
	case strings.HasPrefix(path, "/.moorage/share/"):
		rt.redeemShare(w, r, g, rte, strings.TrimPrefix(path, "/.moorage/share/"))
	default:
		notFound.ServeHTTP(w, r)
	}
}

// sameOrigin says that a form came from this hostname, if the browser says where it came from.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (rt *Router) passwordForm(w http.ResponseWriter, r *http.Request, g *Gatekeeper, rte route) {
	gr := rte.gate
	if gr.mode != GatePassword {
		notFound.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed\n", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		accessDenied(rte.app).ServeHTTP(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxGateForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read\n", http.StatusBadRequest)
		return
	}
	back := safeReturn(rte.prefix, r.PostFormValue("return"))
	right, answered := g.passwordRight(w, r, gr, rt.clientAddress(r), r.PostFormValue("password"))
	if answered {
		return
	}
	if !right {
		passwordPage(rte, back, "That is not the password.").ServeHTTP(w, r)
		return
	}
	expires := time.Now().Add(passwordCookieLife)
	g.setCookie(w, gr, g.cookieValue(gr, GatePassword, "", expires), expires)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// signedIn is where the console sends someone who has signed in, with a one-time code for this
// place, which turns into a cookie of this hostname's own.
func (rt *Router) signedIn(w http.ResponseWriter, r *http.Request, g *Gatekeeper, rte route) {
	gr := rte.gate
	q := r.URL.Query()
	back := safeReturn(rte.prefix, q.Get("return"))
	stateName := cookieName(g.Secure, stateCookieBase, gr.envID)
	state, err := r.Cookie(stateName)
	if err != nil || state.Value == "" || subtle.ConstantTimeCompare([]byte(state.Value), []byte(q.Get("state"))) != 1 {
		// Another tab may have started again. Going back starts this one again, if it needs to.
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: g.Secure, SameSite: http.SameSiteLaxMode})
	userID, sessionID, ok := g.Auth.RedeemGateCode(q.Get("code"), hostname(r.Host)+rte.prefix)
	if !ok || gr.mode != GateLogin {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if _, ok := g.checkLogin(r.Context(), gr, rte.app, userID, sessionID); !ok {
		accessDenied(rte.app).ServeHTTP(w, r)
		return
	}
	expires := time.Now().Add(loginCookieLife)
	g.setCookie(w, gr, g.cookieValue(gr, GateLogin, userID+"/"+sessionID, expires), expires)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// ShareTokenHash is how a share link's token is kept.
func ShareTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// redeemShare turns a share link into a cookie of this hostname, and goes on to the clean address.
// Anything but someone going to it asks first, and so does a link with a limited number of uses:
// what fetches addresses to show a preview of them, as chat programs do, should not use them up,
// nor should an image on another page change who someone is here. Someone already in stays who
// they are.
func (rt *Router) redeemShare(w http.ResponseWriter, r *http.Request, g *Gatekeeper, rte route, token string) {
	gr := rte.gate
	home := rte.prefix + "/"
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed\n", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost && !sameOrigin(r) {
		accessDenied(rte.app).ServeHTTP(w, r)
		return
	}
	client := rt.clientAddress(r)
	if g.limitedClient(w, gr.envID, client) {
		return
	}
	l, err := g.DB.ShareLinkByToken(r.Context(), ShareTokenHash(token))
	now := time.Now()
	if err != nil || l.EnvID != gr.envID {
		g.byClient.Fail(gr.envID + "/" + limitKey(client))
		shareEnded(rte.app).ServeHTTP(w, r)
		return
	}
	if !l.Active(now) || l.MaxUses > 0 && l.Uses >= l.MaxUses {
		shareEnded(rte.app).ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if gr.mode == GatePublic || g.fromCookie(r, rte) != nil {
		http.Redirect(w, r, home, http.StatusSeeOther)
		return
	}
	dest := r.Header.Get("Sec-Fetch-Dest")
	if r.Method != http.MethodPost && (l.MaxUses > 0 || dest != "" && dest != "document") {
		shareConfirm(rte.app, r.URL.Path).ServeHTTP(w, r)
		return
	}
	if used, err := g.DB.UseShareLink(r.Context(), l.ID, now); err != nil || !used {
		shareEnded(rte.app).ServeHTTP(w, r)
		return
	}
	expires := now.Add(shareCookieLife)
	if l.ExpiresAt.Before(expires) {
		expires = l.ExpiresAt
	}
	g.setCookie(w, gr, g.cookieValue(gr, GateShare, l.ID, expires), expires)
	http.Redirect(w, r, home, http.StatusSeeOther)
}

// identityHeaders tells an app who a gate let in, and takes away what is not for it: its gate's
// cookies, a Moorage token, which would let it act as whoever sent it, and a header meant for the gate.
// It is not told anything unless the request comes through here, which the proxy token shows, for
// apps that do not trust what else can reach them on this machine.
func identityHeaders(out http.Header, in *http.Request) {
	removeGateCookies(out)
	if strings.HasPrefix(out.Get("Authorization"), "Bearer moor_") {
		out.Del("Authorization")
	}
	if info, ok := in.Context().Value(infoKey{}).(*requestInfo); ok {
		if info.prefix != "" {
			out.Set("X-Forwarded-Prefix", info.prefix)
		}
		if info.proxyToken != "" {
			out.Set("X-Moorage-Proxy-Token", info.proxyToken)
		}
	}
	id, _ := in.Context().Value(identityKey{}).(*identity)
	if id == nil {
		return
	}
	if id.consumed {
		out.Del("Authorization")
	}
	if id.user.ID != "" {
		out.Set("X-Moorage-User", id.user.ID)
		out.Set("X-Moorage-User-Email", id.user.Email)
		out.Set("X-Moorage-User-Name", headerSafe(id.user.Name))
	}
}

// headerSafe leaves out of a value what would not be safe in a header.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func isGateCookie(name string) bool {
	name = strings.TrimPrefix(name, "__Host-")
	return strings.HasPrefix(name, gateCookieBase)
}

// removeGateCookies keeps a gate's cookies from the app behind it, and leaves the rest as they were.
func removeGateCookies(h http.Header) {
	lines := h.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	found := false
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		var parts []string
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if isGateCookie(name) {
				found = true
				continue
			}
			parts = append(parts, strings.TrimSpace(part))
		}
		if len(parts) > 0 {
			kept = append(kept, strings.Join(parts, "; "))
		}
	}
	if !found {
		return
	}
	h.Del("Cookie")
	for _, line := range kept {
		h.Add("Cookie", line)
	}
}

// rewriteForPrefix makes what an app says of paths fit the path it is served at: its redirects and
// its cookies. What is in the bodies of responses is its own affair.
func rewriteForPrefix(h http.Header, prefix string) {
	if loc := h.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && loc != prefix && !strings.HasPrefix(loc, prefix+"/") {
		h.Set("Location", prefix+loc)
	}
	cookies := h.Values("Set-Cookie")
	if len(cookies) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, c := range cookies {
		h.Add("Set-Cookie", scopeCookie(c, prefix))
	}
}

// scopeCookie puts a cookie's path under a prefix. A cookie for a __Host- name must stay at /.
func scopeCookie(cookie, prefix string) string {
	if strings.HasPrefix(cookie, "__Host-") {
		return cookie
	}
	parts := strings.Split(cookie, ";")
	found := false
	for i, part := range parts {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if i == 0 || !ok || !strings.EqualFold(name, "path") {
			continue
		}
		found = true
		if !strings.HasPrefix(value, prefix+"/") && value != prefix {
			parts[i] = " Path=" + prefix + value
		}
	}
	if !found {
		parts = append(parts, " Path="+prefix+"/")
	}
	return strings.Join(parts, ";")
}
