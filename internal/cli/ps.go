package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wes/jokku/internal/types"
)

var psCommands = []*Command{
	{Name: "ps:scale", Help: "Get or set how many instances of each process type run", App: NeedsApp, Args: "[<type>=<count> ...]", MaxArgs: -1,
		Flags: []Flag{{Name: "skip-deploy", Help: "Only record the new scale; apply it on the next deploy"}}, Run: psScale},
	{Name: "ps:report", Help: "Display process information", App: OptionalApp, AnyFlags: true, Run: psReport},
	{Name: "ps:restart", Help: "Restart an app, applying config, scale and resource changes", App: NeedsApp, Run: psAction("restart")},
	{Name: "ps:start", Help: "Start a stopped app", App: NeedsApp, Run: psAction("start")},
	{Name: "ps:stop", Help: "Stop an app's instances", App: NeedsApp, Run: psAction("stop")},
	{Name: "ps:rebuild", Help: "Build and deploy the last pushed source again", App: NeedsApp, Run: psAction("rebuild")},
	{Name: "logs", Help: "Display recent log output", App: NeedsApp, Flags: []Flag{
		{Name: "tail", Short: "t", Help: "Keep following new output"},
		{Name: "num", Short: "n", Value: "N", Help: "Number of lines to show (default 100)"},
		{Name: "ps", Short: "p", Value: "PROCESS", Help: "Only this process type (web) or instance (web.1)"},
		{Name: "quiet", Short: "q", Help: "Print only the messages, without timestamps and names"},
	}, Run: logs},
	{Name: "releases", Help: "List an app's releases", App: NeedsApp, Run: releases},
}

func psAction(action string) func(*Context) error {
	return func(c *Context) error { return c.ps(action) }
}

// ps runs a ps action on the server and prints its progress.
func (c *Context) ps(action string) error {
	return c.API.PS(c, c.App, action, func(e types.Event) { fmt.Fprintln(c.Stdout, e.Message) })
}

// restartIfDeployed applies settings changes to a running app; before the
// first deploy there is nothing to restart.
func (c *Context) restartIfDeployed() error {
	app, err := c.API.App(c, c.App)
	if err != nil {
		return err
	}
	if app.CurrentRelease == 0 || app.Stopped {
		return nil
	}
	return c.ps("restart")
}

func logs(c *Context) error {
	o := types.LogOptions{Tail: 100, Follow: c.Bool("tail"), Process: c.String("ps")}
	if c.Bool("num") {
		n, err := strconv.Atoi(c.String("num"))
		if err != nil || n < 0 {
			return usageErr("Invalid --num %q", c.String("num"))
		}
		o.Tail = n
	}
	return c.API.Logs(c, c.App, o, func(e types.Event) {
		line := e.Message
		if c.Bool("quiet") {
			if _, msg, ok := strings.Cut(line, "]: "); ok {
				line = msg
			}
		}
		fmt.Fprintln(c.Stdout, line)
	})
}

func releases(c *Context) error {
	rels, err := c.API.Releases(c, c.App)
	if err != nil {
		return err
	}
	c.Header("%s releases", c.App)
	for _, r := range rels {
		current := ""
		if r.Current {
			current = "  (current)"
		}
		fmt.Fprintf(c.Stdout, "v%-4d %s  %s%s\n", r.Version, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Description, current)
	}
	return nil
}

var processTypeFlag = Flag{Name: "process-type", Value: "TYPE", Help: "Apply to one process type instead of the whole app"}

var resourceCommands = []*Command{
	{Name: "resource:limit", Help: "Set (or show) vCPUs and memory per instance; applied with a rolling restart", App: NeedsApp, Flags: []Flag{
		processTypeFlag,
		{Name: "cpu", Value: "N", Help: "vCPUs per instance"},
		{Name: "memory", Value: "SIZE", Help: "Memory per instance: 512, 512m, 1g (plain numbers are megabytes)"},
	}, Run: resourceLimit},
	{Name: "resource:limit-clear", Help: "Clear vCPU and memory overrides", App: NeedsApp, Flags: []Flag{processTypeFlag}, Run: resourceLimitClear},
	{Name: "resource:report", Help: "Display resource information", App: OptionalApp, AnyFlags: true, Run: resourceReport},
}

func psScale(c *Context) error {
	if len(c.Args) == 0 {
		procs, err := c.API.Formation(c, c.App)
		if err != nil {
			return err
		}
		c.Header("Scaling for %s", c.App)
		fmt.Fprintln(c.Stdout, "proctype: qty")
		fmt.Fprintln(c.Stdout, "--------: ---")
		for _, p := range procs {
			fmt.Fprintf(c.Stdout, "%s:  %d\n", p.Type, p.Quantity)
		}
		return nil
	}
	q := map[string]int{}
	for _, arg := range c.Args {
		proc, n, ok := strings.Cut(arg, "=")
		count, err := strconv.Atoi(n)
		if !ok || err != nil || count < 0 {
			return usageErr("Invalid scale %q, expected <type>=<count>", arg)
		}
		q[proc] = count
	}
	c.Step("Scaling %s processes: %s", c.App, strings.Join(c.Args, " "))
	if _, err := c.API.Scale(c, c.App, types.ScaleRequest{Quantities: q}); err != nil {
		return err
	}
	if c.Bool("skip-deploy") {
		return nil
	}
	return c.restartIfDeployed()
}

