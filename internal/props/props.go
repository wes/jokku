// Package props declares the Dokku-style "<plugin>:set <app> <key> <value>"
// properties. Each plugin's keys live in the generic properties table, and
// the CLI generates "<plugin>:set" and "<plugin>:report" commands from this
// list, so adding a setting is one entry here.
package props

import (
	"fmt"
	"strconv"
	"strings"
)

type Key struct {
	Name     string
	Default  string
	Help     string
	Validate func(string) error
}

type Plugin struct {
	Name string
	Help string
	Keys []Key
	// Settable is false for plugins whose properties are only changed by
	// dedicated commands (proxy:enable, letsencrypt:disable, ...).
	Settable bool
}

var Plugins = []Plugin{
	{
		Name: "git", Help: "Manage app deploys via git", Settable: true,
		Keys: []Key{
			{Name: "deploy-branch", Default: "main", Help: "Branch that triggers a deploy when pushed"},
			{Name: "keep-git-dir", Default: "false", Help: "Keep the .git directory in the build context", Validate: isBool},
		},
	},
	{
		Name: "builder", Help: "Manage the builder settings for an app", Settable: true,
		Keys: []Key{
			{Name: "selected", Default: "", Help: "The builder: dockerfile (the default) or compose", Validate: oneOf("", "dockerfile", "compose")},
			{Name: "build-dir", Default: "", Help: "Subdirectory of the repo to build from"},
		},
	},
	{
		Name: "builder-dockerfile", Help: "Manage the Dockerfile builder", Settable: true,
		Keys: []Key{
			{Name: "dockerfile-path", Default: "Dockerfile", Help: "Path to the Dockerfile, relative to the build dir"},
		},
	},
	{
		Name: "builder-compose", Help: "Manage the compose builder", Settable: true,
		Keys: []Key{
			{Name: "compose-file", Default: "", Help: "Path to the compose file, relative to the build dir (default: compose.yaml, compose.yml, docker-compose.yaml or docker-compose.yml)"},
		},
	},
	{
		Name: "checks", Help: "Manage zero-downtime deploy checks", Settable: true,
		Keys: []Key{
			{Name: "wait-to-retire", Default: "60", Help: "Seconds to keep old instances after a deploy", Validate: isNonNegativeInt},
			{Name: "timeout", Default: "120", Help: "Seconds new instances get to boot and pass checks", Validate: isNonNegativeInt},
		},
	},
	{
		Name: "ps", Help: "Manage app processes", Settable: true,
		Keys: []Key{
			{Name: "procfile-path", Default: "Procfile", Help: "Path to the Procfile, relative to the build dir"},
			{Name: "restart-policy", Default: "on-failure:10", Help: "always | no | on-failure[:N]", Validate: isRestartPolicy},
		},
	},
	{
		Name: "proxy", Help: "Manage the HTTP proxy for an app",
		Keys: []Key{
			{Name: "enabled", Default: "true", Help: "Route HTTP traffic to the app", Validate: isBool},
		},
	},
	{
		Name: "letsencrypt", Help: "Manage automatic TLS certificates", Settable: true,
		Keys: []Key{
			{Name: "email", Default: "", Help: "ACME account email (usually set with --global)"},
			// New apps start with "false" (see store.CreateApp); the default
			// is for apps created before that.
			{Name: "enabled", Default: "true", Help: "Request certificates for the app's domains", Validate: isBool},
		},
	},
}

func Lookup(plugin string) (Plugin, bool) {
	for _, p := range Plugins {
		if p.Name == plugin {
			return p, true
		}
	}
	return Plugin{}, false
}

func (p Plugin) Key(name string) (Key, bool) {
	for _, k := range p.Keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

// Check validates a value for plugin/key. An empty value (unset) is always
// valid.
func Check(plugin, key, value string) error {
	p, ok := Lookup(plugin)
	if !ok {
		return fmt.Errorf("unknown plugin %q", plugin)
	}
	k, ok := p.Key(key)
	if !ok {
		names := make([]string, len(p.Keys))
		for i, k := range p.Keys {
			names[i] = k.Name
		}
		return fmt.Errorf("invalid %s property %q, valid properties: %s", plugin, key, strings.Join(names, ", "))
	}
	if value == "" || k.Validate == nil {
		return nil
	}
	if err := k.Validate(value); err != nil {
		return fmt.Errorf("invalid value for %s %s: %w", plugin, key, err)
	}
	return nil
}

// Compute resolves a key: the app value wins, then the global value, then the
// default.
func Compute(p Plugin, app, global map[string]string) map[string]string {
	out := make(map[string]string, len(p.Keys))
	for _, k := range p.Keys {
		switch {
		case app[k.Name] != "":
			out[k.Name] = app[k.Name]
		case global[k.Name] != "":
			out[k.Name] = global[k.Name]
		default:
			out[k.Name] = k.Default
		}
	}
	return out
}

func isBool(v string) error {
	if v != "true" && v != "false" {
		return fmt.Errorf("must be true or false")
	}
	return nil
}

func isNonNegativeInt(v string) error {
	if n, err := strconv.Atoi(v); err != nil || n < 0 {
		return fmt.Errorf("must be a whole number of seconds")
	}
	return nil
}

func isRestartPolicy(v string) error {
	if v == "always" || v == "no" || v == "unless-stopped" || v == "on-failure" {
		return nil
	}
	if n, ok := strings.CutPrefix(v, "on-failure:"); ok {
		if _, err := strconv.Atoi(n); err == nil {
			return nil
		}
	}
	return fmt.Errorf("must be always, no, unless-stopped or on-failure[:N]")
}

func oneOf(allowed ...string) func(string) error {
	return func(v string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		var names []string
		for _, a := range allowed {
			if a != "" {
				names = append(names, a)
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(names, ", "))
	}
}
