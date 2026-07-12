#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "entrypoint must run as root to prepare the content volume" >&2
  exit 1
fi

content_dir="${SHERPA_CONTENT_DIR:-/data/git}"
mkdir -p -- "$content_dir"
chown sherpa:sherpa -- "$content_dir"

export HOME=/home/sherpa
exec setpriv \
  --reuid=sherpa \
  --regid=sherpa \
  --init-groups \
  --no-new-privs \
  /usr/local/bin/registry "$@"
