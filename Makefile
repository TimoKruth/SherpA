.PHONY: test build registry collector-image collector-smoke fmt

test:
	go test ./...

build:
	mkdir -p dist
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=arm64 go build -o dist/sherpa-darwin-arm64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=amd64 go build -o dist/sherpa-darwin-amd64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=linux GOARCH=amd64 go build -o dist/sherpa-linux-amd64 ./cmd/sherpa

registry:
	mkdir -p dist
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" go build -o dist/registry ./cmd/registry

collector-image:
	docker build --file deploy/collector/Dockerfile --build-arg SOURCE_REVISION="$$(git rev-parse HEAD)" --tag sherpa-collector:local .

collector-smoke:
	bash deploy/collector/smoke_build.sh

fmt:
	gofmt -w .
