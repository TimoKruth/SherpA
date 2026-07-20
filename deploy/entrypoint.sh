#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "entrypoint must run as root to prepare the content volume" >&2
  exit 1
fi

SHERPA_CONTENT_DIR="${SHERPA_CONTENT_DIR:-/data/git}"
export_archive_configured="${SHERPA_EXPORT_ARCHIVE_DIR:+yes}"
SHERPA_EXPORT_ARCHIVE_DIR="${SHERPA_EXPORT_ARCHIVE_DIR:-/data/exports}"
export SHERPA_CONTENT_DIR
install -d -m 0700 -o sherpa -g sherpa "$SHERPA_CONTENT_DIR" "$SHERPA_EXPORT_ARCHIVE_DIR"
if [ -z "$export_archive_configured" ]; then
  unset SHERPA_EXPORT_ARCHIVE_DIR
fi

export HOME=/home/sherpa
exec setpriv \
  --reuid=sherpa \
  --regid=sherpa \
  --init-groups \
  --no-new-privs \
  /usr/local/bin/registry "$@"
