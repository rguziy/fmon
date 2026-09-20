package monitor

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Excluder matches paths against the global exclude patterns of fmon.toml.
//
// A pattern without a "/" is matched against the base name of the entry
// ("*.tmp" excludes every .tmp file); a pattern containing "/" is matched
// against the full absolute path using doublestar globbing, where "**"
// spans directories ("**/.git/**", "/var/log/**"). Matching always uses
// forward slashes, also on Windows.
type Excluder struct {
	patterns []string
}

// NewExcluder creates an Excluder. Invalid patterns are ignored here because
// config.Validate rejects them at load time.
func NewExcluder(patterns []string) *Excluder {
	return &Excluder{patterns: patterns}
}

// Match reports whether the absolute path p is excluded.
func (e *Excluder) Match(p string) bool {
	if e == nil || len(e.patterns) == 0 {
		return false
	}
	slash := filepath.ToSlash(p)
	base := path.Base(slash)
	for _, pat := range e.patterns {
		target := base
		if strings.Contains(pat, "/") {
			target = slash
		}
		if ok, err := doublestar.Match(pat, target); err == nil && ok {
			return true
		}
	}
	return false
}
