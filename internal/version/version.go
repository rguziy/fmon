// Package version exposes the fmon release version.
package version

// Version is the fmon release version.
//
// The value below is the version of this source tree. Release builds may
// override it at link time, which is what the Makefile does:
//
//	go build -ldflags "-X github.com/rguziy/fmon/internal/version.Version=1.0.0"
//
// It must stay a variable (not a constant) for -X to work.
var Version = "1.0.0"
