package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/httpauth"
	"github.com/wes/jokku/internal/props"
	"github.com/wes/jokku/internal/proxy"
	"github.com/wes/jokku/internal/store"
	"github.com/wes/jokku/internal/types"
)

// Logins in front of apps (http-auth). Settings are properties of plugin
// cluster.AuthPlugin; users and share links have their own tables.

var (
	userNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{0,63}$`)
	hostRe     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)
)

const (
	minPassword = 6 // for mode password: a PIN at least, and longer is better
	minUserPass = 8
	maxShareTTL = 366 * 24 * time.Hour
)

func (s *Server) getAuth(w http.ResponseWriter, r *http.Request) {
	st, err := s.authSettings(r.Context(), r.PathValue("app"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) authSettings(ctx context.Context, app string) (*types.AuthSettings, error) {
	p, err := s.Store.Properties(ctx, app, cluster.AuthPlugin)
	if err != nil {
		return nil, err
	}
	if app == "" {
		days, err := strconv.Atoi(p["session-days"])
		if err != nil || days <= 0 {
			days = cluster.DefaultSessionDays
		}
		return &types.AuthSettings{Mode: "off", LoginDomain: p["login-domain"], SessionDays: days}, nil
	}
	st := &types.AuthSettings{App: app, Mode: p["mode"], HasPassword: p["password-hash"] != "", Users: cluster.SplitList(p["users"]),
		AllowIPs: cluster.SplitList(p["allowed-ips"]), BypassPaths: cluster.SplitList(p["bypass-paths"])}
	if st.Mode != types.AuthPassword && st.Mode != types.AuthUsers {
		st.Mode = "off"
	}
	shares, err := s.Store.AuthShares(ctx, app)
	if err != nil {
		return nil, err
	}
	st.Shares = len(shares)
	return st, nil
}

// patchAuth changes an app's login, or (without an app) the cluster's login
// settings.
func (s *Server) patchAuth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	var p types.AuthPatch
	if err := decode(r, &p); err != nil {
		s.fail(w, r, err)
		return
	}
	var values map[string]string
	var err error
	if app == "" {
		values, err = s.globalAuthValues(ctx, p)
	} else {
		values, err = s.appAuthValues(ctx, app, p)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if values["mode"] != "" || values["login-domain"] != "" {
		if _, err := s.Cluster.AuthKey(ctx); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.Store.SetProperties(ctx, app, cluster.AuthPlugin, values); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	s.getAuth(w, r)
}

func (s *Server) globalAuthValues(ctx context.Context, p types.AuthPatch) (map[string]string, error) {
	values := map[string]string{}
	if p.Mode != "" || p.Password != "" || p.Users != nil || p.AllowIPs != nil || p.BypassPaths != nil {
		return nil, badRequest("Logins are turned on per app: jokku http-auth:enable <app>")
	}
	if p.LoginDomain != nil {
		d := strings.ToLower(strings.TrimSpace(*p.LoginDomain))
		if d != "" {
			if !hostRe.MatchString(d) {
				return nil, badRequest("Invalid login domain %q, expected e.g. auth.example.com", d)
			}
			if owner, err := s.domainOwner(ctx, d); err != nil {
				return nil, err
			} else if owner != "" {
				return nil, badRequest("%s is a domain of %s; the login domain needs one of its own", d, owner)
			}
		}
		values["login-domain"] = d
	}
	if p.SessionDays != 0 {
		if p.SessionDays < 1 || p.SessionDays > 366 {
			return nil, badRequest("Sessions last between 1 and 366 days")
		}
		values["session-days"] = strconv.Itoa(p.SessionDays)
	}
	return values, nil
}

func (s *Server) domainOwner(ctx context.Context, domain string) (string, error) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		return "", err
	}
	for _, a := range apps {
		ds, err := s.Store.Domains(ctx, a.Name)
		if err != nil {
			return "", err
		}
		for _, d := range ds {
			if strings.EqualFold(d, domain) {
				return a.Name, nil
			}
		}
	}
	return "", nil
}

func (s *Server) appAuthValues(ctx context.Context, app string, p types.AuthPatch) (map[string]string, error) {
	current, err := s.Store.Properties(ctx, app, cluster.AuthPlugin)
	if err != nil {
		return nil, err
	}
	if p.LoginDomain != nil || p.SessionDays != 0 {
		return nil, badRequest("The login domain and session length are cluster settings: use --global")
	}
	values := map[string]string{}
	switch p.Mode {
	case "":
	case "off":
		values["mode"] = "off"
	case types.AuthPassword:
		if p.Password == "" && current["password-hash"] == "" {
			return nil, badRequest("Give %s a password (or PIN) to log in with", app)
		}
		values["mode"] = p.Mode
	case types.AuthUsers:
		values["mode"] = p.Mode
	default:
		return nil, badRequest("Unknown login mode %q, expected password, users or off", p.Mode)
	}
	if p.Password != "" {
		if len(p.Password) < minPassword {
			return nil, badRequest("The password needs at least %d characters", minPassword)
		}
		h, err := httpauth.HashPassword(p.Password)
		if err != nil {
			return nil, badRequest("%v", err)
		}
		values["password-hash"] = h
	}
	if p.Users != nil {
		for _, u := range *p.Users {
			if _, err := s.Store.AuthUser(ctx, u); err != nil {
				return nil, badRequest("No user %s; add one with jokku http-auth:users:add %s", u, u)
			}
		}
		values["users"] = strings.Join(*p.Users, ",")
	}
	if p.AllowIPs != nil {
		var cidrs []string
		for _, a := range *p.AllowIPs {
			pfx, err := netip.ParsePrefix(a)
			if err != nil {
				addr, aerr := netip.ParseAddr(a)
				if aerr != nil {
					return nil, badRequest("Invalid address %q, expected an IP or a CIDR such as 192.168.1.0/24", a)
				}
				pfx = netip.PrefixFrom(addr, addr.BitLen())
			}
			cidrs = append(cidrs, pfx.Masked().String())
		}
		values["allowed-ips"] = strings.Join(cidrs, ",")
	}
	if p.BypassPaths != nil {
		for _, path := range *p.BypassPaths {
			if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, ", ") || strings.Contains(path[:max(len(path)-1, 0)], "*") {
				return nil, badRequest("Invalid path %q: it starts with / and may end with * to match everything below", path)
			}
		}
		values["bypass-paths"] = strings.Join(*p.BypassPaths, ",")
	}
	return values, nil
}

// Users

func (s *Server) listAuthUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.AuthUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.AuthUser{}
	for _, u := range users {
		out = append(out, authUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func authUser(u store.AuthUser) types.AuthUser {
	return types.AuthUser{Name: u.Name, TOTP: u.TOTPSecret != "", CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

func (s *Server) createAuthUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req types.AuthUserRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if !userNameRe.MatchString(req.Name) {
		s.fail(w, r, badRequest("User name %q is invalid: use lowercase letters, digits and . _ @ -", req.Name))
		return
	}
	hash, err := userPassword(req.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.CreateAuthUser(ctx, req.Name, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.updateAuthUser(w, r, req.Name, types.AuthUserRequest{TOTP: req.TOTP}, http.StatusCreated)
}

func userPassword(password string) (string, error) {
	if len(password) < minUserPass {
		return "", badRequest("The password needs at least %d characters", minUserPass)
	}
	h, err := httpauth.HashPassword(password)
	if err != nil {
		return "", badRequest("%v", err)
	}
	return h, nil
}

func (s *Server) patchAuthUser(w http.ResponseWriter, r *http.Request) {
	var req types.AuthUserRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	s.updateAuthUser(w, r, r.PathValue("name"), req, http.StatusOK)
}

// updateAuthUser changes a user's password and second factor, and returns
// the user with their new TOTP secret, if one was made.
func (s *Server) updateAuthUser(w http.ResponseWriter, r *http.Request, name string, req types.AuthUserRequest, status int) {
	ctx := r.Context()
	if _, err := s.Store.AuthUser(ctx, name); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Password != "" {
		hash, err := userPassword(req.Password)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.Store.SetAuthUserPassword(ctx, name, hash); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	secret := ""
	if req.TOTP != nil {
		if *req.TOTP {
			secret = httpauth.NewTOTPSecret()
		}
		if err := s.Store.SetAuthUserTOTP(ctx, name, secret); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	u, err := s.Store.AuthUser(ctx, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	res := types.AuthUserResult{AuthUser: authUser(*u)}
	if secret != "" {
		res.TOTPSecret, res.TOTPURI = secret, httpauth.TOTPURI("Jokku", name, secret)
	}
	writeJSON(w, status, res)
}

func (s *Server) deleteAuthUser(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteAuthUser(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}

// Share links

func (s *Server) listAuthShares(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	if _, err := s.Store.App(ctx, app); err != nil {
		s.fail(w, r, err)
		return
	}
	shares, err := s.Store.AuthShares(ctx, app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := []types.AuthShare{}
	for _, sh := range shares {
		out = append(out, types.AuthShare{ID: sh.ID, App: sh.App, Note: sh.Note, ExpiresAt: sh.ExpiresAt, CreatedAt: sh.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createAuthShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := r.PathValue("app")
	var req types.CreateShareRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > maxShareTTL {
		s.fail(w, r, badRequest("A share link lasts between a second and a year"))
		return
	}
	st, err := s.authSettings(ctx, app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if st.Mode == "off" {
		s.fail(w, r, badRequest("%s has no login, so it needs no share link: turn one on with jokku http-auth:enable %s", app, app))
		return
	}
	base, err := s.appURL(ctx, app)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	idb := make([]byte, 4)
	rand.Read(idb)
	token := httpauth.Token()
	sh := &store.AuthShare{ID: hex.EncodeToString(idb), App: app, TokenHash: httpauth.HashToken(token), Note: req.Note,
		ExpiresAt: time.Now().Add(ttl).UTC().Truncate(time.Second)}
	if err := s.Store.CreateAuthShare(ctx, sh); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	writeJSON(w, http.StatusCreated, types.AuthShare{ID: sh.ID, App: app, Note: sh.Note, ExpiresAt: sh.ExpiresAt, CreatedAt: sh.CreatedAt,
		URL: base + "/.jokku/share/" + token})
}

// appURL is where app is reached: its first domain with a certificate, or
// its first domain.
func (s *Server) appURL(ctx context.Context, app string) (string, error) {
	domains, err := s.Store.Domains(ctx, app)
	if err != nil {
		return "", err
	}
	if len(domains) == 0 {
		return "", badRequest("%s has no domain to share; add one with jokku domains:add %s <domain>", app, app)
	}
	le, _ := props.Lookup("letsencrypt")
	appLE, err := s.Store.Properties(ctx, app, "letsencrypt")
	if err != nil {
		return "", err
	}
	globalLE, err := s.Store.Properties(ctx, "", "letsencrypt")
	if err != nil {
		return "", err
	}
	tls := props.Compute(le, appLE, globalLE)["enabled"] == "true"
	for _, d := range domains {
		if tls && proxy.PublicHost(d) && !strings.HasPrefix(d, "*.") {
			return "https://" + d, nil
		}
	}
	for _, d := range domains {
		if !strings.HasPrefix(d, "*.") {
			return "http://" + d, nil
		}
	}
	return "", badRequest("%s has only wildcard domains; add one it can be reached at", app)
}

func (s *Server) deleteAuthShare(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteAuthShare(r.Context(), r.PathValue("app"), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Cluster.Changed()
	w.WriteHeader(http.StatusNoContent)
}
