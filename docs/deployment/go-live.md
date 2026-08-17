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

**The workflow is already written** (`.github/workflows/release.yml`) and gated
on secrets: when they are absent it publishes the plain binaries exactly as it
does today and skips the package entirely, because an unsigned installer would
be blocked by Gatekeeper anyway. Supplying the secrets below activates signing,
notarization, stapling, and the `.pkg` with no further code change.

| Secret | Contents |
|---|---|
| `MACOS_CERT_P12_BASE64` | Developer ID **Application** certificate, base64 `.p12` |
| `MACOS_CERT_PASSWORD` | its export password |
| `MACOS_SIGN_IDENTITY` | e.g. `Developer ID Application: Name (TEAMID)` |
| `MACOS_INSTALLER_CERT_P12_BASE64` | Developer ID **Installer** certificate, base64 `.p12` |
| `MACOS_INSTALLER_CERT_PASSWORD` | its export password |
| `MACOS_INSTALLER_IDENTITY` | e.g. `Developer ID Installer: Name (TEAMID)` |
| `APPLE_API_KEY_P8_BASE64` | App Store Connect API key, base64 `.p8` |
| `APPLE_API_KEY_ID` | its key ID |
| `APPLE_API_ISSUER_ID` | its issuer ID |

The signing path itself is unverified: it cannot run until the credentials
exist. Treat the first signed release as a test, and check the workflow's own
`spctl --assess` and `stapler validate` gates rather than assuming.

**Remaining prerequisites:**

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

**Done for the beta.** Monitors 11, 13, and 14 deliver to a private Matrix room
on the self-hosted Synapse instance, verified in both directions on 2026-07-29.
See [monitoring.md](monitoring.md).

What remains before go-live is the shared-fate gap, not the channel: Kuma,
Traefik, and Synapse all run on the same VPS, so an outage of that host is both
undetected and undeliverable, and silence cannot be distinguished from health.
Closing it needs something off that host — an external dead-man's-switch on
Kuma, or a second channel on independent infrastructure.

## 4. Run an export shortly after startup

**Implemented in the launch-readiness branch.** `PreparedScheduler.Run` now
records each validated export in a mode-`0600` state file inside the locked
archive queue. On startup it waits only until the next export is due. If no
valid success record exists, it runs after cryptographically random jitter
bounded by the smaller of five minutes or one tenth of the configured interval.
Pending archives still drain before any new archive is created.

This is not theoretical. During beta setup on 2026-07-28 the newest recovery
point was 40h old against a 26h `SHERPA_COLLECTOR_MAX_RECOVERY_AGE`, so the
collector reported `/readyz` 503. The registry had been redeployed six times in
the preceding day, and the only stored object existed solely because the
interval had been temporarily lowered to `1m` during the acceptance run. No
alert fired, because no notification provider was configured at the time. That
gap is now closed (see above), so a repeat would at least be reported.

The beta's temporary `SHERPA_EXPORT_INTERVAL=1h` mitigation can be reverted to
the intended interval after this change is reviewed, merged, deployed, and one
startup-triggered export is observed. Do not change that live variable as part
of the code rollout without the separately required operational approval.

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

## 8. Domain cutover

Railway supports multiple custom domains on a service, so domain capacity does
not force an early beta teardown. If a short validation overlap is useful, add
the permanent domains, verify their certificates and canonical base variables,
then remove the beta domains. Do not redirect the old registry origin; see
`domains.md`.

## 9. Tester migration

See `domains.md`. Testers can now drop individual stacks with
`sherpa remove <profile>`, but the reset still discards local work: anything
committed inside an installed profile with `sherpa save` is lost unless
published or copied out first. Tell testers before the reset, not after.

## 10. Make initialization multi-harness aware

The beta `sherpa init` defaults to one harness unless the user supplies
`--harness`, and importing a second baseline can leave only suffixed baseline
names. Before public go-live, initialization must discover all supported setups,
ask which detected harness should own the canonical protected `mine`, and import
the others as optional protected `mine-<harness-alias>` profiles. It must never
pick the primary from discovery order.

Acceptance coverage must include one detected setup, multiple detected setups,
declined confirmation, an explicit non-interactive primary, adding a harness on
a later run, rerunning without duplication, and rollback after a partial import.
The normative behavior and naming rules are in §3.2 of the design spec.

## Checklist

- [ ] Apple Developer Program joined; both certificates created
- [ ] The nine signing secrets added to the repository
- [ ] First signed release verified; README quarantine bypass removed
- [ ] Homebrew tap evaluated (bypasses quarantine; likely the primary macOS path)
- [ ] Windows binaries Authenticode signed
- [x] Alert delivery configured and tested end to end (Matrix, 2026-07-29)
- [ ] Alerting survives loss of the VPS: external dead-man's-switch or a second
      channel off that host
- [ ] Exporter runs shortly after startup; beta's 1h interval workaround reverted
- [ ] Task 14 steps 3 and 6–8 passed
- [ ] Storage Box snapshot schedule verified
- [ ] Separate production Borg repository provisioned
- [ ] Permanent domains verified and beta domains retired
- [ ] Tester reset instructions sent
- [ ] Multi-harness `sherpa init` discovery, primary confirmation, optional
      baselines, idempotency, and rollback shipped and acceptance-tested
