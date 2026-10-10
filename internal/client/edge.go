package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/wes/jokku/internal/types"
)

// Edges

func (c *Client) Edges(ctx context.Context) ([]types.Edge, error) {
	var out []types.Edge
	return out, c.call(ctx, http.MethodGet, "/v1/edges", nil, &out)
}

// CreateEdge adds an edge; the result holds the command that installs it.
func (c *Client) CreateEdge(ctx context.Context, req types.CreateEdgeRequest) (*types.Edge, error) {
	var e types.Edge
	return &e, c.call(ctx, http.MethodPost, "/v1/edges", req, &e)
}

func (c *Client) RemoveEdge(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v1/edges/"+url.PathEscape(name), nil, nil)
}

// External apps

func (c *Client) Externals(ctx context.Context) ([]types.External, error) {
	var out []types.External
	return out, c.call(ctx, http.MethodGet, "/v1/externals", nil, &out)
}

func (c *Client) External(ctx context.Context, name string) (*types.External, error) {
	var e types.External
	return &e, c.call(ctx, http.MethodGet, "/v1/externals/"+url.PathEscape(name), nil, &e)
}

func (c *Client) CreateExternal(ctx context.Context, req types.ExternalRequest) (*types.External, error) {
	var e types.External
	return &e, c.call(ctx, http.MethodPost, "/v1/externals", req, &e)
}

func (c *Client) PatchExternal(ctx context.Context, name string, req types.ExternalRequest) (*types.External, error) {
	var e types.External
	return &e, c.call(ctx, http.MethodPatch, "/v1/externals/"+url.PathEscape(name), req, &e)
}

func (c *Client) DestroyExternal(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v1/externals/"+url.PathEscape(name), nil, nil)
}

// Logins (http-auth); app "" is the cluster's settings.

func (c *Client) Auth(ctx context.Context, app string) (*types.AuthSettings, error) {
	var s types.AuthSettings
	return &s, c.call(ctx, http.MethodGet, appPath(app, "auth"), nil, &s)
}

func (c *Client) PatchAuth(ctx context.Context, app string, p types.AuthPatch) (*types.AuthSettings, error) {
	var s types.AuthSettings
	return &s, c.call(ctx, http.MethodPatch, appPath(app, "auth"), p, &s)
}

func (c *Client) AuthUsers(ctx context.Context) ([]types.AuthUser, error) {
	var out []types.AuthUser
	return out, c.call(ctx, http.MethodGet, "/v1/auth/users", nil, &out)
}

func (c *Client) CreateAuthUser(ctx context.Context, req types.AuthUserRequest) (*types.AuthUserResult, error) {
	var u types.AuthUserResult
	return &u, c.call(ctx, http.MethodPost, "/v1/auth/users", req, &u)
}

func (c *Client) PatchAuthUser(ctx context.Context, name string, req types.AuthUserRequest) (*types.AuthUserResult, error) {
	var u types.AuthUserResult
	return &u, c.call(ctx, http.MethodPatch, "/v1/auth/users/"+url.PathEscape(name), req, &u)
}

func (c *Client) DeleteAuthUser(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodDelete, "/v1/auth/users/"+url.PathEscape(name), nil, nil)
}

func (c *Client) AuthShares(ctx context.Context, app string) ([]types.AuthShare, error) {
	var out []types.AuthShare
	return out, c.call(ctx, http.MethodGet, appPath(app, "auth/shares"), nil, &out)
}

func (c *Client) CreateAuthShare(ctx context.Context, app string, req types.CreateShareRequest) (*types.AuthShare, error) {
	var s types.AuthShare
	return &s, c.call(ctx, http.MethodPost, appPath(app, "auth/shares"), req, &s)
}

func (c *Client) DeleteAuthShare(ctx context.Context, app, id string) error {
	return c.call(ctx, http.MethodDelete, appPath(app, "auth/shares/"+url.PathEscape(id)), nil, nil)
}
