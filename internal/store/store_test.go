package store

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

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

func TestVolumes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "db")

	v := &Volume{App: "db", Name: "data", Type: types.VolumeLocal, SizeMB: 1024}
	if err := s.CreateVolume(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateVolume(ctx, &Volume{App: "db", Name: "data", Type: types.VolumeLocal, SizeMB: 1}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate name: %v", err)
	}
	m := types.VolumeMount{ProcessType: "web", Path: "/data"}
	if err := s.AddVolumeMount(ctx, v.ID, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.Volume(ctx, "db", "data")
	if err != nil || got.State != VolumeNew || got.Node != "" || len(got.Mounts) != 1 || got.Mounts[0] != m {
		t.Fatalf("volume: %+v, %v", got, err)
	}

	// The first instance places it; later placements don't move it.
	s.PlaceVolume(ctx, v.ID, "n1")
	s.PlaceVolume(ctx, v.ID, "n2")
	if got, _ := s.VolumeByID(ctx, v.ID); got.Node != "n1" {
		t.Fatalf("placed on %s, want n1", got.Node)
	}
	// Its node reporting the disk makes it ready.
	if changed, err := s.ReportVolume(ctx, "n1", types.VolumeStatus{ID: v.ID, State: types.VolumeReady, UsedMB: 12}); err != nil || !changed {
		t.Fatalf("report: %v, %v", changed, err)
	}
	if got, _ := s.VolumeByID(ctx, v.ID); got.State != VolumeReady || got.UsedMB != 12 {
		t.Fatalf("after report: %+v", got)
	}
	// Another node's report about it means nothing.
	if changed, _ := s.ReportVolume(ctx, "n9", types.VolumeStatus{ID: v.ID, State: types.VolumeMissing}); changed {
		t.Fatal("a stranger's report changed the volume")
	}

	// A move: progress comes from the receiving node; a commit swaps homes
	// and keeps the old copy until the new node has the disk.
	if err := s.StartVolumeMove(ctx, v.ID, "n2", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartVolumeMove(ctx, v.ID, "n3", "tok2"); err == nil {
		t.Fatal("a second move started while one runs")
	}
	s.ReportVolume(ctx, "n2", types.VolumeStatus{ID: v.ID, State: types.VolumeSynced, CopiedMB: 7})
	if got, _ := s.VolumeByID(ctx, v.ID); got.Transfer != types.VolumeSynced || got.CopiedMB != 7 || got.MovingTo != "n2" {
		t.Fatalf("during move: %+v", got)
	}
	s.CommitVolumeMove(ctx, v.ID)
	got, _ = s.VolumeByID(ctx, v.ID)
	if got.Node != "n2" || got.PreviousNode != "n1" || got.Moving() || got.MoveToken != "" {
		t.Fatalf("after commit: %+v", got)
	}
	s.ReportVolume(ctx, "n2", types.VolumeStatus{ID: v.ID, State: types.VolumeReady})
	if got, _ := s.VolumeByID(ctx, v.ID); got.PreviousNode != "" {
		t.Fatalf("the old copy is still kept: %+v", got)
	}
	s.StartVolumeMove(ctx, v.ID, "n3", "tok3")
	s.AbortVolumeMove(ctx, v.ID, "")
	if got, _ := s.VolumeByID(ctx, v.ID); got.Node != "n2" || got.Moving() {
		t.Fatalf("after abort: %+v", got)
	}

	// Destroying: a never-used volume goes at once; a placed one waits for
	// its node to delete the disk, and its name is free meanwhile.
	unused := &Volume{App: "db", Name: "unused", Type: types.VolumeLocal, SizeMB: 64}
	s.CreateVolume(ctx, unused)
	s.DestroyVolume(ctx, unused.ID)
	if _, err := s.VolumeByID(ctx, unused.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unused volume after destroy: %v", err)
	}
	if err := s.DestroyVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.VolumeByID(ctx, v.ID)
	if got.State != VolumeDestroying || len(got.Mounts) != 0 {
		t.Fatalf("destroying: %+v", got)
	}
	if _, err := s.Volume(ctx, "db", "data"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a destroyed volume is still found by name: %v", err)
	}
	if err := s.CreateVolume(ctx, &Volume{App: "db", Name: "data", Type: types.VolumeLocal, SizeMB: 64}); err != nil {
		t.Fatalf("reusing the name: %v", err)
	}
	s.ReportVolume(ctx, "n2", types.VolumeStatus{ID: v.ID, State: types.VolumeDestroyed})
	if _, err := s.VolumeByID(ctx, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the node deleted the disk: %v", err)
	}
}

func TestDeletingAnAppDestroysItsVolumes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "db")
	v := &Volume{App: "db", Name: "data", Type: types.VolumeLocal, SizeMB: 64}
	s.CreateVolume(ctx, v)
	s.AddVolumeMount(ctx, v.ID, types.VolumeMount{ProcessType: "web", Path: "/data"})
	s.PlaceVolume(ctx, v.ID, "n1")
	if err := s.DeleteApp(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	vols, err := s.Volumes(ctx, "")
	if err != nil || len(vols) != 1 || vols[0].State != VolumeDestroying || vols[0].App != "" || vols[0].Node != "n1" {
		t.Fatalf("volumes after deleting the app: %+v, %v", vols, err)
	}
	// A new app by the same name doesn't inherit it.
	s.CreateApp(ctx, "db")
	if vols, _ := s.Volumes(ctx, "db"); len(vols) != 0 {
		t.Fatalf("the new app has %d volumes", len(vols))
	}
}

func TestInstanceVolumesAndNodeFeatures(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.CreateApp(ctx, "db")
	rel := &Release{App: "db", Processes: map[string][]string{"web": {"x"}}}
	if err := s.CreateRelease(ctx, rel); err != nil {
		t.Fatal(err)
	}
	in := &Instance{App: "db", ReleaseID: rel.ID, ProcessType: "web", Index: 1, Node: "n1", Desired: DesiredRunning,
		Volumes: []types.InstanceVolume{{ID: "abc", Path: "/data"}}}
	if err := s.CreateInstance(ctx, in, netip.MustParsePrefix("10.210.1.0/24")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Instance(ctx, in.ID)
	if err != nil || len(got.Volumes) != 1 || got.Volumes[0].ID != "abc" {
		t.Fatalf("instance volumes: %+v, %v", got, err)
	}

	s.RegisterControlNode(ctx, Node{Name: "n1"})
	s.NodeReported(ctx, "n1", "v1", "", []string{types.FeatureVolumes}, types.NodeMetrics{})
	if n, _ := s.Node(ctx, "n1"); !n.Has(types.FeatureVolumes) {
		t.Fatalf("features: %v", n.Features)
	}
	s.NodeReported(ctx, "n1", "v0", "", nil, types.NodeMetrics{})
	if n, _ := s.Node(ctx, "n1"); n.Has(types.FeatureVolumes) {
		t.Fatal("an older agent kept the newer one's features")
	}
}

// Migration 7 gathers the old builder settings (builder selected,
// builder-dockerfile, builder-compose, ps procfile-path) into the builder
// plugin, and makes apps last deployed from an image into image apps.
func TestBuilderSettingsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jokku.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db, now: time.Now}
	for _, m := range migrations[:6] {
		if _, err := db.ExecContext(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	db.ExecContext(ctx, "PRAGMA user_version = 6")
	for _, app := range []string{"web", "shop", "cache", "plain"} {
		// As that schema had them: CreateApp writes today's columns.
		if _, err := db.ExecContext(ctx, "INSERT INTO apps (name, created_at) VALUES (?, 0)", app); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range [][4]string{
		{"web", "builder-dockerfile", "dockerfile-path", "docker/prod.Dockerfile"},
		{"web", "ps", "procfile-path", "Procfile.prod"},
		{"web", "builder", "build-dir", "api"},
		{"shop", "builder", "selected", "compose"},
		{"shop", "builder-compose", "compose-file", "prod.yml"},
		{"shop", "builder-dockerfile", "dockerfile-path", "ignored"},
		{"cache", "builder-dockerfile", "dockerfile-path", "ignored"},
		{"", "builder-dockerfile", "dockerfile-path", "ignored"},
		{"", "ps", "procfile-path", "Procfile.all"},
	} {
		if err := old.SetProperty(ctx, p[0], p[1], p[2], p[3]); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range [][3]string{
		{"cache", "git", "succeeded"}, {"cache", "image", "succeeded"}, {"cache", "git", "failed"},
		{"plain", "image", "succeeded"}, {"plain", "git", "succeeded"},
	} {
		dep, err := old.CreateDeploy(ctx, d[0], d[1], "redis:7", "me")
		if err != nil {
			t.Fatal(err)
		}
		old.SetDeployStatus(ctx, dep.ID, d[2], "")
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for app, want := range map[string]map[string]string{
		"web":   {"file": "docker/prod.Dockerfile", "procfile": "Procfile.prod", "build-dir": "api"},
		"shop":  {"type": "compose", "file": "prod.yml"},
		"cache": {"type": "image", "image": "redis:7"},
		"plain": {},
		"":      {"procfile": "Procfile.all"},
	} {
		got, err := s.Properties(ctx, app, "builder")
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("%q builder properties = %v, %v; want %v", app, got, err, want)
		}
	}
	for _, plugin := range []string{"builder-dockerfile", "builder-compose"} {
		if got, _ := s.Properties(ctx, "web", plugin); len(got) != 0 {
			t.Errorf("%s left behind: %v", plugin, got)
		}
	}
	if got, _ := s.Properties(ctx, "web", "ps"); len(got) != 0 {
		t.Errorf("ps properties left behind: %v", got)
	}
}
