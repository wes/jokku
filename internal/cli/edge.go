package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

var edgeCommands = []*Command{
	{Name: "edge:add", Help: "Add an edge: a public server that routes your domains to the nodes behind it, which dial it (no port opened at home)",
		Args: "[<user>@]<address>", MinArgs: 1, Flags: []Flag{
			{Name: "name", Value: "NAME", Help: "The edge's name (default edge1, edge2, ...)"},
			{Name: "print", Help: "Print the command to run on the edge instead of running it over ssh"},
		}, Run: edgeAdd},
	{Name: "edge:list", Help: "List the edges and whether they're connected", Run: edgeList},
	{Name: "edge:remove", Help: "Remove an edge: it stops routing, and the nodes stop dialing it", Args: "<name>", MinArgs: 1,
		Flags: []Flag{forceFlag}, Run: edgeRemove},
}

var externalCommands = []*Command{
	{Name: "external:create", Help: "Route a domain to a service Jokku doesn't run, such as Home Assistant on your network",
		Args: "<name> <url>", MinArgs: 2, Flags: []Flag{
			{Name: "via", Value: "NODE", Help: "The node on the service's network that edges reach it through (default the control node)"},
			{Name: "insecure", Help: "For an https URL: don't check the service's certificate (self-signed, say)"},
		}, Run: externalCreate},
	{Name: "external:list", Help: "List the external apps", Run: externalList},
	{Name: "external:info", Help: "Display an external app's target, domains and login", Args: "<name>", MinArgs: 1, AnyFlags: true, Run: externalInfo},
	{Name: "external:set", Help: "Change an external app's url, via or insecure", Args: "<name> <url|via|insecure> <value>", MinArgs: 3, Run: externalSet},
	{Name: "external:destroy", Help: "Stop routing to an external app and delete it", Args: "<name>", MinArgs: 1, Flags: []Flag{forceFlag}, Run: externalDestroy},
}

func init() {
	namespaceHelp["edge"] = "Route your domains through public servers to nodes behind NAT"
	namespaceHelp["external"] = "Route domains to services Jokku doesn't run, on your network"
}

func edgeAdd(c *Context) error {
	user, address := "", c.Args[0]
	if u, a, ok := strings.Cut(address, "@"); ok {
		user, address = u, a
	}
	if c.Bool("print") && user != "" {
		return usageErr("--print prints the command instead of logging in, so give only the address")
	}
	name := c.String("name")
	if name == "" {
		edges, err := c.API.Edges(c)
		if err != nil {
			return err
		}
		name = nextEdgeName(edges)
	}
	edge, err := c.API.CreateEdge(c, types.CreateEdgeRequest{Name: name, Address: address})
	if err != nil {
		return err
	}
	c.Step("Added edge %s at %s", name, address)
	if c.Bool("print") {
		c.Header("Run this on %s, as root or with sudo:", address)
		fmt.Fprintln(c.Stdout, edge.Command)
		edgeNextSteps(c, name)
		return nil
	}
	if user == "" {
		user = "root"
	}
	c.Step("Installing it over ssh as %s@%s", user, address)
	args := []string{"-o", "ConnectTimeout=15"}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		args = append(args, "-t") // so sudo can ask for a password
	}
	args = append(args, user+"@"+address, remoteInstall(edge.Bundle, edge.Version, user != "root"))
	ssh := exec.Command("ssh", args...)
	ssh.Stdin, ssh.Stdout, ssh.Stderr = os.Stdin, c.Stdout, c.Stderr
	if err := ssh.Run(); err != nil {
		c.Warn("Installing over ssh failed: %v", err)
		c.Warn("Run this on %s instead (as root or with sudo):", address)
		fmt.Fprintln(c.Stdout, edge.Command)
		c.Warn("or remove the edge with: jokku edge:remove %s", name)
		return &exitError{1}
	}
	edgeNextSteps(c, name)
	return nil
}

func edgeNextSteps(c *Context, name string) {
	c.Info("The nodes dial it on UDP port 51820: open that, and TCP 80 and 443, in its firewall.")
	c.Info("Then point your domains' DNS at it, and see it with: jokku edge:list")
}

// remoteInstall is the installer command for an edge, honoring a mirror of
// the installer and binaries set here (JOKKU_INSTALL_URL,
// JOKKU_DOWNLOAD_URL).
func remoteInstall(bundle, ver string, sudo bool) string {
	script := os.Getenv("JOKKU_INSTALL_URL")
	if script == "" {
		script = "https://raw.githubusercontent.com/" + version.Repo + "/main/install.sh"
	}
	env := []string{}
	if strings.HasPrefix(ver, "v") {
		env = append(env, "JOKKU_VERSION="+ver)
	}
	if d := os.Getenv("JOKKU_DOWNLOAD_URL"); d != "" {
		env = append(env, "JOKKU_DOWNLOAD_URL="+shellQuote(d))
	}
	run := "env " + strings.Join(append(env, "sh -s -- --edge "+bundle), " ")
	if sudo {
		run = "sudo " + run
	}
	return "curl -fsSL " + shellQuote(script) + " | " + run
}

