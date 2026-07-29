#!/bin/sh
# Remove the throwaway self-test monitor added by selftest-add.sh.
set -eu

CONTAINER=uptime-kuma-h22v-uptime-kuma-1
DB=/var/lib/docker/volumes/uptime-kuma-h22v_uptime-kuma/_data/kuma.db

docker stop "$CONTAINER" >/dev/null
trap "docker start $CONTAINER >/dev/null; echo 'kuma restarted'" EXIT

python3 - "$DB" <<'PY'
import sqlite3, sys

NAME = "ZZ Alert Self-Test (temporary)"
con = sqlite3.connect(sys.argv[1])
con.execute("PRAGMA foreign_keys = ON")
cur = con.cursor()

row = cur.execute("select id from monitor where name = ?", (NAME,)).fetchone()
if row is None:
    raise SystemExit("self-test monitor not present; nothing to remove")
mid = row[0]

# Delete children explicitly rather than trusting cascade to be enabled on
# every table that references monitor.
for table in ("heartbeat", "monitor_notification", "monitor_tag", "stat_daily", "stat_hourly", "stat_minutely"):
    try:
        cur.execute("delete from %s where monitor_id = ?" % table, (mid,))
    except sqlite3.OperationalError:
        pass  # table absent in this schema version
cur.execute("delete from monitor where id = ?", (mid,))

con.commit()
con.close()
print("removed self-test monitor %d" % mid)
PY
