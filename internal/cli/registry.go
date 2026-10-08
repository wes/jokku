package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var registryCommands = []*Command{
	{Name: "registry:login", Help: "Log in to a private registry so deploys can pull its images (password from stdin if not given)",
		Args: "<server> <username> [<password>]", MinArgs: 2, MaxArgs: 3, Flags: []Flag{
			{Name: "password-stdin", Help: "Read the password from stdin"},
			{Name: "global", Help: "Accepted for Dokku compatibility: logins always apply to every app"},
		}, Run: registryLogin},
	{Name: "registry:logout", Help: "Forget a registry login", Args: "<server>", MinArgs: 1, Run: registryLogout},
	{Name: "registry:report", Help: "Display registry logins", Run: registryReport},
}

func registryLogin(c *Context) error {
	server, user := c.Args[0], c.Args[1]
	password := ""
	if len(c.Args) == 3 {
		password = c.Args[2]
	} else {
		b, err := io.ReadAll(io.LimitReader(c.Stdin, 64<<10))
		if err != nil {
			return err
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	if password == "" {
		return usageErr("No password given: pass it as an argument or on stdin (--password-stdin)")
	}
	if err := c.API.SetRegistryLogin(c, server, user, password); err != nil {
		return err
	}
	c.Step("Logged in to %s as %s", server, user)
	return nil
}

func registryLogout(c *Context) error {
	if err := c.API.DeleteRegistryLogin(c, c.Args[0]); err != nil {
		return err
	}
	c.Step("Logged out of %s", c.Args[0])
	return nil
}

func registryReport(c *Context) error {
	logins, err := c.API.RegistryLogins(c)
	if err != nil {
		return err
	}
	c.Header("Registry logins")
	if len(logins) == 0 {
		c.Info("none (add one with jokku registry:login <server> <username>)")
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVER\tUSERNAME\tSINCE")
	for _, l := range logins {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", l.Server, l.Username, l.CreatedAt.Local().Format("2006-01-02"))
	}
	return tw.Flush()
}
