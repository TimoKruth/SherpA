.PHONY: test build fmt

test:
	go test ./...

build:
	mkdir -p dist
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=arm64 go build -o dist/sherpa-darwin-arm64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=darwin GOARCH=amd64 go build -o dist/sherpa-darwin-amd64 ./cmd/sherpa
	CGO_ENABLED=0 GOFLAGS="$(GOFLAGS) -trimpath" GOOS=linux GOARCH=amd64 go build -o dist/sherpa-linux-amd64 ./cmd/sherpa

fmt:
	gofmt -w .
