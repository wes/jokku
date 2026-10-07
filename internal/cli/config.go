package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wes/jokku/internal/types"
)

var (
	noRestartFlag = Flag{Name: "no-restart", Help: "Don't restart the app after the change"}
	mergedFlag    = Flag{Name: "merged", Help: "Include global config vars (app values win)"}
)

var configCommands = []*Command{
	{Name: "config:show", Help: "Show all config vars", App: AppOrGlobal, Flags: []Flag{mergedFlag}, Run: configShow},
	{Name: "config:get", Help: "Print the value of one config var", App: AppOrGlobal, Args: "KEY", MinArgs: 1,
		Flags: []Flag{{Name: "quoted", Help: "Shell-quote the value"}, mergedFlag}, Run: configGet},
	{Name: "config:set", Help: "Set one or more config vars", App: AppOrGlobal, Args: "KEY1=VALUE1 [KEY2=VALUE2 ...]", MinArgs: 1, MaxArgs: -1,
		Flags: []Flag{noRestartFlag, {Name: "encoded", Help: "Values are base64 encoded"}}, Run: configSet},
	{Name: "config:unset", Help: "Unset one or more config vars", App: AppOrGlobal, Args: "KEY1 [KEY2 ...]", MinArgs: 1, MaxArgs: -1,
		Flags: []Flag{noRestartFlag}, Run: configUnset},
	{Name: "config:keys", Help: "List config var names", App: AppOrGlobal, Flags: []Flag{mergedFlag}, Run: configKeys},
	{Name: "config:export", Help: "Export config vars for use elsewhere", App: AppOrGlobal,
		Flags: []Flag{{Name: "format", Value: "FORMAT", Help: "exports (default), envfile, docker-args, shell, json, json-list or pretty"}, mergedFlag}, Run: configExport},
	{Name: "config:clear", Help: "Unset every config var", App: AppOrGlobal, Flags: []Flag{noRestartFlag}, Run: configClear},
}

// scopeName is how output refers to the target: the app name or "--global".
func (c *Context) scopeName() string {
	if c.App == "" {
		return "--global"
	}
	return c.App
}

func loadVars(c *Context) (map[string]string, error) {
	vars, err := c.API.Config(c, c.App)
	if err != nil || !c.Bool("merged") || c.App == "" {
		return vars, err
	}
	global, err := c.API.Config(c, "")
	if err != nil {
		return nil, err
	}
	for k, v := range vars {
		global[k] = v
	}
	return global, nil
}

func configShow(c *Context) error {
	vars, err := loadVars(c)
	if err != nil {
		return err
	}
	if c.App == "" {
		c.Header("global env vars")
	} else {
		c.Header("%s env vars", c.App)
	}
	printVars(c.Stdout, vars)
	return nil
}

func configGet(c *Context) error {
	vars, err := loadVars(c)
	if err != nil {
		return err
	}
	v, ok := vars[c.Args[0]]
	if !ok {
		return &exitError{code: 1} // Dokku prints nothing for a missing key
	}
	if c.Bool("quoted") {
		v = shellQuote(v)
	}
	fmt.Fprintln(c.Stdout, v)
	return nil
}

func configSet(c *Context) error {
	set := map[string]string{}
	for _, kv := range c.Args {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageErr("Invalid config pair %q, expected KEY=VALUE", kv)
		}
		if c.Bool("encoded") {
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return fmt.Errorf("value of %s is not valid base64: %v", k, err)
			}
			v = string(b)
		}
		set[k] = v
	}
	c.Step("Setting config vars")
	res, err := c.API.PatchConfig(c, c.App, types.ConfigPatch{Set: set})
	if err != nil {
		return err
	}
	keys := sortedKeys(set)
	width := 0
	for _, k := range keys {
		width = max(width, len(k)+1)
	}
	for _, k := range keys {
		c.Info("%-*s  %s", width, k+":", set[k])
	}
	return c.restartAfterConfig(res.Changed)
}

func configUnset(c *Context) error {
	for _, k := range c.Args {
		c.Step("Unsetting %s", k)
	}
	res, err := c.API.PatchConfig(c, c.App, types.ConfigPatch{Unset: c.Args})
	if err != nil {
		return err
	}
	return c.restartAfterConfig(res.Changed)
}

func configClear(c *Context) error {
	c.Step("Clearing config vars for %s", c.scopeName())
	res, err := c.API.PatchConfig(c, c.App, types.ConfigPatch{Clear: true})
	if err != nil {
		return err
	}
	return c.restartAfterConfig(res.Changed)
}

// restartAfterConfig applies a config change to a deployed app, unless
// --no-restart. Global changes apply to each app on its next restart.
func (c *Context) restartAfterConfig(changed bool) error {
	if !changed || c.App == "" || c.Bool("no-restart") {
		return nil
	}
	return c.restartIfDeployed()
}

func configKeys(c *Context) error {
	vars, err := loadVars(c)
	if err != nil {
		return err
	}
	for _, k := range sortedKeys(vars) {
		fmt.Fprintln(c.Stdout, k)
	}
	return nil
}

func configExport(c *Context) error {
	vars, err := loadVars(c)
	if err != nil {
		return err
	}
	keys := sortedKeys(vars)
	switch format := c.String("format"); format {
	case "", "exports":
		for _, k := range keys {
			fmt.Fprintf(c.Stdout, "export %s=%s\n", k, shellQuote(vars[k]))
		}
	case "envfile":
		for _, k := range keys {
			fmt.Fprintf(c.Stdout, "%s=%s\n", k, doubleQuote(vars[k]))
		}
	case "docker-args":
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = "--env=" + k + "=" + shellQuote(vars[k])
		}
		fmt.Fprintln(c.Stdout, strings.Join(parts, " "))
	case "shell":
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + shellQuote(vars[k])
		}
		fmt.Fprintln(c.Stdout, strings.Join(parts, " "))
	case "json":
		return json.NewEncoder(c.Stdout).Encode(vars)
	case "json-list":
		list := make([]map[string]string, len(keys))
		for i, k := range keys {
			list[i] = map[string]string{"name": k, "value": vars[k]}
		}
		return json.NewEncoder(c.Stdout).Encode(list)
	case "pretty":
		printVars(c.Stdout, vars)
	default:
		return usageErr("Unknown format %q", format)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func doubleQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "$", `\$`)
	return `"` + r.Replace(s) + `"`
}
