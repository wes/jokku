package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/wes/jokku/internal/cluster"
	"github.com/wes/jokku/internal/database"
	"github.com/wes/jokku/internal/types"
)

// usersPlugin keeps the passwords of the logins Jokku made in a database
// (db:<engine>:users:add, :passwd), by user, so their URLs can be shown
// again. The database itself is where the users are.
const usersPlugin = "db-users"

func (s *Server) databaseStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Cluster.DatabaseStats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// runInDatabase runs cmd in db's database, and says why it failed in the
// database's words.
func (s *Server) runInDatabase(ctx context.Context, e database.Engine, db *types.App, cmd database.Command) ([]byte, error) {
	name := strings.TrimPrefix(db.Name, e.Name+"-")
	out, err := s.Cluster.RunInDatabase(ctx, db.Name, e, cmd)
	var refused *cluster.ExecError
	switch {
	case errors.Is(err, cluster.ErrNotRunning):
		return nil, httpErrorf(http.StatusConflict, "%s database %s isn't running (jokku db:%s:start %s)", e.Name, name, e.Name, name)
	case errors.As(err, &refused):
		return nil, httpErrorf(http.StatusConflict, "%s", database.Refusal(refused.Stderr))
	}
	return out, err
}

// usersDatabase is the database a users request is for, refusing an engine
// whose users Jokku doesn't manage.
func (s *Server) usersDatabase(ctx context.Context, r *http.Request) (database.Engine, *types.App, map[string]string, error) {
	e, db, err := s.lookupDatabase(ctx, r.PathValue("engine"), r.PathValue("name"))
	if err != nil {
		return e, nil, nil, err
	}
	if !e.HasUsers() {
		return e, nil, nil, badRequest("a %s database has one password, the one in its URL (jokku db:%s:info %s --dsn): Jokku doesn't manage users for it",
			e.Name, e.Name, r.PathValue("name"))
	}
	vars, err := s.Store.ConfigVars(ctx, db.Name)
	return e, db, vars, err
}

// checkUser refuses a user that isn't one Jokku may change: a database's
// own (which its apps use) or the engine's.
func checkUser(e database.Engine, user string) error {
	name, _, _ := strings.Cut(user, "@")
	if err := e.CheckUser(name); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// userView is how a client sees user: with its password and URLs, when
// Jokku made it.
func (s *Server) userView(ctx context.Context, e database.Engine, db *types.App, u database.User, password string) (types.DatabaseUser, error) {
	v := types.DatabaseUser{Name: u.Name, Access: u.Access}
	if password == "" {
		return v, nil
	}
	exp, err := cluster.Exposures(ctx, s.Store, []types.App{*db})
	if err != nil {
		return v, err
	}
	x := exp[db.Name]
	v.Password = password
	v.URL, v.PublicURL = e.UserURLs(strings.TrimPrefix(db.Name, e.Name+"-"), u.Name, password, x.Host, x.Port)
	return v, nil
}

func (s *Server) listDatabaseUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, db, vars, err := s.usersDatabase(ctx, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.runInDatabase(ctx, e, db, e.UsersCommand(strings.TrimPrefix(db.Name, e.Name+"-"), vars))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, err := e.ParseUsers(out)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	passwords, err := s.Store.Properties(ctx, db.Name, usersPlugin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	users := []types.DatabaseUser{}
	for _, u := range list {
		v, err := s.userView(ctx, e, db, u, passwords[u.Name])
		if err != nil {
			s.fail(w, r, err)
			return
		}
		users = append(users, v)
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) addDatabaseUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, db, vars, err := s.usersDatabase(ctx, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var req types.AddDatabaseUserRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := e.CheckUser(req.Name); err != nil {
		s.fail(w, r, badRequest("%v", err))
		return
	}
	name := strings.TrimPrefix(db.Name, e.Name+"-")
	password := database.NewPassword()
	if _, err := s.runInDatabase(ctx, e, db, e.AddUserCommand(name, vars, req.Name, password, req.ReadOnly)); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, db.Name, usersPlugin, map[string]string{req.Name: password}); err != nil {
		s.fail(w, r, err)
		return
	}
	access := database.AccessReadWrite
	if req.ReadOnly {
		access = database.AccessReadOnly
	}
	s.Store.AddEvent(ctx, "app", db.Name, "", "%s added %s user %s to %s database %s", actor(r), access, req.Name, e.Name, name)
	v, err := s.userView(ctx, e, db, database.User{Name: req.Name, Access: access}, password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) databaseUserPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, db, vars, err := s.usersDatabase(ctx, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	user := r.PathValue("user")
	if err := checkUser(e, user); err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.TrimPrefix(db.Name, e.Name+"-")
	password := database.NewPassword()
	if _, err := s.runInDatabase(ctx, e, db, e.PasswordCommand(name, vars, user, password)); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, db.Name, usersPlugin, map[string]string{user: password}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", db.Name, "", "%s gave user %s of %s database %s a new password", actor(r), user, e.Name, name)
	// What the user may do is unchanged, and listed by users:list.
	v, err := s.userView(ctx, e, db, database.User{Name: user}, password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) removeDatabaseUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e, db, vars, err := s.usersDatabase(ctx, r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	user := r.PathValue("user")
	if err := checkUser(e, user); err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.TrimPrefix(db.Name, e.Name+"-")
	if _, err := s.runInDatabase(ctx, e, db, e.RemoveUserCommand(name, vars, user)); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetProperties(ctx, db.Name, usersPlugin, map[string]string{user: ""}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Store.AddEvent(ctx, "app", db.Name, "", "%s removed user %s from %s database %s", actor(r), user, e.Name, name)
	w.WriteHeader(http.StatusNoContent)
}
