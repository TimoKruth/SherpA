FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b AS go-build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out \
    && CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/collector ./cmd/collector \
    && CGO_ENABLED=0 GOFLAGS=-trimpath go test -c -tags=integration -o /out/collector-integration.test ./internal/collector

FROM python:3.13-slim-bookworm@sha256:9d7f287598e1a5a978c015ee176d8216435aaf335ed69ac3c38dd1bbb10e8d64 AS borg-build

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        curl \
        libssl-dev \
        libacl1-dev \
        liblz4-dev \
        libzstd-dev \
        libxxhash-dev \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/*
RUN python -m venv /opt/borg \
    && curl -fsSLo /tmp/borgbackup-1.4.5.tar.gz \
        https://files.pythonhosted.org/packages/source/b/borgbackup/borgbackup-1.4.5.tar.gz \
    && printf '%s  %s\n' \
        '4f9a5fe584c504b15485841236750dea16aa7cd2ddbc4a594e9d2ce5c49c4508' \
        '/tmp/borgbackup-1.4.5.tar.gz' | sha256sum -c - \
    && /opt/borg/bin/pip install --no-cache-dir /tmp/borgbackup-1.4.5.tar.gz \
    && /opt/borg/bin/borg --version | grep -Fx 'borg 1.4.5' \
    && rm -f /tmp/borgbackup-1.4.5.tar.gz
COPY --from=go-build /out/collector-integration.test /tmp/collector-integration.test
ARG INTEGRATION_NONCE=default
RUN test -n "$INTEGRATION_NONCE" \
    && groupadd --gid 10001 collector \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin collector \
    && mkdir /tmp/collector-home \
    && chown -R 10001:10001 /tmp/collector-home /tmp/collector-integration.test \
    && su -s /bin/sh collector -c \
        'HOME=/tmp/collector-home PATH=/opt/borg/bin:$PATH /tmp/collector-integration.test -test.v -test.run "^(TestBorgLocalRepositoryCreateAndExactPresence|TestCollectorEndToEndWithLocalBorgRepository)$"' \
    && rm -rf /tmp/collector-home /tmp/collector-integration.test \
    && find /opt/borg -type d -name __pycache__ -prune -exec rm -rf {} + \
    && find /opt/borg -type f -name '*.pyc' -delete

FROM python:3.13-slim-bookworm@sha256:9d7f287598e1a5a978c015ee176d8216435aaf335ed69ac3c38dd1bbb10e8d64 AS runtime

ARG SOURCE_REVISION
RUN test "${#SOURCE_REVISION}" -eq 40 \
    && case "$SOURCE_REVISION" in *[!0-9a-f]*) exit 1 ;; esac \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        openssh-client \
        libssl3 \
        libacl1 \
        liblz4-1 \
        libzstd1 \
        libxxhash0 \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* \
    && groupadd --gid 10001 collector \
    && useradd --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin collector \
    && mkdir -p /data/spool /data/state /data/borg/cache /data/borg/config /data/borg/security \
    && chown -R 10001:10001 /data \
    && chmod 0700 /data /data/spool /data/state /data/borg /data/borg/cache /data/borg/config /data/borg/security \
    && find / -xdev -type f -perm /6000 -exec chmod a-s {} +
COPY --from=borg-build /opt/borg /opt/borg
COPY --from=go-build /out/collector /usr/local/bin/collector

ENV PATH="/opt/borg/bin:$PATH" \
    BORG_CACHE_DIR=/data/borg/cache \
    BORG_CONFIG_DIR=/data/borg/config \
    BORG_SECURITY_DIR=/data/borg/security
LABEL org.opencontainers.image.source="https://github.com/TimoKruth/SherpA" \
      org.opencontainers.image.revision="$SOURCE_REVISION"

EXPOSE 8080
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/collector"]
CMD ["serve"]
