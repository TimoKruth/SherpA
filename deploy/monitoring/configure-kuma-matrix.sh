#!/bin/sh
# Attach a Matrix notification provider to the three SherpA monitors.
# Uptime Kuma caches notifications in memory and holds the SQLite file open, so
# the container is stopped for the write and restarted unconditionally after.
set -eu

CONTAINER=uptime-kuma-h22v-uptime-kuma-1
DB=/var/lib/docker/volumes/uptime-kuma-h22v_uptime-kuma/_data/kuma.db

. /root/sherpa-alerts-matrix.env
export MATRIX_TOKEN MATRIX_ROOM MATRIX_HOMESERVER

[ -f "$DB" ] || { echo "kuma.db not found at $DB" >&2; exit 1; }

docker stop "$CONTAINER" >/dev/null
# Whatever fails below, the dashboard and the monitors come back.
trap "docker start $CONTAINER >/dev/null; echo 'kuma restarted'" EXIT

python3 - "$DB" <<'PY'
import json, os, sqlite3, sys

db = sys.argv[1]
NAME = "Matrix (SherpA Alerts)"
MONITORS = [11, 13, 14]

config = {
    "name": NAME,
    "type": "matrix",
    "isDefault": False,
    "applyExisting": False,
    "homeserverUrl": os.environ["MATRIX_HOMESERVER"],
    "internalRoomId": os.environ["MATRIX_ROOM"],
    "accessToken": os.environ["MATRIX_TOKEN"],
}

con = sqlite3.connect(db)
con.execute("PRAGMA foreign_keys = ON")
cur = con.cursor()

if cur.execute("select id from notification where name = ?", (NAME,)).fetchone():
    raise SystemExit("refusing to duplicate: a notification with this name exists")

cur.execute(
    "insert into notification (name, active, user_id, is_default, config) values (?, 1, 1, 0, ?)",
    (NAME, json.dumps(config)),
)
nid = cur.lastrowid

# monitor_notification.id is not AUTOINCREMENT, so allocate explicitly.
next_id = cur.execute("select coalesce(max(id), 0) from monitor_notification").fetchone()[0] + 1
for offset, monitor in enumerate(MONITORS):
    row = cur.execute("select name from monitor where id = ?", (monitor,)).fetchone()
    if row is None:
        raise SystemExit("monitor %d does not exist" % monitor)
    cur.execute(
        "insert into monitor_notification (id, monitor_id, notification_id) values (?, ?, ?)",
        (next_id + offset, monitor, nid),
    )
    print("attached -> monitor %d (%s)" % (monitor, row[0]))

con.commit()
con.close()
print("notification id: %d" % nid)
PY
