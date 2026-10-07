package client

import "testing"

func TestParseGitRemote(t *testing.T) {
	tests := []struct {
		in, dest, app string
		ok            bool
	}{
		{"jokku@203.0.113.10:myapp", "jokku@203.0.113.10", "myapp", true},
		{"jokku@host:myapp.git\n", "jokku@host", "myapp", true},
		{"ssh://jokku@host:2222/myapp", "jokku@host:2222", "myapp", true},
		{"ssh://jokku@host/myapp.git", "jokku@host", "myapp", true},
		{"https://github.com/org/repo.git", "", "", false},
		{"git@github.com:org/repo.git", "", "", false},
		{"/local/path", "", "", false},
	}
	for _, tt := range tests {
		dest, app, ok := ParseGitRemote(tt.in)
		if dest != tt.dest || app != tt.app || ok != tt.ok {
			t.Errorf("ParseGitRemote(%q) = %q, %q, %v; want %q, %q, %v", tt.in, dest, app, ok, tt.dest, tt.app, tt.ok)
		}
	}
}
