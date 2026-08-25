.PHONY: test build registry collector-image collector-smoke beta-smoke fmt

test:
	go test ./...

# Functional pass over the installed CLI. Override the binary under test with
# SHERPA_BIN; add SHERPA_BETA_ONLINE=1 for the live-registry suite.
beta-smoke:
	bash scripts/beta/run-all.sh

build:
	mkdir -p dist
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=arm64 go build -o dist/sherpa-darwin-arm64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=amd64 go build -o dist/sherpa-darwin-amd64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=linux GOARCH=amd64 go build -o dist/sherpa-linux-amd64 ./cmd/sherpa

registry:
	mkdir -p dist
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" go build -o dist/registry ./cmd/registry

collector-image:
	bash deploy/collector/smoke_build.sh --build-only sherpa-collector:local

collector-smoke:
	bash deploy/collector/smoke_build.sh

fmt:
	gofmt -w .
