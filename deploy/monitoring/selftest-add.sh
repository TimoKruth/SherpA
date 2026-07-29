#!/bin/sh
# Add a throwaway monitor that is guaranteed to fail, wired to the Matrix
# notification, to prove Uptime Kuma actually dispatches. Removed by
# selftest-remove.sh once the message arrives.
set -eu

CONTAINER=uptime-kuma-h22v-uptime-kuma-1
DB=/var/lib/docker/volumes/uptime-kuma-h22v_uptime-kuma/_data/kuma.db

docker stop "$CONTAINER" >/dev/null
trap "docker start $CONTAINER >/dev/null; echo 'kuma restarted'" EXIT

python3 - "$DB" <<'PY'
import sqlite3, sys

NAME = "ZZ Alert Self-Test (temporary)"
con = sqlite3.connect(sys.argv[1])
con.row_factory = sqlite3.Row
cur = con.cursor()

if cur.execute("select id from monitor where name = ?", (NAME,)).fetchone():
    raise SystemExit("self-test monitor already exists")

# Clone monitor 14 so every NOT NULL column and default is inherited from a
# row Kuma itself created, rather than guessed at.
src = dict(cur.execute("select * from monitor where id = 14").fetchone())
del src["id"]
src.update({
    "name": NAME,
    # 404 on a host we own: no third party is probed, nothing real is disturbed.
    "url": "https://beta.trysherpa.net/__kuma-alert-selftest",
    "interval": 20,
    "retry_interval": 20,
    "maxretries": 0,   # fail on the first check rather than after three
    "description": "Temporary. Verifies Matrix alert delivery. Safe to delete.",
})

cols = ", ".join(src)
cur.execute(
    "insert into monitor (%s) values (%s)" % (cols, ", ".join("?" * len(src))),
    list(src.values()),
)
mid = cur.lastrowid

nid = cur.execute("select id from notification where name = 'Matrix (SherpA Alerts)'").fetchone()[0]
next_id = cur.execute("select coalesce(max(id), 0) from monitor_notification").fetchone()[0] + 1
cur.execute(
    "insert into monitor_notification (id, monitor_id, notification_id) values (?, ?, ?)",
    (next_id, mid, nid),
)

con.commit()
con.close()
print("self-test monitor id: %d" % mid)
PY
