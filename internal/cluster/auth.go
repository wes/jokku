package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"

	"github.com/wes/jokku/internal/types"
)

// Logins in front of apps (http-auth) are properties of plugin "http-auth":
// per app, mode (password or users; anything else is off), password-hash,
// users, allowed-ips and bypass-paths; globally, login-domain,
// session-days and session-key, which signs sessions. They aren't in
// props.Plugins: the hash and the key must never be shown by a report.
const AuthPlugin = "http-auth"

// DefaultSessionDays is how long a login lasts unless set otherwise.
const DefaultSessionDays = 30

// SplitList reads a comma-separated property.
func SplitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// AuthKey returns the key that signs sessions, making it on first use.
func (c *Controller) AuthKey(ctx context.Context) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	return c.Store.EnsureProperty(ctx, "", AuthPlugin, "session-key", base64.RawStdEncoding.EncodeToString(b))
}

// RotateAuthKey replaces the session key, if there is one, which logs
// everyone out: for when a machine that held it (an edge) leaves.
func (c *Controller) RotateAuthKey(ctx context.Context) error {
	p, err := c.Store.Properties(ctx, "", AuthPlugin)
	if err != nil || p["session-key"] == "" {
		return err
	}
	b := make([]byte, 32)
	rand.Read(b)
	return c.Store.SetProperty(ctx, "", AuthPlugin, "session-key", base64.RawStdEncoding.EncodeToString(b))
}

// authState is the login part of the proxies' state: each app's login, and
// the cluster's settings when any app has one (or a login domain is set).
func (c *Controller) authState(ctx context.Context, apps []types.App) (*types.ProxyAuth, map[string]*types.RouteAuth, error) {
	routes := map[string]*types.RouteAuth{}
	var shares map[string][]types.ProxyShare
	for _, a := range apps {
		p, err := c.Store.Properties(ctx, a.Name, AuthPlugin)
		if err != nil {
			return nil, nil, err
		}
		mode := p["mode"]
		if mode != types.AuthPassword && mode != types.AuthUsers {
			continue
		}
		if mode == types.AuthPassword && p["password-hash"] == "" {
			continue // the API never leaves it so; better open than locked for good
		}
		if shares == nil {
			if shares, err = c.shares(ctx); err != nil {
				return nil, nil, err
			}
		}
		routes[a.Name] = &types.RouteAuth{
			Mode: mode, PasswordHash: p["password-hash"], Users: SplitList(p["users"]),
			AllowIPs: SplitList(p["allowed-ips"]), BypassPaths: SplitList(p["bypass-paths"]), Shares: shares[a.Name],
		}
	}
	global, err := c.Store.Properties(ctx, "", AuthPlugin)
	if err != nil {
		return nil, nil, err
	}
	if len(routes) == 0 && global["login-domain"] == "" {
		return nil, routes, nil
	}
	key := global["session-key"]
	if key == "" {
		if key, err = c.AuthKey(ctx); err != nil {
			return nil, nil, err
		}
	}
	days, err := strconv.Atoi(global["session-days"])
	if err != nil || days <= 0 {
		days = DefaultSessionDays
	}
	users, err := c.Store.AuthUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	auth := &types.ProxyAuth{Key: key, LoginDomain: global["login-domain"], SessionDays: days}
	for _, u := range users {
		auth.Users = append(auth.Users, types.ProxyUser{Name: u.Name, PasswordHash: u.PasswordHash, TOTPSecret: u.TOTPSecret})
	}
	return auth, routes, nil
}

func (c *Controller) shares(ctx context.Context) (map[string][]types.ProxyShare, error) {
	all, err := c.Store.AuthShares(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string][]types.ProxyShare{}
	for _, sh := range all {
		out[sh.App] = append(out[sh.App], types.ProxyShare{ID: sh.ID, TokenHash: sh.TokenHash, ExpiresAt: sh.ExpiresAt})
	}
	return out, nil
}
