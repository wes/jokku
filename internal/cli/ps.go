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
	_, err := c.API.Scale(c, c.App, types.ScaleRequest{Quantities: q, SkipDeploy: c.Bool("skip-deploy")})
	return err
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
		var scale []string
		for _, p := range procs {
			scale = append(scale, fmt.Sprintf("%s=%d", p.Type, p.Quantity))
		}
		return c.report(a.Name+" ps information", []row{
			{"deployed", "Deployed", yesNo(a.CurrentRelease > 0)},
			{"ps-procfile-path", "Ps procfile path", ps.Computed["procfile-path"]},
			{"ps-restart-policy", "Ps restart policy", ps.Computed["restart-policy"]},
			{"ps-scale", "Ps scale", strings.Join(scale, " ")},
		})
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
	_, err := c.API.SetResources(c, c.App, l)
	return err
}

func resourceLimitClear(c *Context) error {
	zero := 0
	l := types.ResourceLimits{ProcessType: c.String("process-type"), CPUs: &zero, MemoryMB: &zero}
	c.Step("Clearing resource limits for %s", c.App)
	_, err := c.API.SetResources(c, c.App, l)
	return err
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

// parseMemory reads sizes like 512, 512m, 512MB, 1g, 1.5G into megabytes.
func parseMemory(s string) (int, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.TrimSuffix(strings.TrimSuffix(v, "ib"), "b")
	mult := 1.0
	switch {
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
