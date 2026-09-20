package monitor

import "testing"

func TestExcluder(t *testing.T) {
	e := NewExcluder([]string{"*.tmp", ".DS_Store", "**/.git/**", "/var/log/**"})
	tests := []struct {
		path string
		want bool
	}{
		{"/home/u/a.tmp", true},
		{"/home/u/a.txt", false},
		{"/home/u/.DS_Store", true},
		{"/home/u/repo/.git/config", true},
		{"/home/u/repo/.git/objects/ab/cd", true},
		{"/home/u/repo/src/main.go", false},
		{"/var/log/syslog", true},
		{"/var/log/nginx/access.log", true},
		{"/var/lib/x", false},
		{"/home/u/tmp/keep", false}, // "*.tmp" must not match a name that merely contains "tmp"
	}
	for _, tc := range tests {
		if got := e.Match(tc.path); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if NewExcluder(nil).Match("/anything") {
		t.Error("empty exclude list must match nothing")
	}
}
