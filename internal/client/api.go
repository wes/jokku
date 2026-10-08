package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/wes/jokku/internal/types"
)

func (c *Client) Version(ctx context.Context) (*types.Version, error) {
	var v types.Version
	return &v, c.call(ctx, http.MethodGet, "/v1/version", nil, &v)
}

// Apps

func (c *Client) Apps(ctx context.Context) ([]types.App, error) {
	var apps []types.App
	return apps, c.call(ctx, http.MethodGet, "/v1/apps", nil, &apps)
}

func (c *Client) App(ctx context.Context, app string) (*types.App, error) {
	var a types.App
	return &a, c.call(ctx, http.MethodGet, appPath(app, ""), nil, &a)
}

func (c *Client) CreateApp(ctx context.Context, app string) (*types.App, error) {
	var a types.App
	return &a, c.call(ctx, http.MethodPost, "/v1/apps", types.CreateAppRequest{Name: app}, &a)
}

func (c *Client) DestroyApp(ctx context.Context, app string) error {
	return c.call(ctx, http.MethodDelete, appPath(app, ""), nil, nil)
}

func (c *Client) RenameApp(ctx context.Context, app, newName string) error {
	return c.call(ctx, http.MethodPost, appPath(app, "rename"), types.RenameAppRequest{NewName: newName}, nil)
}

func (c *Client) CloneApp(ctx context.Context, app, newName string) error {
	return c.call(ctx, http.MethodPost, appPath(app, "clone"), types.CloneAppRequest{NewName: newName}, nil)
}

func (c *Client) SetAppLocked(ctx context.Context, app string, locked bool) error {
	method := http.MethodPut
	if !locked {
		method = http.MethodDelete
	}
	return c.call(ctx, method, appPath(app, "lock"), nil, nil)
}

// Config vars; app "" is the global scope.

func (c *Client) Config(ctx context.Context, app string) (map[string]string, error) {
	var res types.ConfigVars
	return res.Vars, c.call(ctx, http.MethodGet, appPath(app, "config"), nil, &res)
}

func (c *Client) PatchConfig(ctx context.Context, app string, p types.ConfigPatch) (*types.ConfigVars, error) {
	var res types.ConfigVars
	return &res, c.call(ctx, http.MethodPatch, appPath(app, "config"), p, &res)
}

// Domains; app "" is the global scope.

func (c *Client) Domains(ctx context.Context, app string) (*types.Domains, error) {
	var d types.Domains
	return &d, c.call(ctx, http.MethodGet, appPath(app, "domains"), nil, &d)
}

func (c *Client) PatchDomains(ctx context.Context, app string, p types.DomainsPatch) (*types.Domains, error) {
	var d types.Domains
	return &d, c.call(ctx, http.MethodPatch, appPath(app, "domains"), p, &d)
}

// Properties; app "" is the global scope.

func (c *Client) Properties(ctx context.Context, app, plugin string) (*types.Properties, error) {
	var p types.Properties
	return &p, c.call(ctx, http.MethodGet, appPath(app, "properties/"+url.PathEscape(plugin)), nil, &p)
}

func (c *Client) SetProperty(ctx context.Context, app, plugin, key, value string) error {
	path := appPath(app, "properties/"+url.PathEscape(plugin)+"/"+url.PathEscape(key))
	return c.call(ctx, http.MethodPut, path, types.SetPropertyRequest{Value: value}, nil)
}

// Processes and resources

func (c *Client) Formation(ctx context.Context, app string) ([]types.Process, error) {
	var f types.Formation
	return f.Processes, c.call(ctx, http.MethodGet, appPath(app, "formation"), nil, &f)
}

func (c *Client) Scale(ctx context.Context, app string, req types.ScaleRequest) ([]types.Process, error) {
	var f types.Formation
	return f.Processes, c.call(ctx, http.MethodPost, appPath(app, "scale"), req, &f)
}

func (c *Client) Resources(ctx context.Context, app string) (*types.Resources, error) {
	var r types.Resources
	return &r, c.call(ctx, http.MethodGet, appPath(app, "resources"), nil, &r)
}

func (c *Client) SetResources(ctx context.Context, app string, l types.ResourceLimits) (*types.Resources, error) {
	var r types.Resources
	return &r, c.call(ctx, http.MethodPatch, appPath(app, "resources"), l, &r)
}

// Deploys

func (c *Client) Deploys(ctx context.Context, app string) ([]types.Deploy, error) {
	var ds []types.Deploy
	return ds, c.call(ctx, http.MethodGet, appPath(app, "deploys"), nil, &ds)
}

