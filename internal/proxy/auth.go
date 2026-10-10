package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	pathpkg "path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/wes/jokku/internal/httpauth"
	"github.com/wes/jokku/internal/types"
)

func init() {
	caddy.RegisterModule(AuthHandler{})
	caddy.RegisterModule(StripIdentity{})
}

// AuthHandler puts a login in front of an app (http-auth), or, as the
// portal, serves the login domain: one login page for every app whose
// login is the cluster's users, which hands each app's domain a session of
// its own.
//
// Everything it needs is in its config and the session key, so any ingress
// node, an edge included, checks logins on its own. Its pages live under
// /.jokku/ on the app's domains:
//
//	/.jokku/login          the login form (users get a second step for a TOTP code)
//	/.jokku/callback       where the portal hands a login to an app's domain
//	/.jokku/logout         ends the session
//	/.jokku/share/<token>  a share link: lets its holder in until it expires
//
// Requests that pass get X-Jokku-User (the user's name, for users logins)
// and lose the session cookie on the way to the app.
type AuthHandler struct {
	App    string           `json:"app,omitempty"`
	Auth   *types.RouteAuth `json:"auth,omitempty"` // nil for the portal
	Global *types.ProxyAuth `json:"global"`
	// PortalURL is the login domain, https://auth.example.com; empty
	// without one.
	PortalURL string `json:"portal_url,omitempty"`
	// Portal makes this handler the login domain's. Hosts are then the
	// domains it may log in to.
	Portal bool                  `json:"portal,omitempty"`
	Hosts  map[string]PortalHost `json:"hosts,omitempty"`

	key   []byte
	users map[string]types.ProxyUser
	allow []netip.Prefix
}

// PortalHost is a domain the portal logs in to: who may, and whether it is
// served over HTTPS (its login is then only ever handed over HTTPS).
type PortalHost struct {
	Users []string `json:"users"` // empty: every user
	TLS   bool     `json:"tls,omitempty"`
}

func (AuthHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.jokku_auth", New: func() caddy.Module { return new(AuthHandler) }}
}

func (h *AuthHandler) Provision(caddy.Context) error {
	if h.Global == nil {
		return fmt.Errorf("jokku_auth: no settings")
	}
	key, err := httpauth.Key(h.Global.Key)
	if err != nil {
		return fmt.Errorf("jokku_auth: %w", err)
	}
	h.key = key
	h.users = map[string]types.ProxyUser{}
	for _, u := range h.Global.Users {
		h.users[u.Name] = u
	}
	if h.Auth != nil {
		for _, a := range h.Auth.AllowIPs {
			if p, err := netip.ParsePrefix(a); err == nil {
				h.allow = append(h.allow, p)
			}
		}
	}
	return nil
}

const (
	sessionCookie = "jokku_session"
	portalCookie  = "jokku_sso"
	userHeader    = "X-Jokku-User"
	prefix        = "/.jokku/"

	purposeSession = "session" // an app domain's cookie
	purposePortal  = "portal"  // the login domain's cookie
	purposeHandoff = "handoff" // a login handed from the portal to an app domain
	purposePending = "pending" // a password checked, waiting for its TOTP code
)

func (h *AuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	stripIdentity(r.Header) // only this handler says who someone is
	if h.Portal {
		return h.servePortal(w, r)
	}
	switch p := r.URL.Path; {
	case p == prefix+"login":
		return h.login(w, r)
	case p == prefix+"callback":
		return h.callback(w, r)
	case p == prefix+"logout":
		return h.logout(w, r)
	case strings.HasPrefix(p, prefix+"share/"):
		return h.share(w, r, strings.TrimPrefix(p, prefix+"share/"))
	}
	if h.allowedIP(r) || h.bypassed(r.URL) {
		stripCookie(r, sessionCookie)
		return next.ServeHTTP(w, r)
	}
	if s, ok := h.session(r); ok {
		if s.Kind == httpauth.KindUser {
			r.Header.Set(userHeader, s.Name)
		}
		stripCookie(r, sessionCookie)
		return next.ServeHTTP(w, r)
	}
	// Browsers go to the login page; anything else (an API client, a
	// form post) gets a plain 401.
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && acceptsHTML(r) {
		http.Redirect(w, r, h.loginURL(r, r.URL.RequestURI()), http.StatusFound)
		return nil
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, "Log in at %s\n", h.loginURL(r, "/"))
	return nil
}

