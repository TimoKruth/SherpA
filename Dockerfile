FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/registry ./cmd/registry

FROM postgres:16-bookworm

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        git \
        util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --create-home --home-dir /home/sherpa --shell /usr/sbin/nologin sherpa

COPY --from=build /out/registry /usr/local/bin/registry
COPY --chmod=0755 deploy/entrypoint.sh /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
