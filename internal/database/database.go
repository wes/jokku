// Package database describes the databases Jokku runs for apps: Postgres,
// MySQL and Redis. Each is a Jokku app of its own (kind postgres, mysql or
// redis) named <engine>-<name>, deployed from a compose file with one
// service, the official image and a data volume, reachable from other apps
// at <engine>-<name>.internal and from nowhere else. Its config vars hold
// its passwords and fill in the compose file; linking it to an app sets a
// connection URL on the app.
package database

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
)

// Engine is one kind of database.
type Engine struct {
	Name     string // postgres, mysql or redis
	Image    string // the default image, without a tag
	Version  string // its default tag
	Port     int
	MemoryMB int
	// EnvVar is the config var a link sets on the app.
	EnvVar string
	// Mount is where the data volume is in the VM.
	Mount string
	// secrets are the config vars generated at creation, each a password.
	secrets []string
	compose string
	url     func(host string, name string, vars map[string]string) string
	connect func(name string, vars map[string]string) ([]string, []string)
	export  func(name string, vars map[string]string) ([]string, []string)
	restore func(name string, vars map[string]string) ([]string, []string)
	// Dump is what an export is, for messages and file names.
	Dump string
}

// Config vars a database's compose file reads.
const (
	VarImage = "DB_IMAGE"
	VarName  = "DB_NAME"
)

var engines = map[string]Engine{
	"postgres": {
		Name: "postgres", Image: "postgres", Version: "17", Port: 5432, MemoryMB: 512, EnvVar: "DATABASE_URL",
		Mount: "/var/lib/postgresql/data", secrets: []string{"POSTGRES_PASSWORD"}, Dump: "a pg_dump archive (custom format)",
		// PGDATA is a directory inside the volume: initdb refuses the
		// volume's own root, which holds lost+found.
		compose: `services:
  postgres:
    image: ${DB_IMAGE}
    environment:
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: ${DB_NAME}
      PGDATA: /var/lib/postgresql/data/pgdata
    volumes: ["data:/var/lib/postgresql/data"]
    deploy:
      resources:
        limits:
          memory: %dM
volumes:
  data: {}
`,
		url: func(host, name string, vars map[string]string) string {
			return (&url.URL{Scheme: "postgres", User: url.UserPassword("postgres", vars["POSTGRES_PASSWORD"]),
				Host: fmt.Sprintf("%s:%d", host, 5432), Path: "/" + name}).String()
		},
		connect: func(name string, _ map[string]string) ([]string, []string) {
			return []string{"psql", "-U", "postgres", name}, nil
		},
		export: func(name string, _ map[string]string) ([]string, []string) {
			return []string{"pg_dump", "-U", "postgres", "--format=custom", "--no-owner", "--no-acl", name}, nil
		},
		restore: func(name string, _ map[string]string) ([]string, []string) {
			return []string{"pg_restore", "-U", "postgres", "-d", name, "--clean", "--if-exists", "--no-owner", "--no-acl", "--exit-on-error"}, nil
		},
	},
	"mysql": {
		Name: "mysql", Image: "mysql", Version: "8.4", Port: 3306, MemoryMB: 1024, EnvVar: "DATABASE_URL",
		Mount: "/var/lib/mysql", secrets: []string{"MYSQL_ROOT_PASSWORD", "MYSQL_PASSWORD"}, Dump: "SQL from mysqldump",
		// Its data is a directory inside the volume: mysqld won't initialize
		// the volume's own root, which holds lost+found.
		compose: `services:
  mysql:
    image: ${DB_IMAGE}
    command: ["mysqld", "--datadir=/var/lib/mysql/data"]
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD}
      MYSQL_DATABASE: ${DB_NAME}
      MYSQL_USER: mysql
      MYSQL_PASSWORD: ${MYSQL_PASSWORD}
    volumes: ["data:/var/lib/mysql"]
    deploy:
      resources:
        limits:
          memory: %dM
volumes:
  data: {}
`,
		url: func(host, name string, vars map[string]string) string {
			return (&url.URL{Scheme: "mysql", User: url.UserPassword("mysql", vars["MYSQL_PASSWORD"]),
				Host: fmt.Sprintf("%s:%d", host, 3306), Path: "/" + name}).String()
		},
		connect: func(name string, vars map[string]string) ([]string, []string) {
			return []string{"mysql", "-uroot", name}, []string{"MYSQL_PWD=" + vars["MYSQL_ROOT_PASSWORD"]}
		},
		export: func(name string, vars map[string]string) ([]string, []string) {
			return []string{"mysqldump", "-uroot", "--single-transaction", "--routines", "--triggers", "--set-gtid-purged=OFF", name},
				[]string{"MYSQL_PWD=" + vars["MYSQL_ROOT_PASSWORD"]}
		},
		restore: func(name string, vars map[string]string) ([]string, []string) {
			return []string{"mysql", "-uroot", name}, []string{"MYSQL_PWD=" + vars["MYSQL_ROOT_PASSWORD"]}
		},
	},
	"redis": {
		Name: "redis", Image: "redis", Version: "7", Port: 6379, MemoryMB: 256, EnvVar: "REDIS_URL",
		Mount: "/data", secrets: []string{"REDIS_PASSWORD"}, Dump: "an RDB snapshot",
		compose: `services:
  redis:
    image: ${DB_IMAGE}
    command: ["redis-server", "--appendonly", "yes", "--requirepass", "${REDIS_PASSWORD}"]
    volumes: ["data:/data"]
    deploy:
      resources:
        limits:
          memory: %dM
volumes:
  data: {}
`,
		url: func(host, _ string, vars map[string]string) string {
			return (&url.URL{Scheme: "redis", User: url.UserPassword("", vars["REDIS_PASSWORD"]),
				Host: fmt.Sprintf("%s:%d", host, 6379)}).String()
		},
		connect: func(_ string, vars map[string]string) ([]string, []string) {
			return []string{"redis-cli"}, []string{"REDISCLI_AUTH=" + vars["REDIS_PASSWORD"]}
		},
		export: func(_ string, vars map[string]string) ([]string, []string) {
			return []string{"redis-cli", "--rdb", "-"}, []string{"REDISCLI_AUTH=" + vars["REDIS_PASSWORD"]}
		},
	},
}

