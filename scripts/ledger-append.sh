#!/usr/bin/env bash
# Append one entry to the execution ledger (docs/codex_publication_execution_plan.md §10)
# with the timestamp taken from the clock at the moment of writing, so an entry
# can never carry a guessed time (KB1-LOOP-145 and KB1-LOOP-147 both wrote one).
#
#   scripts/ledger-append.sh BODY_FILE
#
# BODY_FILE holds the entry without the leading "- [time] " stamp: its first
# line is the bold title, further lines are the indented bullets. The stamp is
# UTC+8 as the ledger requires, whatever the host's zone.
set -euo pipefail
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
LEDGER="$ROOT/docs/codex_publication_execution_plan.md"
body="${1:?usage: ledger-append.sh BODY_FILE}"
[ -s "$body" ] || { echo "ledger-append: $body is empty" >&2; exit 1; }
stamp=$(TZ=Asia/Shanghai date '+%Y-%m-%d %H:%M')
{ printf -- '- [%s UTC+8] ' "$stamp"; cat "$body"; } >> "$LEDGER"
tail -c 1 "$LEDGER" | od -An -c | command grep -q '\\n' || printf '\n' >> "$LEDGER"
echo "appended at $stamp UTC+8"
