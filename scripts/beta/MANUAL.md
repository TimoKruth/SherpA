# Manual beta checks

The parts `run-all.sh` cannot automate. Run after every reinstall, in order.
Recorded results are from the 2026-08-06 pass on macOS 26.5.1, arm64,
`sherpa v0.1.0-beta.2`.

Never suppress stderr in these commands. A `2>/dev/null || true` hid a real
clone failure during the first pass and the next three steps failed on top of
it without an obvious cause.

## M1 — Binary install and integrity

```sh
VERSION=v0.1.0-beta.2
ASSET=sherpa-darwin-arm64
BASE=https://github.com/TimoKruth/SherpA/releases/download/$VERSION

cd "$(mktemp -d)"
curl -fsSLO "$BASE/$ASSET"
curl -fsSLO "$BASE/SHA256SUMS"
shasum -a 256 --ignore-missing --check SHA256SUMS
chmod +x "$ASSET"
sudo mv "$ASSET" /usr/local/bin/sherpa
sherpa version
```

Gatekeeper state — expected to fail during beta (go-live items 1–2):

```sh
codesign -dv /usr/local/bin/sherpa 2>&1 | grep -E 'Signature|TeamIdentifier'
spctl --assess --type execute /usr/local/bin/sherpa 2>&1
```

The blocked path is the browser download, not `curl`. `curl` never sets the
quarantine attribute, so checking a `curl`-fetched binary proves nothing.
Download the asset from the releases page in a browser first, then:

```sh
xattr -p com.apple.quarantine ~/Downloads/sherpa-darwin-arm64
spctl --assess --type execute ~/Downloads/sherpa-darwin-arm64
```

Expect the attribute to be present and the verdict to be `rejected`.

## M2 — Login (device flow, needs a browser)

```sh
export SHERPA_REGISTRY_URL=https://registry.trysherpa.net
sherpa login
sherpa status
ls -l ~/.sherpa/registry-session.json                    # expect -rw-------
jq -r '.registry_url, .login' ~/.sherpa/registry-session.json
```

## M3 — Live-registry suite

```sh
cd /Users/timokruth/Projekte/feat
SHERPA_BETA_ONLINE=1 scripts/beta/40-registry-online.sh                        # 22 checks
SHERPA_BETA_ONLINE=1 SHERPA_BETA_PUBLISH=1 scripts/beta/40-registry-online.sh  # 31 checks
```

The publish form creates `@<you>/beta-smoke-<timestamp>` v1 and v2 on the real
registry. They are immutable and survive until the beta reset.

## M4 — Real harness launch

Touches the real `~/.claude` and calls the model. That is the point: it is the
only way to prove the harness reads what SherpA installs.

`sherpa init` reporting `already initialized` is correct, not a failure.

```sh
sherpa init
sherpa status
```

Use a real published stack. The SherpA source repository is not a stack — it
has no `stack.yaml` — so cloning it fails, and `sherpa try` then treats the
missing profile name as a git URL.

```sh
sherpa clone @TimoKruth/go-change-reviewer --name probe --review=keep
PROBE=~/.sherpa/profiles/probe
printf '\nWhen asked for the probe word, answer exactly: MARKER-7731\n' >> "$PROBE/CLAUDE.md"
sherpa try probe
# then ask: what is the probe word?
# PASS = MARKER-7731 comes back. FAIL = the stack was not loaded.
```

Codex `skills/`: SherpA allowlists the path, and Codex CLI 0.146.1
(`gpt-5.6-sol high`) was confirmed on 2026-08-06 to read and execute a skill
from `CODEX_HOME/skills/`. Skill loading is not part of any published contract,
so re-run this probe after a Codex upgrade.

`sherpa run` launches the harness of the *active* profile
(`internal/cli/cmd_run.go:30-34`), and `init --harness codex` deliberately
leaves `mine-claude` active. Without the `use` step below, this probe silently
launches Claude and proves nothing.