// loginURL is where to log in to come back to path: the portal for users
// logins when there is one, this domain's login page otherwise.
func (h *AuthHandler) loginURL(r *http.Request, path string) string {
	if h.Auth.Mode == types.AuthUsers && h.PortalURL != "" {
		back := scheme(r) + "://" + r.Host + path
		return h.PortalURL + prefix + "login?" + url.Values{"rd": {back}}.Encode()
	}
	return prefix + "login?" + url.Values{"rd": {path}}.Encode()
}

func (h *AuthHandler) allowedIP(r *http.Request) bool {
	if len(h.allow) == 0 {
		return false
	}
	ip, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range h.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// bypassed says whether a path skips the login. Only a clean path can: one
// with .. or // in it could match /api/webhook/* and still mean something
// else to the app, such as /api/webhook/../admin.
func (h *AuthHandler) bypassed(u *url.URL) bool {
	path := u.Path
	if len(h.Auth.BypassPaths) == 0 || !clean(path, u.EscapedPath()) {
		return false
	}
	for _, b := range h.Auth.BypassPaths {
		if pre, ok := strings.CutSuffix(b, "*"); ok {
			if strings.HasPrefix(path, pre) {
				return true
			}
		} else if path == b {
			return true
		}
	}
	return false
}

// session returns the valid session this request carries, if any. It is
// checked against the settings now, so a changed password, a removed user
// or a revoked share ends it at once.
func (h *AuthHandler) session(r *http.Request) (httpauth.Session, bool) {
	var s httpauth.Session
	c, err := r.Cookie(sessionCookie)
	if err != nil || !httpauth.Verify(h.key, purposeSession, hostOnly(r.Host), c.Value, &s) || time.Now().Unix() >= s.Expires {
		return s, false
	}
	switch s.Kind {
	case httpauth.KindPassword:
		return s, h.Auth.Mode == types.AuthPassword && s.App == h.App && s.Version == httpauth.Fingerprint(h.Auth.PasswordHash)
	case httpauth.KindUser:
		u, ok := h.users[s.Name]
		return s, ok && h.Auth.Mode == types.AuthUsers && h.mayEnter(u.Name) && s.Version == userVersion(u)
	case httpauth.KindShare:
		return s, s.App == h.App && h.shareByID(s.Name) != nil
	}
	return s, false
}

func (h *AuthHandler) mayEnter(user string) bool {
	return len(h.Auth.Users) == 0 || slices.Contains(h.Auth.Users, user)
}

func userVersion(u types.ProxyUser) string { return httpauth.Fingerprint(u.PasswordHash, u.TOTPSecret) }

func (h *AuthHandler) shareByID(id string) *types.ProxyShare {
	for i, s := range h.Auth.Shares {
		if s.ID == id && time.Now().Before(s.ExpiresAt) {
			return &h.Auth.Shares[i]
		}
	}
	return nil
}

func (h *AuthHandler) setSession(w http.ResponseWriter, r *http.Request, s httpauth.Session) error {
	v, err := httpauth.Sign(h.key, purposeSession, hostOnly(r.Host), s)
	if err != nil {
		return err
	}
	setCookie(w, r, sessionCookie, v, time.Unix(s.Expires, 0))
	return nil
}

func (h *AuthHandler) sessionEnd() int64 {
	return time.Now().Add(time.Duration(h.Global.SessionDays) * 24 * time.Hour).Unix()
}

// login shows the form, and checks what it sends: the app's password, or a
// user's password and then their TOTP code.
func (h *AuthHandler) login(w http.ResponseWriter, r *http.Request) error {
	rd := localPath(r.FormValue("rd"))
	if h.Auth.Mode == types.AuthUsers && h.PortalURL != "" {
		http.Redirect(w, r, h.loginURL(r, rd), http.StatusFound)
		return nil
	}
	pg := page{Title: "Log in", App: h.App, Action: prefix + "login", RD: rd, Users: h.Auth.Mode == types.AuthUsers}
	if r.Method != http.MethodPost {
		return pg.render(w, http.StatusOK)
	}
	if h.Auth.Mode == types.AuthPassword {
		keys := []string{clientKey(r), appKey(h.App)}
		if wait := limits.wait(keys...); wait > 0 {
			pg.Error = tooMany(wait)
			return pg.render(w, http.StatusTooManyRequests)
		}
		if !httpauth.CheckPassword(h.Auth.PasswordHash, r.PostFormValue("password")) {
			limits.failed(keys...)
			pg.Error = "That password isn't right."
			return pg.render(w, http.StatusUnauthorized)
		}
		err := h.setSession(w, r, httpauth.Session{Kind: httpauth.KindPassword, App: h.App,
			Version: httpauth.Fingerprint(h.Auth.PasswordHash), Expires: h.sessionEnd()})
		if err != nil {
			return err
		}
		http.Redirect(w, r, rd, http.StatusSeeOther)
		return nil
	}
	u, err := h.checkUser(w, r, &pg, h.App)
	if u == nil || err != nil {
		return err
	}
	if !h.mayEnter(u.Name) {
		pg.Error = fmt.Sprintf("%s may not use %s.", u.Name, h.App)
		return pg.render(w, http.StatusForbidden)
	}
	if err := h.setSession(w, r, httpauth.Session{Kind: httpauth.KindUser, Name: u.Name, Version: userVersion(*u), Expires: h.sessionEnd()}); err != nil {
		return err
	}
	http.Redirect(w, r, rd, http.StatusSeeOther)
	return nil
}

// pending is a password that checked out, waiting for the TOTP code.
type pending struct {
	User    string `json:"u"`
	Version string `json:"v"`
	Expires int64  `json:"e"`
}

// checkUser checks a users login form, either step, for app (or the
// portal). It returns the user once both steps pass; otherwise it renders
// the next form (or the error) and returns nil. Guesses are limited per
// client, per app and per user (see limits): a code is guessed no faster
// than a password.
func (h *AuthHandler) checkUser(w http.ResponseWriter, r *http.Request, pg *page, app string) (*types.ProxyUser, error) {
	host := hostOnly(r.Host)
	tok := r.PostFormValue("pending")
	var p pending
	if tok != "" {
		if !httpauth.Verify(h.key, purposePending, host, tok, &p) || time.Now().Unix() >= p.Expires {
			pg.Error = "That took too long; log in again."
			return nil, pg.render(w, http.StatusUnauthorized)
		}
		pg.User = p.User
	} else {
		pg.User = strings.ToLower(strings.TrimSpace(r.PostFormValue("user")))
	}
	keys := []string{clientKey(r), appKey(app), userKey(pg.User)}
	if wait := limits.wait(keys...); wait > 0 {
		pg.Error = tooMany(wait)
		return nil, pg.render(w, http.StatusTooManyRequests)
	}
	u, ok := h.users[pg.User]
	if tok != "" {
		if !ok || userVersion(u) != p.Version {
			pg.Error = "Log in again."
			return nil, pg.render(w, http.StatusUnauthorized)
		}
		step, ok := httpauth.CheckTOTP(u.TOTPSecret, r.PostFormValue("code"), time.Now())
		if !ok || !totpSteps.use(u.Name, step) {
			limits.failed(keys...)
			pg.Error, pg.Code, pg.Pending = "That code isn't right.", true, tok
			return nil, pg.render(w, http.StatusUnauthorized)
		}
		limits.reset(userKey(u.Name))
		return &u, nil
	}
	// A missing user costs the same as a wrong password, so timing doesn't
	// tell which names exist.
	hash := u.PasswordHash
	if !ok {
		hash = dummyHash()
	}
	if !httpauth.CheckPassword(hash, r.PostFormValue("password")) || !ok {
		limits.failed(keys...)
		pg.Error = "That user name or password isn't right."
		return nil, pg.render(w, http.StatusUnauthorized)
	}
	if u.TOTPSecret == "" {
		limits.reset(userKey(u.Name))
		return &u, nil
	}
	tok, err := httpauth.Sign(h.key, purposePending, host, pending{User: u.Name, Version: userVersion(u),
		Expires: time.Now().Add(5 * time.Minute).Unix()})
	if err != nil {
		return nil, err
	}
	pg.Code, pg.Pending = true, tok
	return nil, pg.render(w, http.StatusOK)
}

// handoff is a login on the portal, for one app domain, for a minute.
type handoff struct {
	User    string `json:"u"`
	Version string `json:"v"`
	Expires int64  `json:"e"`
}

// callback takes a login handed over by the portal, once.
func (h *AuthHandler) callback(w http.ResponseWriter, r *http.Request) error {
	var ho handoff
	rd := localPath(r.FormValue("rd"))
	code := r.PostFormValue("code")
	if r.Method != http.MethodPost || !httpauth.Verify(h.key, purposeHandoff, hostOnly(r.Host), code, &ho) ||
		time.Now().Unix() >= ho.Expires || !usedCodes.first(code, time.Unix(ho.Expires, 0)) {
		http.Redirect(w, r, h.loginURL(r, rd), http.StatusFound)
		return nil
	}
	u, ok := h.users[ho.User]
	if !ok || userVersion(u) != ho.Version || h.Auth.Mode != types.AuthUsers || !h.mayEnter(u.Name) {
		pg := page{Title: "Log in", App: h.App, Error: fmt.Sprintf("%s may not use %s.", ho.User, h.App)}
		return pg.render(w, http.StatusForbidden)
	}
	if err := h.setSession(w, r, httpauth.Session{Kind: httpauth.KindUser, Name: u.Name, Version: ho.Version, Expires: h.sessionEnd()}); err != nil {
		return err
	}
	http.Redirect(w, r, rd, http.StatusSeeOther)
	return nil
}

func (h *AuthHandler) logout(w http.ResponseWriter, r *http.Request) error {
	setCookie(w, r, sessionCookie, "", time.Unix(0, 0))
	if h.Auth.Mode == types.AuthUsers && h.PortalURL != "" {
		http.Redirect(w, r, h.PortalURL+prefix+"logout", http.StatusFound)
		return nil
	}
	pg := page{Title: "Logged out", App: h.App, Message: "You're logged out.", Link: "/", LinkText: "Log in again"}
	return pg.render(w, http.StatusOK)
}

// share lets the holder of a share link in, until the link expires.
func (h *AuthHandler) share(w http.ResponseWriter, r *http.Request, token string) error {
	if wait := limits.wait(clientKey(r)); wait > 0 {
		pg := page{Title: "Link expired", App: h.App, Message: tooMany(wait)}
		return pg.render(w, http.StatusTooManyRequests)
	}
	hash := httpauth.HashToken(token)
	for _, s := range h.Auth.Shares {
		if s.TokenHash == hash && time.Now().Before(s.ExpiresAt) {
			err := h.setSession(w, r, httpauth.Session{Kind: httpauth.KindShare, Name: s.ID, App: h.App, Expires: s.ExpiresAt.Unix()})
			if err != nil {
				return err
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return nil
		}
	}
	limits.failed(clientKey(r))
	pg := page{Title: "Link expired", App: h.App, Message: "This link has expired or was revoked."}
	return pg.render(w, http.StatusNotFound)
}

// The portal: the login domain.

func (h *AuthHandler) servePortal(w http.ResponseWriter, r *http.Request) error {
	switch r.URL.Path {
	case prefix + "logout":
		setCookie(w, r, portalCookie, "", time.Unix(0, 0))
		pg := page{Title: "Logged out", Message: "You're logged out here. Sites you opened since logging in stay logged in until you log out of each."}
		return pg.render(w, http.StatusOK)
	case prefix + "login":
	case "/":
		if u, ok := h.portalUser(r); ok {
			pg := page{Title: "Logged in", Message: "You're logged in as " + u.Name + ".", Link: prefix + "logout", LinkText: "Log out"}
			return pg.render(w, http.StatusOK)
		}
	default:
		http.NotFound(w, r)
		return nil
	}
	back, target, err := h.portalTarget(r.FormValue("rd"))
	pg := page{Title: "Log in", Action: prefix + "login", RD: back, Users: true}
	if err != nil {
		if r.FormValue("rd") != "" {
			pg.RD, pg.Action, pg.Error = "", "", err.Error()
			return pg.render(w, http.StatusBadRequest)
		}
		target = "" // logging in to the portal itself
	} else {
		pg.App = target
	}
	if u, ok := h.portalUser(r); ok && r.Method != http.MethodPost && target != "" {
		return h.handOff(w, u, back, target)
	}
	if r.Method != http.MethodPost {
		return pg.render(w, http.StatusOK)
	}
	u, err := h.checkUser(w, r, &pg, "portal")
	if u == nil || err != nil {
		return err
	}
	v, err := httpauth.Sign(h.key, purposePortal, hostOnly(r.Host), httpauth.Session{Kind: httpauth.KindUser, Name: u.Name,
		Version: userVersion(*u), Expires: h.sessionEnd()})
	if err != nil {
		return err
	}
	setCookie(w, r, portalCookie, v, time.Unix(h.sessionEnd(), 0))
	if target == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
	return h.handOff(w, *u, back, target)
}

// portalTarget checks where a login on the portal goes back to: a domain
// with a users login, on its default port, over HTTPS if it is served so
// (whatever rd says). It returns the URL to go back to and its host.
func (h *AuthHandler) portalTarget(rd string) (string, string, error) {
	if rd == "" {
		return "", "", fmt.Errorf("no site to log in to")
	}
	u, err := url.Parse(rd)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", "", fmt.Errorf("unknown site")
	}
	if u.Port() != "" {
		return "", "", fmt.Errorf("%s doesn't log in here", u.Host)
	}
	host := strings.ToLower(u.Hostname())
	ph, ok := h.portalHost(host)
	if !ok {
		return "", "", fmt.Errorf("%s doesn't log in here", host)
	}
	path := localPath(u.RequestURI())
	scheme := "http"
	if ph.TLS {
		scheme = "https"
	}
	return scheme + "://" + host + path, host, nil
}

// portalHost is the login settings of host, a domain of an app with a
// users login (or matching its wildcard domain).
func (h *AuthHandler) portalHost(host string) (PortalHost, bool) {
	if ph, ok := h.Hosts[host]; ok {
		return ph, true
	}
	if _, parent, ok := strings.Cut(host, "."); ok {
		if ph, ok := h.Hosts["*."+parent]; ok {
			return ph, true
		}
	}
	return PortalHost{}, false
}

func (h *AuthHandler) portalUser(r *http.Request) (types.ProxyUser, bool) {
	var s httpauth.Session
	c, err := r.Cookie(portalCookie)
	if err != nil || !httpauth.Verify(h.key, purposePortal, hostOnly(r.Host), c.Value, &s) || time.Now().Unix() >= s.Expires {
		return types.ProxyUser{}, false
	}
	u, ok := h.users[s.Name]
	return u, ok && s.Version == userVersion(u)
}

// handOff sends a logged-in user back to the domain they came from, with a
// login for it, posted (so it never lands in a log or a Referer).
func (h *AuthHandler) handOff(w http.ResponseWriter, u types.ProxyUser, back, host string) error {
	ph, _ := h.portalHost(host)
	if len(ph.Users) > 0 && !slices.Contains(ph.Users, u.Name) {
		pg := page{Title: "Log in", Error: fmt.Sprintf("%s may not use %s.", u.Name, host), Link: prefix + "logout", LinkText: "Log in as someone else"}
		return pg.render(w, http.StatusForbidden)
	}
	b, _ := url.Parse(back)
	code, err := httpauth.Sign(h.key, purposeHandoff, host, handoff{User: u.Name, Version: userVersion(u), Expires: time.Now().Add(time.Minute).Unix()})
	if err != nil {
		return err
	}
	pg := page{Title: "Logging in", Handoff: b.Scheme + "://" + b.Host + prefix + "callback", HandoffCode: code, RD: b.RequestURI()}
	return pg.render(w, http.StatusOK)
}

// StripIdentity removes X-Jokku-User from requests, in every spelling an
// app's server might read as it (X_Jokku_User becomes HTTP_X_JOKKU_USER in
// CGI, Rack and WSGI): on every route, so no app ever trusts one a visitor
// sent, login or not.
type StripIdentity struct{}

func (StripIdentity) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{ID: "http.handlers.jokku_strip_identity", New: func() caddy.Module { return new(StripIdentity) }}
}

