package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

var sshKeysCommands = []*Command{
	{Name: "ssh-keys:add", Help: "Add a public key (from a file or stdin) for git push and remote commands", Args: "<name> [<key-file>]", MinArgs: 1, MaxArgs: 2, Run: sshKeysAdd},
	{Name: "ssh-keys:remove", Help: "Remove a key by name, or by fingerprint with --fingerprint", Args: "<name>|<fingerprint>", MinArgs: 1,
		Flags: []Flag{{Name: "fingerprint", Help: "The argument is a key fingerprint"}}, Run: sshKeysRemove},
	{Name: "ssh-keys:list", Help: "List keys", Flags: []Flag{{Name: "format", Value: "FORMAT", Help: "text (default) or json"}}, Run: sshKeysList},
}

func sshKeysAdd(c *Context) error {
	name := c.Args[0]
	var data []byte
	var err error
	if len(c.Args) == 2 {
		data, err = os.ReadFile(c.Args[1])
	} else {
		if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			return usageErr("Pass a key file, or pipe the key in: cat ~/.ssh/id_ed25519.pub | jokku ssh-keys:add %s", name)
		}
		data, err = io.ReadAll(io.LimitReader(c.Stdin, 64<<10))
	}
	if err != nil {
		return err
	}
	key, err := c.API.AddSSHKey(c, name, string(data))
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, key.Fingerprint)
	return nil
}

func sshKeysRemove(c *Context) error {
	if err := c.API.RemoveSSHKey(c, c.Args[0], c.Bool("fingerprint")); err != nil {
		return err
	}
	c.Step("Removed SSH key %s", c.Args[0])
	return nil
}

func sshKeysList(c *Context) error {
	keys, err := c.API.SSHKeys(c)
	if err != nil {
		return err
	}
	switch c.String("format") {
	case "json":
		return json.NewEncoder(c.Stdout).Encode(keys)
	case "", "text":
	default:
		return usageErr("Unknown format %q", c.String("format"))
	}
	if len(keys) == 0 {
		c.Warn("No public keys found")
		return nil
	}
	for _, k := range keys {
		fmt.Fprintf(c.Stdout, "%s NAME=%q\n", k.Fingerprint, k.Name)
	}
	return nil
}
