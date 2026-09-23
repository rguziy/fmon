# fmon build automation.
#
#   make                 show the list of targets (same as "make list")
#   make build           build for the host into dist/fmon
#   make all             cross-compile every target, create zip archives and SHA256SUMS
#   make linux-armv7     build a single target (see "make list")
#   make test            run vet and the test suite
#   make VERSION=1.0.1   override the version embedded into the binary
#
# All builds are static, pure Go (CGO_ENABLED=0). Archives are created with
# the "zip" tool, which must be installed.

MODULE  := github.com/rguziy/fmon
VERSION ?= 1.1.0
DIST    := dist
LDFLAGS := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

export CGO_ENABLED := 0

TARGETS := linux-amd64 linux-arm64 linux-armv5 linux-armv6 linux-armv7 \
           windows-amd64 darwin-amd64 darwin-arm64

.DEFAULT_GOAL := list

.PHONY: list help build all test vet checksums clean package $(TARGETS)

list help:
	@echo "fmon $(VERSION) - make targets"
	@echo ""
	@echo "  list, help      Show this list (default target)"
	@echo "  build           Build for this host into $(DIST)/fmon"
	@echo "  all             Cross-compile every target below, create zip archives and SHA256SUMS"
	@echo "  test            Run go vet and go test"
	@echo "  vet             Run go vet"
	@echo "  checksums       Write $(DIST)/SHA256SUMS for the archives of VERSION"
	@echo "  clean           Remove $(DIST)/"
	@echo ""
	@echo "Cross-compile a single target (zip archive in $(DIST)/):"
	@for t in $(TARGETS); do echo "  $$t"; done
	@echo ""
	@echo "Override the version with: make VERSION=x.y.z <target>"

build:
	@mkdir -p $(DIST)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/fmon ./cmd/fmon

all: $(TARGETS) checksums

vet:
	go vet ./...

test: vet
	go test ./...

# Each platform target re-invokes make with the platform variables set.
linux-amd64:   ; @$(MAKE) --no-print-directory package GOOS=linux   GOARCH=amd64 SUFFIX=linux_amd64
linux-arm64:   ; @$(MAKE) --no-print-directory package GOOS=linux   GOARCH=arm64 SUFFIX=linux_arm64
linux-armv5:   ; @$(MAKE) --no-print-directory package GOOS=linux   GOARCH=arm GOARM=5 SUFFIX=linux_armv5
linux-armv6:   ; @$(MAKE) --no-print-directory package GOOS=linux   GOARCH=arm GOARM=6 SUFFIX=linux_armv6
linux-armv7:   ; @$(MAKE) --no-print-directory package GOOS=linux   GOARCH=arm GOARM=7 SUFFIX=linux_armv7
windows-amd64: ; @$(MAKE) --no-print-directory package GOOS=windows GOARCH=amd64 SUFFIX=windows_amd64
darwin-amd64:  ; @$(MAKE) --no-print-directory package GOOS=darwin  GOARCH=amd64 SUFFIX=darwin_amd64
darwin-arm64:  ; @$(MAKE) --no-print-directory package GOOS=darwin  GOARCH=arm64 SUFFIX=darwin_arm64

EXE     = $(if $(filter windows,$(GOOS)),.exe,)
ARCHIVE = $(DIST)/fmon_$(VERSION)_$(SUFFIX).zip
STAGE   = $(DIST)/.stage/$(SUFFIX)

# Internal: build one platform and wrap binary + README + LICENSE in a zip.
# The plain binary is also kept in dist/bin/ (used by CI smoke tests).
package:
	@test -n "$(GOOS)" || { echo "use one of: $(TARGETS)"; exit 1; }
	@echo "==> $(GOOS)/$(GOARCH)$(if $(GOARM), v$(GOARM)) $(VERSION)"
	@rm -rf "$(STAGE)" "$(ARCHIVE)"
	@mkdir -p "$(STAGE)" "$(DIST)/bin"
	GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM=$(GOARM) go build -trimpath -ldflags "$(LDFLAGS)" -o "$(STAGE)/fmon$(EXE)" ./cmd/fmon
	@cp "$(STAGE)/fmon$(EXE)" "$(DIST)/bin/fmon_$(SUFFIX)$(EXE)"
	@cp README.md LICENSE "$(STAGE)/"
	@cd "$(STAGE)" && zip -q -9 "$(CURDIR)/$(ARCHIVE)" "fmon$(EXE)" README.md LICENSE
	@rm -rf "$(STAGE)"
	@rmdir "$(DIST)/.stage" 2>/dev/null || true

checksums:
	@cd $(DIST) && { sha256sum fmon_$(VERSION)_*.zip 2>/dev/null || shasum -a 256 fmon_$(VERSION)_*.zip; } > SHA256SUMS
	@echo "==> $(DIST)/SHA256SUMS"

clean:
	rm -rf $(DIST)