// Deploy uploads a source tarball and streams the build and rollout. It
// returns the deploy's failure, if any.
func (c *Client) Deploy(ctx context.Context, app, source, ref string, tarball io.Reader, onEvent func(types.Event)) error {
	q := url.Values{"source": {source}}
	if ref != "" {
		q.Set("ref", ref)
	}
	req, err := c.newRequest(ctx, http.MethodPost, appPath(app, "deploys")+"?"+q.Encode(), tarball)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return stream(resp.Body, onEvent)
}

// EnsureGitRepo creates the app's bare repo (with its deploy hook) if needed
// and returns its path on the server.
func (c *Client) EnsureGitRepo(ctx context.Context, app string) (string, error) {
	var r types.GitRepo
	return r.Path, c.call(ctx, http.MethodPost, appPath(app, "git"), nil, &r)
}

// SSH keys

func (c *Client) SSHKeys(ctx context.Context) ([]types.SSHKey, error) {
	var keys []types.SSHKey
	return keys, c.call(ctx, http.MethodGet, "/v1/ssh-keys", nil, &keys)
}

func (c *Client) AddSSHKey(ctx context.Context, name, publicKey string) (*types.SSHKey, error) {
	var k types.SSHKey
	return &k, c.call(ctx, http.MethodPost, "/v1/ssh-keys", types.AddSSHKeyRequest{Name: name, PublicKey: publicKey}, &k)
}

func (c *Client) RemoveSSHKey(ctx context.Context, nameOrFingerprint string, byFingerprint bool) error {
	path := "/v1/ssh-keys/" + url.PathEscape(nameOrFingerprint)
	if byFingerprint {
		path += "?fingerprint=true"
	}
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}

// Nodes

func (c *Client) Nodes(ctx context.Context) ([]types.Node, error) {
	var nodes []types.Node
	return nodes, c.call(ctx, http.MethodGet, "/v1/nodes", nil, &nodes)
}

func (c *Client) Node(ctx context.Context, name string) (*types.Node, error) {
	var n types.Node
	return &n, c.call(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(name), nil, &n)
}

// SetNodeFlag sets schedulable, ingress or draining on a node.
func (c *Client) SetNodeFlag(ctx context.Context, name, flag string, value bool) (*types.Node, error) {
	var n types.Node
	return &n, c.call(ctx, http.MethodPatch, "/v1/nodes/"+url.PathEscape(name), map[string]bool{flag: value}, &n)
}

// Processes and logs

// PS runs restart, start, stop or rebuild and streams its progress.
func (c *Client) PS(ctx context.Context, app, action string, onEvent func(types.Event)) error {
	return c.streamCall(ctx, http.MethodPost, appPath(app, "ps/"+url.PathEscape(action)), onEvent)
}

func (c *Client) Instances(ctx context.Context, app string) ([]types.Instance, error) {
	var out []types.Instance
	return out, c.call(ctx, http.MethodGet, appPath(app, "instances"), nil, &out)
}

func (c *Client) Releases(ctx context.Context, app string) ([]types.Release, error) {
	var out []types.Release
	return out, c.call(ctx, http.MethodGet, appPath(app, "releases"), nil, &out)
}

// Logs streams log lines; with Follow it runs until ctx is done.
func (c *Client) Logs(ctx context.Context, app string, o types.LogOptions, onEvent func(types.Event)) error {
	q := url.Values{"tail": {strconv.Itoa(o.Tail)}}
	if o.Follow {
		q.Set("follow", "true")
	}
	if o.Process != "" {
		q.Set("process", o.Process)
	}
	return c.streamCall(ctx, http.MethodGet, appPath(app, "logs")+"?"+q.Encode(), onEvent)
}