func psReport(c *Context) error {
	return forEachApp(c, func(a *types.App) error {
		procs, err := c.API.Formation(c, a.Name)
		if err != nil {
			return err
		}
		ps, err := c.API.Properties(c, a.Name, "ps")
		if err != nil {
			return err
		}
		insts, err := c.API.Instances(c, a.Name)
		if err != nil {
			return err
		}
		var scale []string
		for _, p := range procs {
			scale = append(scale, fmt.Sprintf("%s=%d", p.Type, p.Quantity))
		}
		running := 0
		var status []row
		for _, in := range insts {
			if in.Desired != "running" {
				continue // retiring after a deploy
			}
			if in.State == "healthy" {
				running++
			}
			status = append(status, row{"status-" + in.Name, "Status " + in.Name,
				fmt.Sprintf("%s (v%d, %s:%d, %d vCPU, %s, %d restarts)", in.State, in.Release, in.IP, in.Port, in.CPUs, formatMemory(in.MemoryMB), in.Restarts)})
		}
		return c.report(a.Name+" ps information", append([]row{
			{"deployed", "Deployed", yesNo(a.CurrentRelease > 0)},
			{"processes", "Processes", strconv.Itoa(len(status))},
			{"running", "Running", yesNo(running > 0 && !a.Stopped)},
			{"ps-procfile-path", "Ps procfile path", ps.Computed["procfile-path"]},
			{"ps-restart-policy", "Ps restart policy", ps.Computed["restart-policy"]},
			{"ps-scale", "Ps scale", strings.Join(scale, " ")},
		}, status...))
	})
}

func resourceLimit(c *Context) error {
	l := types.ResourceLimits{ProcessType: c.String("process-type")}
	if c.Bool("cpu") {
		n, err := strconv.Atoi(c.String("cpu"))
		if err != nil || n < 1 {
			return usageErr("Invalid --cpu %q, expected a whole number of vCPUs", c.String("cpu"))
		}
		l.CPUs = &n
	}
	if c.Bool("memory") {
		mb, err := parseMemory(c.String("memory"))
		if err != nil {
			return usageErr("%v", err)
		}
		l.MemoryMB = &mb
	}
	if l.CPUs == nil && l.MemoryMB == nil {
		return resourceReport(c)
	}
	scope := "default"
	if l.ProcessType != "" {
		scope = l.ProcessType
	}
	c.Header("Setting resource limits for %s (%s)", c.App, scope)
	if l.CPUs != nil {
		c.Info("cpu: %d", *l.CPUs)
	}
	if l.MemoryMB != nil {
		c.Info("memory: %s", formatMemory(*l.MemoryMB))
	}
	if _, err := c.API.SetResources(c, c.App, l); err != nil {
		return err
	}
	return c.restartIfDeployed()
}

func resourceLimitClear(c *Context) error {
	zero := 0
	l := types.ResourceLimits{ProcessType: c.String("process-type"), CPUs: &zero, MemoryMB: &zero}
	c.Step("Clearing resource limits for %s", c.App)
	if _, err := c.API.SetResources(c, c.App, l); err != nil {
		return err
	}
	return c.restartIfDeployed()
}

func resourceReport(c *Context) error {
	return forEachApp(c, func(a *types.App) error {
		procs, err := c.API.Formation(c, a.Name)
		if err != nil {
			return err
		}
		res, err := c.API.Resources(c, a.Name)
		if err != nil {
			return err
		}
		rows := []row{
			{"resource-default-cpu", "Default cpu", orDefault(res.Default.CPUs, "1")},
			{"resource-default-memory", "Default memory", orDefaultMem(res.Default.MemoryMB, "256m")},
		}
		seen := map[string]bool{}
		for _, p := range procs {
			seen[p.Type] = true
			rows = append(rows,
				row{p.Type + "-cpu", p.Type + " cpu", strconv.Itoa(p.CPUs)},
				row{p.Type + "-memory", p.Type + " memory", formatMemory(p.MemoryMB)})
		}
		// Overrides for process types that have not been scaled yet.
		for _, t := range sortedKeys(res.Process) {
			if seen[t] {
				continue
			}
			size := res.Process[t]
			rows = append(rows,
				row{t + "-cpu", t + " cpu", orDefault(size.CPUs, "default")},
				row{t + "-memory", t + " memory", orDefaultMem(size.MemoryMB, "default")})
		}
		return c.report(a.Name+" resource information", rows)
	})
}

func orDefault(n int, def string) string {
	if n == 0 {
		return def
	}
	return strconv.Itoa(n)
}

func orDefaultMem(mb int, def string) string {
	if mb == 0 {
		return def
	}
	return formatMemory(mb)
}

// parseMemory reads sizes like 512, 512m, 512MB, 1g, 1.5G, 1t into megabytes.
func parseMemory(s string) (int, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.TrimSuffix(strings.TrimSuffix(v, "ib"), "b")
	mult := 1.0
	switch {
	case strings.HasSuffix(v, "t"):
		mult, v = 1024*1024, strings.TrimSuffix(v, "t")
	case strings.HasSuffix(v, "g"):
		mult, v = 1024, strings.TrimSuffix(v, "g")
	case strings.HasSuffix(v, "m"):
		v = strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "k"):
		mult, v = 1.0/1024, strings.TrimSuffix(v, "k")
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid memory size %q, expected e.g. 512, 512m or 1g", s)
	}
	return int(f * mult), nil
}

func formatMemory(mb int) string {
	if mb >= 1024 && mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + "g"
	}
	return strconv.Itoa(mb) + "m"
}
