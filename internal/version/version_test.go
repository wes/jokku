package version

import "testing"

func TestRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.8.5": true, "v1.0.0": true, "v0.10.12": true,
		"0.8.5": false, "v0.8": false, "v0.8.5-3-g1a2b3c4": false, "v0.8.5-dirty": false,
		"0.0.0-dev": false, "dev": false, "": false, "v0.08.5": false, "v0.8.x": false,
	} {
		if got := Release(v); got != want {
			t.Errorf("Release(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.8.4", "v0.8.5", true},
		{"v0.8.9", "v0.8.10", true},
		{"v0.9.0", "v0.10.0", true},
		{"v0.8.5", "v1.0.0", true},
		{"v0.8.5", "v0.8.5", false},
		{"v0.8.6", "v0.8.5", false},
		{"v0.8.4-2-gabc", "v0.8.5", false}, // development builds are left alone
		{"v0.8.4", "0.0.0-dev", false},
		{"", "v0.8.5", false},
	} {
		if got := Older(c.a, c.b); got != c.want {
			t.Errorf("Older(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
