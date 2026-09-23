.PHONY: test build fmt

test:
	go test -race ./...

build:
	mkdir -p dist
	go build -trimpath -o dist/sherpa ./cmd/sherpa

fmt:
	gofmt -w cmd internal
