// Package sshkeys turns stored SSH keys into the jokku user's
// authorized_keys, where every key is bound to "jokku ssh-command".
package sshkeys

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/wes/jokku/internal/types"
)

// ValidName matches key names that are safe to embed in a forced command
// without quoting.
var ValidName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// Parse validates a single public key line and returns it normalized (type and
// base64 only, no comment or options) along with its SHA256 fingerprint.
func Parse(line string) (normalized, fingerprint string, err error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", fmt.Errorf("public key is empty")
	}
	if strings.Count(line, "\n") > 0 {
		return "", "", fmt.Errorf("expected exactly one public key, got several lines")
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", "", fmt.Errorf(`invalid public key: expected one line like "ssh-ed25519 AAAA... you@laptop" (the .pub file)`)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub), nil
}

// Render builds the authorized_keys content. exe is the absolute path of the
// jokku binary on the server.
func Render(exe string, keys []types.SSHKey) []byte {
	var b bytes.Buffer
	b.WriteString("# Managed by jokku. Edit with: jokku ssh-keys:add / ssh-keys:remove\n")
	for _, k := range keys {
		// restrict disables forwarding and the like; pty is re-enabled for
		// interactive commands such as "jokku run app bash".
		fmt.Fprintf(&b, "restrict,pty,command=\"%s ssh-command --key-name %s\" %s %s\n", exe, k.Name, k.PublicKey, k.Name)
	}
	return b.Bytes()
}

// Owner is who should own written files; nil leaves ownership alone.
type Owner struct{ UID, GID int }

// Write atomically replaces path with the rendered keys.
func Write(path, exe string, keys []types.SSHKey, owner *Owner) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".authorized_keys-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(Render(exe, keys)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if owner != nil {
		if err := os.Chown(dir, owner.UID, owner.GID); err != nil {
			return err
		}
		if err := os.Chown(tmp.Name(), owner.UID, owner.GID); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), path)
}
