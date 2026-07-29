# Matrix alert channel provisioning

The scripts that built the Uptime Kuma → Matrix alert channel described in
[../../docs/deployment/monitoring.md](../../docs/deployment/monitoring.md).
They are kept because the go-live plan tears the beta down and rebuilds on
`sherpa.guide`, and this should be replayed rather than re-derived.

Each ran successfully on the beta VPS on 2026-07-29 as committed. They are not
generic: container names, the volume path, the homeserver domain, the owner MXID
and the monitor IDs are hardcoded for that host and must be reviewed before
reuse.

Run as root on the VPS, in order:

| Script | Effect |
|---|---|
| `provision-matrix-alerts.sh` | Registers the bot, logs in, creates the room, invites the owner, writes `/root/sherpa-alerts-matrix.env` (`0600`) |
| `configure-kuma-matrix.sh` | Inserts the notification and attaches it to monitors 11, 13, 14 |
| `selftest-add.sh` | Adds a throwaway monitor pointed at a 404 to force a Down alert |
| `selftest-recover.sh` | Repoints it at a healthy URL to force the Up alert |
| `selftest-remove.sh` | Deletes the throwaway monitor and its rows |

Notes:

- No script prints an access token. Secrets exist only in the `0600` env file on
  the host.
- `provision-matrix-alerts.sh` refuses to run twice, because a second run would
  orphan the first token and room.
- Uptime Kuma caches notifications in memory and holds `kuma.db` open, so every
  script that writes stops the container first and restarts it from an `EXIT`
  trap — the dashboard comes back even if the write fails partway.
- Verify delivery in Synapse's event history, not in Kuma's log: Kuma logs the
  failing check but does not log a successful send.