func (StripIdentity) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	stripIdentity(r.Header)
	return next.ServeHTTP(w, r)
}

func stripIdentity(h http.Header) {
	for k := range h {
		if strings.EqualFold(strings.ReplaceAll(k, "_", "-"), userHeader) {
			delete(h, k)
		}
	}
}

// Helpers

// clean reports whether a decoded path is already in its shortest form (a
// trailing slash aside), and its escaped form hides no slash.
func clean(decoded, escaped string) bool {
	c := pathpkg.Clean(decoded)
	return (c == decoded || c+"/" == decoded) && !strings.Contains(strings.ToLower(escaped), "%2f") &&
		!strings.Contains(escaped, "\\")
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(host)
}

// localPath keeps redirects on this domain: a path, never //elsewhere, and
// nothing a browser would read differently (a backslash, a tab it drops).
func localPath(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsRune(p, '\\') || strings.IndexFunc(p, unicode.IsControl) >= 0 || strings.ContainsRune(p, ' ') {
		return "/"
	}
	return p
}

func clientIP(r *http.Request) string {
	if ip, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && ip != "" {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func acceptsHTML(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return a == "" || strings.Contains(a, "text/html") || strings.Contains(a, "*/*") && r.Header.Get("Sec-Fetch-Mode") == "navigate"
}

func setCookie(w http.ResponseWriter, r *http.Request, name, value string, expires time.Time) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
	}
	http.SetCookie(w, c)
}

