#==============================================================#
# File      :   Makefile
# Mtime     :   2025-08-14
# License   :   Apache-2.0 @ https://github.com/pgsty/pg_exporter
# Copyright :   2018-2026  Ruohang Feng / Vonng (rh@vonng.com)
#==============================================================#
VERSION      ?= v1.6.0
DOCKER_REPO  ?= pgsty/pg_exporter
BUILD_DATE   := $(shell date '+%Y%m%d%H%M%S')
GIT_BRANCH   := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
GIT_REVISION := $(shell git rev-parse --short HEAD 2>/dev/null  || echo "HEAD")
LDFLAGS_META := -X 'pg_exporter/exporter.Version=$(VERSION)' \
                -X 'pg_exporter/exporter.Branch=$(GIT_BRANCH)' \
                -X 'pg_exporter/exporter.Revision=$(GIT_REVISION)' \
                -X 'pg_exporter/exporter.BuildDate=$(BUILD_DATE)'
LDFLAGS_STATIC := -s -w -extldflags \"-static\" $(LDFLAGS_META)

# Release Dir
LINUX_AMD_DIR:=dist/$(VERSION)/pg_exporter-$(VERSION).linux-amd64
LINUX_ARM_DIR:=dist/$(VERSION)/pg_exporter-$(VERSION).linux-arm64
DARWIN_AMD_DIR:=dist/$(VERSION)/pg_exporter-$(VERSION).darwin-amd64
DARWIN_ARM_DIR:=dist/$(VERSION)/pg_exporter-$(VERSION).darwin-arm64


###############################################################
#                        Shortcuts                            #
###############################################################
build:
	go build -ldflags "$(LDFLAGS_META)" -o pg_exporter
clean:
	rm -rf pg_exporter
build-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -a -ldflags "$(LDFLAGS_STATIC)" -o pg_exporter
build-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -a -ldflags "$(LDFLAGS_STATIC)" -o pg_exporter
build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -a -ldflags "$(LDFLAGS_STATIC)" -o pg_exporter
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -a -ldflags "$(LDFLAGS_STATIC)" -o pg_exporter

r: release
release: release-linux release-darwin

release-linux: linux-amd64 linux-arm64
linux-amd64: clean build-linux-amd64
	rm -rf $(LINUX_AMD_DIR) && mkdir -p $(LINUX_AMD_DIR)
	nfpm package --packager rpm --config package/nfpm-amd64-rpm.yaml --target dist/$(VERSION)
	nfpm package --packager deb --config package/nfpm-amd64-deb.yaml --target dist/$(VERSION)
	cp pg_exporter $(LINUX_AMD_DIR)/pg_exporter
	cp pg_exporter.yml $(LINUX_AMD_DIR)/pg_exporter.yml
	cp LICENSE $(LINUX_AMD_DIR)/LICENSE
	tar -czf dist/$(VERSION)/pg_exporter-$(VERSION).linux-amd64.tar.gz -C dist/$(VERSION) pg_exporter-$(VERSION).linux-amd64
	rm -rf $(LINUX_AMD_DIR)

linux-arm64: clean build-linux-arm64
	rm -rf $(LINUX_ARM_DIR) && mkdir -p $(LINUX_ARM_DIR)
	nfpm package --packager rpm --config package/nfpm-arm64-rpm.yaml --target dist/$(VERSION)
	nfpm package --packager deb --config package/nfpm-arm64-deb.yaml --target dist/$(VERSION)
	cp pg_exporter $(LINUX_ARM_DIR)/pg_exporter
	cp pg_exporter.yml $(LINUX_ARM_DIR)/pg_exporter.yml
	cp LICENSE $(LINUX_ARM_DIR)/LICENSE
	tar -czf dist/$(VERSION)/pg_exporter-$(VERSION).linux-arm64.tar.gz -C dist/$(VERSION) pg_exporter-$(VERSION).linux-arm64
	rm -rf $(LINUX_ARM_DIR)

release-darwin: darwin-amd64 darwin-arm64
darwin-amd64: clean build-darwin-amd64
	rm -rf $(DARWIN_AMD_DIR) && mkdir -p $(DARWIN_AMD_DIR)
	cp pg_exporter $(DARWIN_AMD_DIR)/pg_exporter
	cp pg_exporter.yml $(DARWIN_AMD_DIR)/pg_exporter.yml
	cp LICENSE $(DARWIN_AMD_DIR)/LICENSE
	tar -czf dist/$(VERSION)/pg_exporter-$(VERSION).darwin-amd64.tar.gz -C dist/$(VERSION) pg_exporter-$(VERSION).darwin-amd64
	rm -rf $(DARWIN_AMD_DIR)