func nextEdgeName(edges []types.Edge) string {
	taken := map[string]bool{}
	for _, e := range edges {
		taken[e.Name] = true
	}
	for i := 1; ; i++ {
		if n := "edge" + strconv.Itoa(i); !taken[n] {
			return n
		}
	}
}

func edgeList(c *Context) error {
	st, err := c.API.ClusterStatus(c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tADDRESS\tMESH IP\tLAST SEEN\tVERSION")
	n := 0
	for _, e := range st.Nodes {
		if e.Role != types.RoleEdge {
			continue
		}
		n++
		status := "connected"
		switch {
		case e.LastSeen.IsZero():
			status = "not installed yet"
		case e.Status == "down":
			status = "disconnected"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, status, e.Address, e.MeshIP, ago(e.LastSeen), e.Version)
	}
	if n == 0 {
		fmt.Fprintln(c.Stdout, "No edges yet. Add one with: jokku edge:add root@<public server>")
		return nil
	}
	return tw.Flush()
}

func edgeRemove(c *Context) error {
	name := c.Args[0]
	if !c.Bool("force") {
		if err := c.confirm(fmt.Sprintf("remove edge %s: domains pointing at it stop working", name), name); err != nil {
			return err
		}
	}
	if err := c.API.RemoveEdge(c, name); err != nil {
		return err
	}
	c.Step("Removed edge %s", name)
	c.Info("Its proxy stops routing; uninstall jokku there or reuse the server.")
	return nil
}

// External apps

func externalCreate(c *Context) error {
	req := types.ExternalRequest{Name: c.Args[0], URL: c.Args[1], Via: c.String("via")}
	if c.Bool("insecure") {
		t := true
		req.Insecure = &t
	}
	e, err := c.API.CreateExternal(c, req)
	if err != nil {
		return err
	}
	c.Step("Created %s, routing to %s through %s", e.Name, e.URL, e.Via)
	if len(e.Domains) > 0 {
		c.Info("Domains: %s", strings.Join(e.Domains, " "))
	} else {
		c.Info("Give it a domain with: jokku domains:add %s <domain>", e.Name)
	}
	c.Info("Turn on TLS with: jokku letsencrypt:enable %s; a login with: jokku http-auth:enable %s", e.Name, e.Name)
	return nil
}

func externalList(c *Context) error {
	list, err := c.API.Externals(c)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(c.Stdout, "No external apps yet. Create one with: jokku external:create <name> http://192.168.1.50:8123")
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tURL\tVIA\tLOGIN\tDOMAINS")
	for _, e := range list {
		login := e.Auth
		if login == "" {
			login = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.URL, e.Via, login, strings.Join(e.Domains, " "))
	}
	return tw.Flush()
}

func externalInfo(c *Context) error {
	e, err := c.API.External(c, c.Args[0])
	if err != nil {
		return err
	}
	login := e.Auth
	if login == "" {
		login = "none"
	}
	return c.report(e.Name+" external app information", []row{
		{"external-url", "External url", e.URL},
		{"external-via", "External via", e.Via},
		{"external-insecure", "External insecure", yesNo(e.Insecure)},
		{"external-domains", "External domains", strings.Join(e.Domains, " ")},
		{"external-login", "External login", login},
	})
}

func externalSet(c *Context) error {
	name, key, value := c.Args[0], c.Args[1], c.Args[2]
	var req types.ExternalRequest
	switch key {
	case "url":
		req.URL = value
	case "via":
		req.Via = value
	case "insecure":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return usageErr("insecure is true or false")
		}
		req.Insecure = &b
	default:
		return usageErr("Unknown setting %q, expected url, via or insecure", key)
	}
	if _, err := c.API.PatchExternal(c, name, req); err != nil {
		return err
	}
	c.Step("Set %s %s to %s", name, key, value)
	return nil
}

func externalDestroy(c *Context) error {
	name := c.Args[0]
	if !c.Bool("force") {
		if err := c.confirm(fmt.Sprintf("stop routing to %s and delete its settings", name), name); err != nil {
			return err
		}
	}
	if err := c.API.DestroyExternal(c, name); err != nil {
		return err
	}
	c.Step("Destroyed %s", name)
	return nil
}
