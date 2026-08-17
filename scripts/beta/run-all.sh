#!/usr/bin/env bash
# Runs the offline beta suites in order and prints one summary.
#
#   scripts/beta/run-all.sh                       # offline suites only
#   SHERPA_BETA_ONLINE=1 scripts/beta/run-all.sh  # also the live-registry suite
#   SHERPA_BIN=./dist/sherpa-darwin-arm64 scripts/beta/run-all.sh

set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
SUITES="10-core-claude.sh 20-core-codex.sh 30-trust-gate.sh"
if [ "${SHERPA_BETA_ONLINE:-}" = "1" ]; then
	SUITES="$SUITES 40-registry-online.sh"
fi

BIN="${SHERPA_BIN:-sherpa}"
printf 'sherpa binary: %s\n' "$(command -v "$BIN" 2>/dev/null || printf '%s' "$BIN")"
"$BIN" version 2>/dev/null || true

FAILED=""
for suite in $SUITES; do
	printf '\n==================== %s ====================\n' "$suite"
	if ! bash "$DIR/$suite"; then
		FAILED="$FAILED $suite"
	fi
done

printf '\n============================================================\n'
if [ -n "$FAILED" ]; then
	printf 'FAILED suites:%s\n' "$FAILED"
	exit 1
fi
printf 'All suites passed.\n'
