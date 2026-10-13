package database

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// HasUsers says whether Jokku manages the engine's logins (db:<engine>:users).
// Redis keeps users only in a file its server is started with, which an
// existing database's isn't.
func (e Engine) HasUsers() bool { return e.Name == "postgres" || e.Name == "mysql" }

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,31}$`)

// CheckUser refuses a name that isn't a new login's: one the engine keeps
// for itself, or the database's own (which its apps use).
func (e Engine) CheckUser(user string) error {
	if !userRe.MatchString(user) {
		return fmt.Errorf("%q is not a user name: lowercase letters, digits and underscores, up to 32, not starting with a digit", user)
	}
	switch {
	case e.Name == "postgres" && (user == "postgres" || strings.HasPrefix(user, "pg_")):
		return fmt.Errorf("%s is Postgres's own user", user)
	case e.Name == "mysql" && (user == "root" || user == "mysql"):
		return fmt.Errorf("%s is the database's own user", user)
	}
	return nil
}

// UsersCommand lists the database's logins, for ParseUsers.
func (e Engine) UsersCommand(name string, vars map[string]string) Command {
	if e.Name == "mysql" {
		return Command{Argv: []string{"mysql", "-uroot", "-N", "-B", "--raw"}, Env: mysqlEnv(vars),
			Stdin: fmt.Sprintf(mysqlUsers, sqlString(name))}
	}
	return Command{Argv: psql(name), Stdin: postgresUsers}
}

const postgresUsers = `SELECT json_build_object('users', (SELECT coalesce(json_agg(u), '[]') FROM (
  SELECT rolname AS name, rolsuper AS super,
    pg_has_role(oid, 'pg_read_all_data', 'MEMBER') AS can_read,
    pg_has_role(oid, 'pg_write_all_data', 'MEMBER') AS can_write
  FROM pg_roles WHERE rolcanlogin) u));
`

const mysqlUsers = `SELECT JSON_OBJECT('users', (SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('name', u.User, 'host', u.Host, 'super', u.Super_priv = 'Y',
    'all', COALESCE(d.Create_priv = 'Y' AND d.Alter_priv = 'Y' AND d.Drop_priv = 'Y', 0),
    'can_read', u.Select_priv = 'Y' OR COALESCE(d.Select_priv = 'Y', 0),
    'can_write', u.Insert_priv = 'Y' OR COALESCE(d.Insert_priv = 'Y' AND d.Update_priv = 'Y' AND d.Delete_priv = 'Y', 0))), JSON_ARRAY())
  FROM mysql.user u LEFT JOIN mysql.db d ON d.User = u.User AND d.Host = u.Host AND d.Db = %s
  WHERE u.User NOT LIKE 'mysql.%%'));
`

// ParseUsers reads UsersCommand's output.
func (e Engine) ParseUsers(out []byte) ([]User, error) {
	if e.Name == "mysql" {
		var st myStats
		if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
			return nil, fmt.Errorf("reading MySQL's users: %w", err)
		}
		return st.Users.list(), nil
	}
	var st pgStats
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return nil, fmt.Errorf("reading Postgres's users: %w", err)
	}
	var users []User
	for _, u := range st.Users {
		users = append(users, User{Name: u.Name, Access: accessOf(u.Super, u.CanRead, u.CanWrite)})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Name < users[j].Name })
	return users, nil
}

// accessOf names a login's access from what it may do.
func accessOf(super, read, write bool) string {
	switch {
	case super:
		return AccessAll
	case read && write:
		return AccessReadWrite
	case read:
		return AccessReadOnly
	}
	return AccessOther
}

// AddUserCommand creates a login that reads (and unless readOnly, writes)
// the database's data, but can't change its schema: Postgres's
// pg_read_all_data and pg_write_all_data roles, MySQL's SELECT, INSERT,
// UPDATE and DELETE on its tables. The password goes in on stdin, never
// in arguments.
func (e Engine) AddUserCommand(name string, vars map[string]string, user, password string, readOnly bool) Command {
	if e.Name == "mysql" {
		grant := "SELECT, INSERT, UPDATE, DELETE"
		if readOnly {
			grant = "SELECT"
		}
		return Command{Argv: []string{"mysql", "-uroot", "-N", "-B"}, Env: mysqlEnv(vars),
			Stdin: fmt.Sprintf("CREATE USER %s@'%%' IDENTIFIED BY %s;\nGRANT %s ON %s.* TO %s@'%%';\n",
				sqlString(user), sqlString(password), grant, mysqlIdent(name), sqlString(user))}
	}
	script := fmt.Sprintf("BEGIN;\nCREATE ROLE %s LOGIN PASSWORD %s;\nGRANT pg_read_all_data TO %[1]s;\n", pgIdent(user), sqlString(password))
	if !readOnly {
		script += fmt.Sprintf("GRANT pg_write_all_data TO %s;\n", pgIdent(user))
	}
	return Command{Argv: psql(name), Stdin: script + "COMMIT;\n"}
}

// RemoveUserCommand drops a login and ends its sessions; the objects it
// owned (Postgres) go to the database's own user.
func (e Engine) RemoveUserCommand(name string, vars map[string]string, user string) Command {
	if e.Name == "mysql" {
		u, host, ok := strings.Cut(user, "@")
		if !ok {
			host = "%"
		}
		// DROP USER leaves its sessions running: they are killed after it,
		// from the list it printed.
		return Command{Argv: []string{"sh", "-c", `set -e