```sh
sherpa init --harness codex
CPROF=~/.sherpa/profiles/mine-codex
mkdir -p "$CPROF/skills/probe"
printf -- '---\nname: probe\ndescription: Answers with the probe word\n---\nAnswer exactly: MARKER-7731\n' \
  > "$CPROF/skills/probe/SKILL.md"

sherpa use mine-codex
sherpa status                 # must read: active: mine-codex
sherpa run                    # must show the Codex banner, not Claude
# ask codex to use the "probe" skill; if it cannot see it, record the finding.
sherpa use mine-claude        # restore
```

Check the banner before typing. A Claude banner means the profile did not
switch and the result is meaningless.

## M5 — Website

`curl` carries no browser session, so these results are identical whether or not
you are logged in in a browser. That is expected.

```sh
for p in / /search /login /auth/error /dashboard /healthz /robots.txt \
         /static/app.css /static/app.js /static/sherpa-mark.png /static/favicon.png; do
  printf '%-28s %s\n' "$p" \
    "$(curl -s -o /dev/null -w '%{http_code}' "https://beta.trysherpa.net$p")"
done

curl -s https://beta.trysherpa.net/robots.txt
curl -sI https://beta.trysherpa.net/ | grep -i x-robots-tag
```

Observed and correct: `/` `/search` `/healthz` `/robots.txt` and all four static
assets 200; `/login` and `/dashboard` 303 while anonymous; `/auth/error` 401;
`Disallow: /` and `x-robots-tag: noindex`.

Browser, manually: `/login` → GitHub → `/dashboard`, then `/logout`. Confirm the
session cookie is `Secure`, `HttpOnly`, `SameSite=Lax` in devtools, and open a
user page and a stack detail page.

## M6 — Registry API and git over HTTPS

```sh
REG=https://registry.trysherpa.net
curl -s -o /dev/null -w 'healthz %{http_code}\n' "$REG/healthz"
curl -s "$REG/v1/search?q=review" | jq '.stacks[] | {ref, version}'

TOKEN=$(jq -r .access_token ~/.sherpa/registry-session.json)
curl -s -H "Authorization: Bearer $TOKEN" "$REG/v1/me" | jq
curl -s -H "Authorization: Bearer $TOKEN" "$REG/v1/me/follows" | jq
```

Git transport, independent of the CLI. Substitute a real owner and name — a
literal `<owner>/<name>` returns 404, which git surfaces over HTTP/2 as an
unhelpful `PROTOCOL_ERROR` rather than a plain not-found:

```sh
rm -rf /tmp/stack-clone
git clone "$REG/v1/stacks/TimoKruth/go-change-reviewer.git" /tmp/stack-clone
git -C /tmp/stack-clone tag -l 'v*'
```

Immutability — re-fetch a tag you already have and confirm the SHA is unchanged.
Note the tag is `v2`, not `v1`: `publish` bumps the manifest before tagging, so a
stack seeded at `version: 1` first appears as `v2`.

```sh
git -C /tmp/stack-clone rev-parse v2
git -C /tmp/stack-clone fetch --tags --force && git -C /tmp/stack-clone rev-parse v2
```

## M7 — Collector and alerting

```sh
COL=https://sherpa-collector.kruth-support.de
curl --proto '=https' --tlsv1.2 --fail -sS "$COL/healthz"; echo
curl --proto '=https' --tlsv1.2 --fail -sS "$COL/readyz"; echo
```

A healthy `/healthz` with a failing `/readyz` means the newest recovery point is
older than `SHERPA_COLLECTOR_MAX_RECOVERY_AGE`. To force an export rather than
waiting out the interval, redeploy the registry on Railway and re-check after
`SHERPA_EXPORT_INTERVAL` (set to `1h` for beta) — there is no run-at-boot, which
is go-live item 4.

Alert delivery: pause a Kuma monitor (11, 13, or 14), confirm the message
reaches the Matrix room, then resume it.

## M8 — Cleanup

```sh
sherpa remove probe --yes
sherpa logout
rm -rf /tmp/stack-clone
```

Full reset before a clean reinstall test. This discards anything saved but never
published:

```sh
sudo rm -f /usr/local/bin/sherpa
rm -rf ~/.sherpa
```
