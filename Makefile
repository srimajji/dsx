GO ?= go
VERSION ?= dev
COMMIT ?= unknown
BUILT_AT ?= unknown
LDFLAGS := -s -w -X github.com/srimajji/dsx/internal/buildinfo.Version=$(VERSION) -X github.com/srimajji/dsx/internal/buildinfo.Commit=$(COMMIT) -X github.com/srimajji/dsx/internal/buildinfo.BuiltAt=$(BUILT_AT)

export VERSION COMMIT SOURCE_DATE_EPOCH DSX_AGENT_IMAGE DSX_BROWSER_IMAGE SYFT_BIN OUT_DIR
export SIGNING_IDENTITY NOTARY_KEYCHAIN_PROFILE SIGNED_OUT EXPECTED_VERSION EXPECTED_COMMIT

.PHONY: build build-host build-guest test clean release-tools release-dry-run release-sign release-verify release-smoke

build: build-host build-guest

build-host:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dsx ./cmd/dsx

build-guest:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dsx-guest ./cmd/dsx-guest

release-tools:
	./scripts/release/install-syft.sh

release-dry-run:
	./scripts/release/build.sh

release-sign:
	./scripts/release/sign-notarize.sh "$(UNSIGNED_DIR)"

release-verify:
	./scripts/release/verify.sh --package "$(PACKAGE_DIR)" --notarization-result "$(NOTARIZATION_RESULT)"

release-smoke:
	./scripts/release/smoke-install.sh "$(PACKAGE_DIR)" "$(NOTARIZATION_RESULT)" "$(PREFIX)"

test:
	$(GO) test ./...

clean:
	rm -rf bin
