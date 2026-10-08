package store

import "testing"

func TestReleasePort(t *testing.T) {
	for vars, want := range map[string]int{"": 3000, "8080": 8080, "nope": 3000, "0": 3000, "70000": 3000} {
		r := &Release{Image: ImageConfig{Port: 3000}, ConfigVars: map[string]string{}}
		if vars != "" {
			r.ConfigVars["PORT"] = vars
		}
		if got := r.Port(); got != want {
			t.Errorf("PORT=%q: %d, want %d", vars, got, want)
		}
	}
}