kills=$(mysql -uroot -N -B --raw -e "$1")
[ -z "$kills" ] || mysql -uroot -N -B --force -e "$kills" 2>/dev/null || true`, "sh",
			fmt.Sprintf("DROP USER %s@%s; SELECT GROUP_CONCAT(CONCAT('KILL ', ID, ';') SEPARATOR ' ') FROM performance_schema.processlist WHERE USER = %[1]s HAVING COUNT(*) > 0;",
				sqlString(u), sqlString(host))},
			Env: mysqlEnv(vars)}
	}
	return Command{Argv: psql(name), Stdin: fmt.Sprintf(`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = %s;
REASSIGN OWNED BY %s TO postgres;
DROP OWNED BY %[2]s;
DROP ROLE %[2]s;
`, sqlString(user), pgIdent(user))}
}

// UserURL is how user connects to the database at addr (host:port).
func (e Engine) UserURL(name, user, password, addr string) string {
	return (&url.URL{Scheme: e.Name, User: url.UserPassword(user, password), Host: addr, Path: "/" + name}).String()
}

// UserURLs are user's URLs: the one its apps use, and, when the database is
// exposed at host:port, the one from outside.
func (e Engine) UserURLs(name, user, password, host string, port int) (internal, public string) {
	internal = e.UserURL(name, user, password, net.JoinHostPort(e.Host(name), strconv.Itoa(e.Port)))
	if host != "" {
		public = e.UserURL(name, user, password, net.JoinHostPort(host, strconv.Itoa(port)))
		if e.Name == "postgres" {
			public += "?sslmode=require"
		}
	}
	return internal, public
}

// NewPassword is a fresh password for a login.
func NewPassword() string { return password() }

func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func mysqlIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// PasswordCommand gives a login a new password.
func (e Engine) PasswordCommand(name string, vars map[string]string, user, password string) Command {
	if e.Name == "mysql" {
		u, host, ok := strings.Cut(user, "@")
		if !ok {
			host = "%"
		}
		return Command{Argv: []string{"mysql", "-uroot", "-N", "-B"}, Env: mysqlEnv(vars),
			Stdin: fmt.Sprintf("ALTER USER %s@%s IDENTIFIED BY %s;\n", sqlString(u), sqlString(host), sqlString(password))}
	}
	return Command{Argv: psql(name), Stdin: fmt.Sprintf("ALTER ROLE %s PASSWORD %s;\n", pgIdent(user), sqlString(password))}
}

// errPrefix is how psql and mysql start an error: where in the script it
// came from.
var errPrefix = regexp.MustCompile(`(?m)^(psql:<stdin>:\d+: |ERROR \d+ \([0-9A-Z]+\) at line \d+: )`)

// Refusal is what a database said when it refused a command, without
// its client's prefixes.
func Refusal(stderr string) string {
	return strings.TrimSpace(errPrefix.ReplaceAllString(stderr, ""))
}
