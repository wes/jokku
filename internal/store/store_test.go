package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wes/jokku/internal/types"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestApps(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	if _, err := s.CreateApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx, "web"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: got %v, want ErrExists", err)
	}
	if _, err := s.App(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app: got %v, want ErrNotFound", err)
	}
	if err := s.RenameApp(ctx, "web", "site"); err != nil {
		t.Fatal(err)
	}
	apps, err := s.Apps(ctx)
	if err != nil || len(apps) != 1 || apps[0].Name != "site" {
		t.Fatalf("apps after rename = %+v, %v", apps, err)
	}
}

func TestConfigVarsScopesAndChanges(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "web")

	vars, changed, err := s.UpdateConfigVars(ctx, "web", types.ConfigPatch{Set: map[string]string{"A": "1", "B": "2"}})
	if err != nil || !changed || len(vars) != 2 {
		t.Fatalf("set: vars=%v changed=%v err=%v", vars, changed, err)
	}
	if _, changed, _ := s.UpdateConfigVars(ctx, "web", types.ConfigPatch{Set: map[string]string{"A": "1"}}); changed {
		t.Fatal("setting the same value should not count as a change")
	}
	vars, _, _ = s.UpdateConfigVars(ctx, "web", types.ConfigPatch{Unset: []string{"A"}})
	if _, ok := vars["A"]; ok || vars["B"] != "2" {
		t.Fatalf("after unset: %v", vars)
	}
	if _, _, err := s.UpdateConfigVars(ctx, "", types.ConfigPatch{Set: map[string]string{"G": "x"}}); err != nil {
		t.Fatal(err)
	}
	global, _ := s.ConfigVars(ctx, "")
	app, _ := s.ConfigVars(ctx, "web")
	if global["G"] != "x" || app["G"] != "" {
		t.Fatalf("global var leaked: global=%v app=%v", global, app)
	}
}

func TestDomainsAreUniqueAcrossApps(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "a")
	s.CreateApp(ctx, "b")

	if _, err := s.UpdateDomains(ctx, "a", types.DomainsPatch{Add: []string{"a.com", "www.a.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDomains(ctx, "b", types.DomainsPatch{Add: []string{"a.com"}}); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	// The same domain may be global and on an app.
	if _, err := s.UpdateDomains(ctx, "", types.DomainsPatch{Add: []string{"a.com"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UpdateDomains(ctx, "a", types.DomainsPatch{Set: []string{"new.com"}})
	if len(got) != 1 || got[0] != "new.com" {
		t.Fatalf("set replaced to %v", got)
	}
}

func TestResourcesEffective(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "web")
	two, gig := 2, 1024

	if err := s.SetResources(ctx, "web", types.ResourceLimits{MemoryMB: &gig}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetResources(ctx, "web", types.ResourceLimits{ProcessType: "worker", CPUs: &two}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Resources(ctx, "web")
	if got := res.Effective("web"); got.CPUs != DefaultCPUs || got.MemoryMB != 1024 {
		t.Fatalf("web = %+v", got)
	}
	if got := res.Effective("worker"); got.CPUs != 2 || got.MemoryMB != 1024 {
		t.Fatalf("worker = %+v", got)
	}
	zero := 0
	s.SetResources(ctx, "web", types.ResourceLimits{ProcessType: "worker", CPUs: &zero})
	res, _ = s.Resources(ctx, "web")
	if _, ok := res.Process["worker"]; ok {
		t.Fatal("clearing every field should remove the override row")
	}
}

func TestNewAppsStartWithoutLetsEncrypt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	enabled := func(app string) string {
		t.Helper()
		p, err := s.Properties(ctx, app, "letsencrypt")
		if err != nil {
			t.Fatal(err)
		}
		return p["enabled"]
	}
	s.CreateApp(ctx, "new")
	if got := enabled("new"); got != "false" {
		t.Errorf("a new app has letsencrypt enabled=%q, want false", got)
	}
	// Turned on for one app, a clone keeps it on.
	s.SetProperty(ctx, "new", "letsencrypt", "enabled", "true")
	s.CloneApp(ctx, "new", "copy")
	if got := enabled("copy"); got != "true" {
		t.Errorf("a clone of an app with letsencrypt on has enabled=%q, want true", got)
	}
	// Turned on with --global, new apps follow the global setting.
	s.SetProperty(ctx, "", "letsencrypt", "enabled", "true")
	s.CreateApp(ctx, "after")
	if got := enabled("after"); got != "" {
		t.Errorf("with letsencrypt on globally, a new app has its own enabled=%q, want none", got)
	}
}

func TestCloneAndDelete(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "src")
	s.UpdateConfigVars(ctx, "src", types.ConfigPatch{Set: map[string]string{"K": "v"}})
	s.UpdateDomains(ctx, "src", types.DomainsPatch{Add: []string{"src.com"}})
	s.Scale(ctx, "src", map[string]int{"web": 3})
	s.SetProperty(ctx, "src", "git", "deploy-branch", "prod")

	if err := s.CloneApp(ctx, "src", "dst"); err != nil {
		t.Fatal(err)
	}
	vars, _ := s.ConfigVars(ctx, "dst")
	doms, _ := s.Domains(ctx, "dst")
	form, _ := s.Formation(ctx, "dst")
	props, _ := s.Properties(ctx, "dst", "git")
	if vars["K"] != "v" || len(doms) != 0 || len(form) != 1 || form[0].Quantity != 3 || props["deploy-branch"] != "prod" {
		t.Fatalf("clone: vars=%v domains=%v formation=%v props=%v", vars, doms, form, props)
	}

	if err := s.DeleteApp(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	// The deleted app's domain is free again.
	if _, err := s.UpdateDomains(ctx, "dst", types.DomainsPatch{Add: []string{"src.com"}}); err != nil {
		t.Fatal(err)
	}
}

func TestDeploys(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "web")
	d, err := s.CreateDeploy(ctx, "web", "git", "abc123", "admin")
	if err != nil || d.Status != "pending" || d.App != "web" {
		t.Fatalf("create deploy = %+v, %v", d, err)
	}
	s.SetDeployStatus(ctx, d.ID, types.StatusFailed, "boom")
	list, _ := s.Deploys(ctx, "web", 10)
	if len(list) != 1 || list[0].Status != types.StatusFailed || list[0].FinishedAt == nil {
		t.Fatalf("deploys = %+v", list)
	}
}
