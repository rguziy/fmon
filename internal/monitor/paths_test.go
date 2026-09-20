package monitor

import (
	"path/filepath"
	"testing"
)

func TestOverlaps(t *testing.T) {
	sep := string(filepath.Separator)
	p := func(parts ...string) string {
		out := sep
		for i, s := range parts {
			if i > 0 {
				out += sep
			}
			out += s
		}
		return out
	}
	tests := []struct {
		a, b string
		want bool
	}{
		{p("etc"), p("etc"), true},
		{p("etc"), p("etc", "ssh"), true},
		{p("etc", "ssh"), p("etc"), true},
		{p("etc"), p("etcd"), false}, // shared textual prefix only
		{p("etc", "ssh"), p("etc", "ssh2"), false},
		{sep, p("etc"), true}, // filesystem root contains everything
		{p("a", "b.txt"), p("a"), true},
		{p("a"), p("b"), false},
	}
	for _, tc := range tests {
		if got := overlaps(tc.a, tc.b); got != tc.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
