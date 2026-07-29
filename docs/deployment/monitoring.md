# Monitoring and alert delivery

Uptime Kuma on the Hostinger VPS watches the SherpA beta and delivers alerts to
a private Matrix room on the self-hosted Synapse instance. No third-party
service is involved.

## What is configured

| | |
|---|---|
| Dashboard | Uptime Kuma v2, container `uptime-kuma-h22v-uptime-kuma-1` |
| Homeserver | Synapse at `https://matrix.kruth-support.de` |
| Sender | `@sherpa-alerts:matrix.kruth-support.de` (dedicated bot) |
| Room | `SherpA Alerts` — private, invite-only, **unencrypted** |
| Secrets | `/root/sherpa-alerts-matrix.env` on the VPS, mode `0600` |

Alerting monitors:

| ID | Monitor | Target |
|---|---|---|
| 11 | SherpA Off-Site Collector `/readyz` | VPS collector |
| 13 | SherpA Beta Registry `/healthz` | Railway |
| 14 | SherpA Beta Website `/healthz` | Railway |

The other monitors on the instance are unrelated client sites and were
deliberately left without a notification provider; wiring them up is a separate
decision.

## The room must stay unencrypted

Uptime Kuma's Matrix provider PUTs a plaintext `m.room.message` through the
client API (`server/notification-providers/matrix.js`) and has no E2EE support.
If encryption is ever enabled in this room, alerts do not fail loudly — they
arrive as undecryptable messages, which looks identical to no alerts at all.
Synapse sets no `encryption_enabled_by_default_for_room_type`, so the room was
created unencrypted and stays that way unless someone turns it on in a client.

## Shared-fate limits

Kuma, Traefik, Synapse, and the collector all run on the same VPS. That host is
a single point of failure for both detection and delivery:

- **VPS or Traefik down** — nothing observes the outage and nothing could report
  it anyway. Silence is indistinguishable from health.
- **Synapse down** — Kuma still detects failures, but cannot deliver them.
  Monitor 10 watches Matrix itself and therefore cannot report its own outage.
- **Railway registry or website down** — fully covered. These are the failures
  the channel actually protects against today, and they are the beta's most
  likely ones.

Closing the first two gaps needs something off this host: an external
dead-man's-switch that alerts when Kuma stops reporting, or a second channel on
independent infrastructure. Neither is in place. Until then, absence of alerts
is not evidence of health.

## Operations

Verify or change the wiring (Kuma caches notifications in memory and holds the
SQLite file open, so it must be stopped for writes and restarted after):

```sh
docker exec uptime-kuma-h22v-uptime-kuma-1 \
  sqlite3 -readonly /app/data/kuma.db \
  "select m.id, m.name, case when mn.id is null then '-' else 'matrix' end
     from monitor m left join monitor_notification mn on mn.monitor_id = m.id
    order by m.id;"
```

Rotate or revoke the bot's access token:

```sh
. /root/sherpa-alerts-matrix.env
curl -X POST "$MATRIX_HOMESERVER/_matrix/client/v3/logout" \
  -H "Authorization: Bearer $MATRIX_TOKEN"
```

The token in Kuma's `notification.config` becomes invalid immediately, so log in
again with `MATRIX_PASSWORD`, update the stored token, and restart Kuma.

## Hardening note

The homeserver runs with `enable_registration: true` and
`enable_registration_without_verification: true`, so anyone who can reach
`matrix.kruth-support.de` can create an account. This predates the alert channel
and is unrelated to SherpA, but it is worth closing before the homeserver
carries operational alerts for a public beta.

## Verification

Tested end to end on 2026-07-29 with a temporary monitor pointed at a URL that
returns 404, then flipped to a healthy one, then deleted. Both directions were
confirmed in the room's event history on Synapse rather than from Kuma's logs
alone:

```
09:41:59Z  [ZZ Alert Self-Test (temporary)] [🔴 Down] Request failed with status code 404
09:43:0xZ  [ZZ Alert Self-Test (temporary)] [✅ Up] 200 - OK
```

The down event matches Kuma's own log line for monitor #15 to the second. No
real service was disrupted and no real monitor's history was affected.
