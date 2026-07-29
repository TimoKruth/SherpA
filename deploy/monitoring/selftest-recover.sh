#!/bin/sh
set -eu
CONTAINER=uptime-kuma-h22v-uptime-kuma-1
DB=/var/lib/docker/volumes/uptime-kuma-h22v_uptime-kuma/_data/kuma.db
docker stop "$CONTAINER" >/dev/null
trap "docker start $CONTAINER >/dev/null; echo 'kuma restarted'" EXIT
python3 - "$DB" <<'PY'
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
cur = con.cursor()
n = cur.execute("update monitor set url = 'https://beta.trysherpa.net/healthz' where name = 'ZZ Alert Self-Test (temporary)'").rowcount
if n != 1:
    raise SystemExit("expected exactly one self-test monitor, updated %d" % n)
con.commit(); con.close()
print("self-test monitor pointed at a healthy URL")
PY