// stripCookie removes one cookie from the request, so the app never sees
// the session. It edits the header as sent: the app's own cookies reach it
// byte for byte, whatever their names and values.
func stripCookie(r *http.Request, name string) {
	lines := r.Header.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	var keep []string
	for _, line := range lines {
		var parts []string
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if n, _, _ := strings.Cut(part, "="); part != "" && strings.TrimSpace(n) != name {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			keep = append(keep, strings.Join(parts, "; "))
		}
	}
	r.Header.Del("Cookie")
	for _, k := range keep {
		r.Header.Add("Cookie", k)
	}
}

func tooMany(wait time.Duration) string {
	return fmt.Sprintf("Too many attempts. Try again in %d minutes.", int(wait.Minutes())+1)
}

// dummyHash is checked when a user doesn't exist, so that costs as much as
// a wrong password. It's made on first use: every jokku command loads this
// package, and a bcrypt hash takes a while.
var dummyHash = sync.OnceValue(func() string {
	h, _ := httpauth.HashPassword("jokku-no-such-user")
	return h
})

// limits slows down guessing. Failed logins are counted per client (an
// IPv4 address, or an IPv6 /64, which one machine usually has all of), per
// app (all clients together, so changing addresses doesn't help) and per
// user; once one count reaches its limit within failWindow, that kind of
// attempt waits. A success clears only that user's count. Counts are kept by
// each ingress node.
var limits = newLimiter()

