# SherpA Domain Plan

Status: 2026-07-27. Supersedes nothing; complements `railway.md` and `collector.md`.

## Strategy

Two phases with a deliberate hard reset between them. The beta runs on a throwaway
domain, and go-live is a fresh build on the permanent domain rather than a migration.

| Phase | Domain | Railway environment | Fate |
|---|---|---|---|
| Internal beta | `trysherpa.net` | `staging` | Discarded entirely at go-live |
| Public launch | `sherpa.guide` | `production` (built fresh) | Permanent |

The reset is the point. Nothing is migrated between phases, so the classes of bug
that a domain migration would create do not arise. See "Go-live reset" below for
what that costs.

## Hostname scheme

Both phases use the same shape. The registry and the website must be on
**distinct origins** — `internal/registry/config.go` rejects a configuration where
scheme and host match — and both must be HTTPS outside loopback.

Beta:

```text
trysherpa.net            -> web service      (discovery site)
registry.trysherpa.net   -> registry service (API + git over HTTPS)
```

Go-live:

```text
sherpa.guide             -> web service
registry.sherpa.guide    -> registry service
```

The registry host is the one that matters most: it is embedded in every
`repo_url` the API returns, and therefore in every git remote a user ends up with.

## Variables bound to the hostnames

Set on the registry service:

| Variable | Beta value |
|---|---|
| `SHERPA_PUBLIC_BASE_URL` | `https://registry.trysherpa.net` |
| `SHERPA_WEB_PUBLIC_BASE_URL` | `https://trysherpa.net` |
| `SHERPA_GITHUB_CLIENT_ID` | from the beta OAuth app |
| `SHERPA_GITHUB_CLIENT_SECRET` | from the beta OAuth app |

`SHERPA_GITHUB_CLIENT_SECRET` and `SHERPA_WEB_PUBLIC_BASE_URL` must be set
together, and both require `SHERPA_GITHUB_CLIENT_ID` and `SHERPA_PUBLIC_BASE_URL`.

Each phase gets its **own GitHub OAuth app**. Device flow enabled. Callback:

```text
https://registry.trysherpa.net/v1/auth/web/callback
```

## What is server-side and therefore free to change

`repo_url` is computed per request from `SHERPA_PUBLIC_BASE_URL`
(`internal/registry/api/read.go:276`). It is not persisted, so the registry's
own view of its hostname is a single environment variable.

## What is client-side and therefore sticky

Written into each user's `~/.sherpa` (or `$SHERPA_HOME`) and never re-resolved:

- session `registry_url` — a mismatch is a hard error, forcing `sherpa login`
- `profile.Origin` — the git remote `sherpa update` fetches from
- `profile.Registry.RegistryURL` — drives follow, trial, and update calls
- the git remote inside each installed profile

`sherpa remove <profile>` clears a stale profile through the CLI. A full reset
still means deleting `$SHERPA_HOME`, which also drops sessions and trials.

## Go-live reset

Everything on `trysherpa.net` is discarded: Postgres, the `/data` git volume and
every bare repository in it, all sessions, all published stacks and their version
history. Version numbers restart at 1.

Do **not** redirect `trysherpa.net` to `sherpa.guide`. Git follows HTTP redirects
on fetch, so a redirect would point existing clones at a live host where their
stack no longer exists — a confusing failure. Let the beta domain stop resolving,
or serve a static "beta ended, reinstall" page.

Tester instructions at go-live:

1. Delete `~/.sherpa`, or remove stacks individually with `sherpa remove`.
2. Reinstall the CLI and run `sherpa init`; `mine` rebuilds from `~/.claude`.
3. Register and log in again on the new registry.

Warn testers in advance that work committed with `sherpa save` inside an
installed profile is lost with that directory unless they published it or copied
it out first.

## Backups do not reset

The collector's Borg repository is append-only; the routine identity cannot
delete archives. Every encrypted beta snapshot persists after the reset, and
clearing it requires the recovery identity plus its own fresh approval per
`collector.md`.

Therefore: **provision a separate Borg repository for production.** Reusing the
beta repository would interleave beta and production recovery points in one
append-only store with no clean way to separate them afterwards.

## Related

Everything else deferred until public launch — binary signing and notarization,
alert delivery, the disaster-recovery gate, snapshots — is tracked in
`go-live.md`.

## Open items

- `sherpa.guide` is not yet registered. The relaunch depends on it; it was still
  unregistered as of 2026-07-27. Register and park it before the beta starts.
- `trysherpa.net` registration had not appeared in the `.net` registry as of
  2026-07-27; DNS cannot be configured until it does.
- The web service has never been deployed in any environment.
- No alert delivery is configured for the readiness monitor.