func (c *Client) streamCall(ctx context.Context, method, path string, onEvent func(types.Event)) error {
	req, err := c.newRequest(ctx, method, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return stream(resp.Body, onEvent)
}

// Cluster

func (c *Client) CreateJoinToken(ctx context.Context, req types.CreateJoinTokenRequest) (*types.JoinToken, error) {
	var t types.JoinToken
	return &t, c.call(ctx, http.MethodPost, "/v1/cluster/join-tokens", req, &t)
}

func (c *Client) ClusterStatus(ctx context.Context) (*types.ClusterStatus, error) {
	var st types.ClusterStatus
	return &st, c.call(ctx, http.MethodGet, "/v1/cluster/status", nil, &st)
}

func (c *Client) Events(ctx context.Context, app string, limit int) ([]types.ClusterEvent, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if app != "" {
		q.Set("app", app)
	}
	var out []types.ClusterEvent
	return out, c.call(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil, &out)
}

func (c *Client) RemoveNode(ctx context.Context, name string, force bool) error {
	path := "/v1/nodes/" + url.PathEscape(name)
	if force {
		path += "?force=true"
	}
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}

// Requests streams the HTTP requests the cluster's proxies handle (all apps
// when app is ""): the last tail, then new ones while following.
func (c *Client) Requests(ctx context.Context, app string, tail int, follow bool, fn func(types.Request)) error {
	q := url.Values{"tail": {strconv.Itoa(tail)}}
	if app != "" {
		q.Set("app", app)
	}
	if follow {
		q.Set("follow", "true")
	}
	return c.streamCall(ctx, http.MethodGet, "/v1/requests?"+q.Encode(), func(e types.Event) {
		if e.Request != nil {
			fn(*e.Request)
		}
	})
}

// Volumes

func volumePath(app, name, rest string) string {
	p := appPath(app, "volumes/"+url.PathEscape(name))
	if rest != "" {
		p += "/" + rest
	}
	return p
}

func (c *Client) Volumes(ctx context.Context, app string) ([]types.Volume, error) {
	var out []types.Volume
	return out, c.call(ctx, http.MethodGet, appPath(app, "volumes"), nil, &out)
}

func (c *Client) Volume(ctx context.Context, app, name string) (*types.Volume, error) {
	var v types.Volume
	return &v, c.call(ctx, http.MethodGet, volumePath(app, name, ""), nil, &v)
}

func (c *Client) CreateVolume(ctx context.Context, app string, req types.CreateVolumeRequest) (*types.Volume, error) {
	var v types.Volume
	return &v, c.call(ctx, http.MethodPost, appPath(app, "volumes"), req, &v)
}

func (c *Client) ResizeVolume(ctx context.Context, app, name string, sizeMB int) (*types.Volume, error) {
	var v types.Volume
	return &v, c.call(ctx, http.MethodPatch, volumePath(app, name, ""), types.ResizeVolumeRequest{SizeMB: sizeMB}, &v)
}

func (c *Client) DestroyVolume(ctx context.Context, app, name string) error {
	return c.call(ctx, http.MethodDelete, volumePath(app, name, ""), nil, nil)
}

func (c *Client) MountVolume(ctx context.Context, app, name string, m types.VolumeMount) (*types.Volume, error) {
	var v types.Volume
	return &v, c.call(ctx, http.MethodPost, volumePath(app, name, "mounts"), m, &v)
}

// UnmountVolume removes a mount; an empty path removes the volume's only one.
func (c *Client) UnmountVolume(ctx context.Context, app, name string, m types.VolumeMount) (*types.Volume, error) {
	q := url.Values{}
	if m.ProcessType != "" {
		q.Set("process_type", m.ProcessType)
	}
	if m.Path != "" {
		q.Set("path", m.Path)
	}
	var v types.Volume
	return &v, c.call(ctx, http.MethodDelete, volumePath(app, name, "mounts")+"?"+q.Encode(), nil, &v)
}

func (c *Client) MoveVolume(ctx context.Context, app, name, node string) (*types.Volume, error) {
	var v types.Volume
	return &v, c.call(ctx, http.MethodPost, volumePath(app, name, "move"), types.MoveVolumeRequest{Node: node}, &v)
}

// DeployImage deploys a registry image (git:from-image) and streams the pull
// and rollout.
func (c *Client) DeployImage(ctx context.Context, app, image string, onEvent func(types.Event)) error {
	return c.Deploy(ctx, app, "image", image, http.NoBody, onEvent)
}

// Registry logins

func (c *Client) RegistryLogins(ctx context.Context) ([]types.RegistryLogin, error) {
	var out []types.RegistryLogin
	return out, c.call(ctx, http.MethodGet, "/v1/registries", nil, &out)
}

func (c *Client) SetRegistryLogin(ctx context.Context, server, username, password string) error {
	return c.call(ctx, http.MethodPut, "/v1/registries/"+url.PathEscape(server),
		types.RegistryLoginRequest{Username: username, Password: password}, nil)
}

func (c *Client) DeleteRegistryLogin(ctx context.Context, server string) error {
	return c.call(ctx, http.MethodDelete, "/v1/registries/"+url.PathEscape(server), nil, nil)
}