const failWindow = 10 * time.Minute

// failLimits are the failures allowed per window, by kind of key.
var failLimits = map[byte]int{'c': 10, 'a': 30, 'u': 10}

func clientKey(r *http.Request) string {
	ip, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return "c:" + clientIP(r)
	}
	if ip = ip.Unmap(); ip.Is6() {
		p, _ := ip.Prefix(64)
		return "c:" + p.String()
	}
	return "c:" + ip.String()
}

func appKey(app string) string   { return "a:" + app }
func userKey(user string) string { return "u:" + user }

// maxKeys bounds memory: past it, new clients are no longer counted one by
// one (their app's and users' counts still are).
const maxKeys = 100000

type limiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{seen: map[string][]time.Time{}} }

func (l *limiter) recent(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.seen[key] {
		if now.Sub(t) < failWindow {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.seen, key)
	} else {
		l.seen[key] = keep
	}
	return keep
}

// wait is how long until none of keys is over its limit.
func (l *limiter) wait(keys ...string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var longest time.Duration
	for _, k := range keys {
		limit := failLimits[k[0]]
		if r := l.recent(k, now); limit > 0 && len(r) >= limit {
			longest = max(longest, failWindow-now.Sub(r[len(r)-limit]))
		}
	}
	return longest
}

func (l *limiter) failed(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.seen) > maxKeys {
		for k := range l.seen {
			l.recent(k, now)
		}
	}
	for _, k := range keys {
		if _, known := l.seen[k]; !known && k[0] == 'c' && len(l.seen) > maxKeys {
			continue
		}
		l.seen[k] = append(l.recent(k, now), now)
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, key)
}

// totpSteps remembers each user's last accepted TOTP step, so a code works
// once (on each ingress node).
var totpSteps = &stepTracker{last: map[string]uint64{}}

type stepTracker struct {
	mu   sync.Mutex
	last map[string]uint64
}

func (t *stepTracker) use(user string, step uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[user]; ok && step <= last {
		return false
	}
	t.last[user] = step
	return true
}

// usedCodes remembers the portal's hand-offs taken until they expire, so
// each works once.
var usedCodes = &codeTracker{seen: map[string]time.Time{}}

type codeTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (t *codeTracker) first(code string, expires time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for c, exp := range t.seen {
		if now.After(exp) {
			delete(t.seen, c)
		}
	}
	if _, used := t.seen[code]; used {
		return false
	}
	t.seen[code] = expires
	return true
}

var (
	_ caddy.Provisioner           = (*AuthHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*AuthHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*StripIdentity)(nil)
)
