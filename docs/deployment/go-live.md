# SherpA Go-Live Checklist

Status: 2026-07-27. Work that is deliberately deferred during the internal beta
and must be closed before public launch on the permanent domain.

Domain and teardown specifics live in `domains.md`; this file covers everything
else that beta is running without.

## 1. Sign and notarize the macOS binaries

**Current state, measured on the published `v0.1.0-beta.1` artifacts:**

```text
codesign -dv sherpa-darwin-arm64
  Signature=adhoc
  CodeDirectory flags=0x20002(adhoc,linker-signed)
  TeamIdentifier=not set

spctl --assess --type execute sherpa-darwin-arm64
  rejected
```

The Go linker ad-hoc signs darwin binaries, which is why they execute at all on
Apple silicon, but they carry no Developer ID and Gatekeeper rejects them.

A binary fetched with `curl` has no `com.apple.quarantine` attribute, so the
README's install flow works today. Anyone who downloads through a browser gets
the quarantine attribute and is blocked. The README currently papers over this
with `xattr -d com.apple.quarantine`, which is a workaround, not a fix: it
teaches users to strip a security control by hand.

**Required before launch:**

1. Apple Developer Program membership and a **Developer ID Application**
   certificate. Export as `.p12`.
2. Store as repository secrets: the base64 `.p12` and its password, plus App
   Store Connect API credentials for `notarytool` (issuer ID, key ID, and the
   base64 `.p8`). An Apple ID with an app-specific password also works but ties
   releases to one person's account.
3. Extend the release workflow with a `macos-*` runner job that imports the
   certificate into a temporary keychain and signs both darwin binaries with
   `--timestamp --options runtime`. The hardened runtime is mandatory for
   notarization.
4. Submit with `xcrun notarytool submit --wait`.
5. Generate `SHA256SUMS` **after** signing and stapling. Both rewrite their
   artifact, so checksums produced earlier will not match what ships.
6. Gate publication on `codesign --verify --strict` and
   `spctl --assess --type execute` both succeeding.
7. Drop the quarantine-bypass line from the README.

**Artifact format (decided 2026-07-28):** publish both a staplable archive and
the plain binaries.

`xcrun stapler staple` does not work on a bare Mach-O executable, so a plain
binary can only be validated by an online Gatekeeper check. That is acceptable
because the two install paths differ:

| Path | `com.apple.quarantine` | Gatekeeper | Stapling |
|---|---|---|---|
| `curl` / script / CI | not set | never runs | irrelevant |
| Browser download | set | runs | required offline |

Verified on the published `v0.1.0-beta.1` artifact: a `curl` download carries no
quarantine attribute. Plain binaries therefore serve scripted installs, and the
stapled archive serves browser downloads including offline first launch.

Notarization issues tickets by cdhash and covers nested code, so the same signed
binary is recognised whether it ships inside the archive or standalone.

Prefer `.pkg` over `.dmg` for the archive. A `.pkg` installs directly to
`/usr/local/bin`; a `.dmg` holding a bare executable makes the user mount it,
drag the binary onto `PATH`, and set the execute bit. Note that `.pkg` signing
requires a **Developer ID Installer** certificate, which is distinct from the
**Developer ID Application** certificate used by `codesign`. Both are covered by
the same membership, but two certificates must be created and stored.

Generate `SHA256SUMS` after **stapling**, not merely after signing: stapling
rewrites the archive to embed the ticket.

GitHub-hosted macOS runners are billed at a higher rate than Linux, but Actions
minutes are free for public repositories, so this costs runner time only.

## 2. Sign the Windows binaries

The same gap exists on Windows and is not covered by the item above. The
published `.exe` files are unsigned, so SmartScreen warns on download and on
first run. Closing it needs an Authenticode certificate from a CA; OV
certificates accumulate reputation over time, EV certificates start trusted and
cost more. Lower priority than macOS only if the beta shows few Windows users.

## 3. Configure alert delivery