// Lookup finds an engine by name.
func Lookup(name string) (Engine, bool) {
	e, ok := engines[name]
	return e, ok
}

// Names lists the engines, sorted.
func Names() []string {
	var out []string
	for n := range engines {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`)

// CheckName refuses a database name that wouldn't make an app name and a
// database name both engines accept.
func CheckName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%q is not a database name: lowercase letters, digits and dashes, up to 42", name)
	}
	return nil
}

// AppName is the app a database runs as.
func (e Engine) AppName(name string) string { return e.Name + "-" + name }

// Host is where apps reach a database.
func (e Engine) Host(name string) string { return e.AppName(name) + ".internal" }

// Process is the database's process type in its app.
func (e Engine) Process() string { return e.Name }

// ImageRef is the image to run: image (default the engine's) at version
// (default the engine's).
func (e Engine) ImageRef(image, version string) string {
	if image == "" {
		image = e.Image
	}
	if version == "" {
		version = e.Version
	}
	return image + ":" + version
}

// Compose is the compose file a database is deployed from, with memoryMB
// for its VM.
func (e Engine) Compose(memoryMB int) string {
	if memoryMB <= 0 {
		memoryMB = e.MemoryMB
	}
	return fmt.Sprintf(e.compose, memoryMB)
}

// Vars are a new database's config vars: its image, name and fresh
// passwords.
func (e Engine) Vars(name, image string) map[string]string {
	vars := map[string]string{VarImage: image, VarName: name}
	for _, s := range e.secrets {
		vars[s] = password()
	}
	return vars
}

// URL is how a linked app connects.
func (e Engine) URL(name string, vars map[string]string) string {
	return e.url(e.Host(name), name, vars)
}

// ConnectCommand is the shell to run in the database's VM, and its
// environment (passwords go there, not in the arguments).
func (e Engine) ConnectCommand(name string, vars map[string]string) ([]string, []string) {
	return e.connect(name, vars)
}

// ExportCommand writes the database's contents to stdout.
func (e Engine) ExportCommand(name string, vars map[string]string) ([]string, []string) {
	return e.export(name, vars)
}

// ImportCommand reads an export from stdin; ok is false when the engine
// can't import that way.
func (e Engine) ImportCommand(name string, vars map[string]string) ([]string, []string, bool) {
	if e.restore == nil {
		return nil, nil, false
	}
	argv, env := e.restore(name, vars)
	return argv, env, true
}

func password() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
