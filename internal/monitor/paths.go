package monitor

import (
	"errors"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// normalizePath returns the absolute, cleaned form of p.
func normalizePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func equalPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether child lies strictly inside parent.
func within(child, parent string) bool {
	sep := string(filepath.Separator)
	prefix := strings.TrimRight(parent, sep) + sep // "/" trims to "" and becomes "/"
	if len(child) <= len(prefix) {
		return false
	}
	return equalPath(child[:len(prefix)], prefix)
}

// overlaps reports whether a and b are the same path or one contains the other.
func overlaps(a, b string) bool {
	return equalPath(a, b) || within(a, b) || within(b, a)
}

// isNotExist reports whether err means "the path is gone". ENOTDIR covers the
// case where a parent directory was replaced by a file.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