Uptime Kuma has **zero notification providers**. Monitors 11, 13, and 14 detect
collector readiness and both beta hosts correctly, but a failure notifies
nobody — it is visible only to someone already looking at the dashboard.

Self-hosted Matrix is the preferred channel; the instance is already monitored
and adds no third party. An n8n webhook or SMTP are the alternatives.

## 4. Run an export shortly after startup

`PreparedScheduler.Run` schedules exports with `time.NewTicker(interval)`
(`internal/registry/export/export.go:1146`), so the first export fires one full
interval after process start and there is no run-at-boot. Every deploy, crash,
or host reboot resets the timer, so a registry that restarts more often than
its interval never exports at all.

This is not theoretical. During beta setup on 2026-07-28 the newest recovery
point was 40h old against a 26h `SHERPA_COLLECTOR_MAX_RECOVERY_AGE`, so the
collector reported `/readyz` 503. The registry had been redeployed six times in
the preceding day, and the only stored object existed solely because the
interval had been temporarily lowered to `1m` during the acceptance run. No
alert fired, because no notification provider is configured (see above).

Mitigated for beta by lowering `SHERPA_EXPORT_INTERVAL` to `1h`, which is
shorter than the deploy cadence. That is a workaround, not a fix: a production
registry on a 24h interval remains one restart away from silently skipping a
day.

The fix is to run one cycle shortly after startup and then continue on the
interval. It needs care:

- apply startup jitter, or a crash-looping service will stampede the collector;
- persist the last successful run so a restart does not re-export needlessly;
- keep the existing queue rediscovery, which already runs at boot, unchanged.

## 5. Complete the disaster-recovery gate

Task 14 steps 3 and 6–8 are outstanding:

- the registry retry-persistence drill (disruptive; needs its own approval);
- independent retrieval of a real object using the offline age identity and the
  recovery-only Borg identity, from a trusted environment;
- sibling restore of PostgreSQL and the bare Git repositories, with audit,
  login, search, detail, and clone verified.

Until this passes, the backup chain has never been proven to restore. Doing it
while the only data at risk is synthetic is far cheaper than doing it under
pressure with users' published stacks on the line.

## 6. Verify the Storage Box snapshot schedule

Snapshots are one of the compensating controls the reduced threat model depends
on, and the schedule has never been verified. Until it is, that control is
intended rather than operative. Needs Hetzner Cloud API or console access.

## 7. Give production its own Borg repository

The collector's Borg repository is append-only; the routine identity cannot
delete archives. Every encrypted beta snapshot therefore survives the teardown.
Pointing the production stack at the same repository would interleave beta and
production recovery points in one immutable store with no clean way to separate
them later. Provision a separate repository, and retire the beta one as a unit
under its own approval.

## 8. Railway custom-domain limit

The current plan allows **one custom domain per service**. `registry` and `web`
each have their slot filled by the beta hostnames, so the permanent domains
cannot be added alongside them. Either remove the beta domains first — which
the teardown does anyway — or upgrade the plan if both must coexist during a
cutover.

## 9. Tester migration

See `domains.md`. Testers can now drop individual stacks with
`sherpa remove <profile>`, but the reset still discards local work: anything
committed inside an installed profile with `sherpa save` is lost unless
published or copied out first. Tell testers before the reset, not after.

## Checklist

- [ ] macOS binaries signed with Developer ID and notarized; staplable archive
      published alongside them; README bypass removed
- [ ] Homebrew tap evaluated (bypasses quarantine; likely the primary macOS path)
- [ ] Windows binaries Authenticode signed
- [ ] Alert delivery configured and tested end to end
- [ ] Exporter runs shortly after startup; beta's 1h interval workaround reverted
- [ ] Task 14 steps 3 and 6–8 passed
- [ ] Storage Box snapshot schedule verified
- [ ] Separate production Borg repository provisioned
- [ ] Custom-domain slots freed or plan upgraded
- [ ] Tester reset instructions sent