darwin-arm64: clean build-darwin-arm64
	rm -rf $(DARWIN_ARM_DIR) && mkdir -p $(DARWIN_ARM_DIR)
	cp pg_exporter $(DARWIN_ARM_DIR)/pg_exporter
	cp pg_exporter.yml $(DARWIN_ARM_DIR)/pg_exporter.yml
	cp LICENSE $(DARWIN_ARM_DIR)/LICENSE
	tar -czf dist/$(VERSION)/pg_exporter-$(VERSION).darwin-arm64.tar.gz -C dist/$(VERSION) pg_exporter-$(VERSION).darwin-arm64
	rm -rf $(DARWIN_ARM_DIR)



###############################################################
#                      Configuration                          #
###############################################################
# generate merged config from separated configuration
conf:
	rm -rf pg_exporter.yml
	cat config/*.yml >> pg_exporter.yml

# generate legacy merged config for PostgreSQL 9.1 - 9.6
conf9:
	rm -rf legacy/pg_exporter.yml
	cat legacy/config/*.yml >> legacy/pg_exporter.yml

# Backward-compatible alias (deprecated)
conf-pg9: conf9


###############################################################
#                         Release                             #
###############################################################
release-dir:
	mkdir -p dist/$(VERSION)

release-clean:
	rm -rf dist/$(VERSION)

###############################################################
#                      GoReleaser                            #
###############################################################
# Install goreleaser if not present
goreleaser-install:
	@which goreleaser > /dev/null || (echo "Installing goreleaser..." && go install github.com/goreleaser/goreleaser/v2@latest)

# Build snapshot release (without publishing)
goreleaser-snapshot: goreleaser-install
	goreleaser release --snapshot --clean --skip=publish

# Build release locally (without git tag)
goreleaser-build: goreleaser-install
	goreleaser build --snapshot --clean

# Build release locally without snapshot suffix (requires clean git)
goreleaser-local: goreleaser-install
	goreleaser release --clean --skip=publish

# Release with goreleaser (requires git tag)
goreleaser-release: goreleaser-install
	goreleaser release --clean

# Test release (creates prerelease, no notifications)
goreleaser-test-release: goreleaser-install
	@echo "Creating test release (prerelease mode, no notifications)..."
	goreleaser release --clean

# Production release (set prerelease to false in config first)
goreleaser-prod-release: goreleaser-install
	@echo "Creating production release (will notify subscribers if announce.skip is false)..."
	goreleaser release --clean

# Check goreleaser configuration
goreleaser-check: goreleaser-install
	goreleaser check

# New main release task using goreleaser
release-new: goreleaser-release


# Build a local image from the canonical Dockerfile.
# Multi-architecture release images are built by GoReleaser.
docker: docker-build
docker-build:
	docker build -f Dockerfile \
		--build-arg GOPROXY=$${GOPROXY:-https://proxy.golang.org,direct} \
		--build-arg GOSUMDB=$${GOSUMDB:-sum.golang.org} \
		-t $(DOCKER_REPO):$(VERSION)-dev \
		-t $(DOCKER_REPO):dev .

###############################################################
#                         Develop                             #
###############################################################
install: build
	sudo install -m 0755 pg_exporter /usr/bin/pg_exporter

uninstall:
	sudo rm -rf /usr/bin/pg_exporter

runb:
	./pg_exporter --log.level=info --config=pg_exporter.yml --auto-discovery
run:
	go run main.go --log.level=info --config=pg_exporter.yml --auto-discovery

debug:
	go run main.go --log.level=debug --config=pg_exporter.yml --auto-discovery

curl:
	curl localhost:9630/metrics | grep -v '#' | grep pg_

upload:
	./upload.sh

d: dev
dev:
	hugo serve

.PHONY: build clean build-darwin-amd64 build-darwin-arm64 build-linux-amd64 build-linux-arm64 \
 r release release-darwin release-linux linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 \
 docker docker-build \
 install uninstall debug curl upload \
 goreleaser-install goreleaser-snapshot goreleaser-build goreleaser-release goreleaser-test-release \
 goreleaser-check release-new goreleaser-local
