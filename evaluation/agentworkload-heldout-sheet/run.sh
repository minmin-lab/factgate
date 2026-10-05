#!/bin/sh
# Held-out sheet-correction study: generate, lower with the frozen lowerer, summarize.
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH= cd -- "$here/../.." && pwd)
cd "$root"
python3 "$here/gen.py"
mkdir -p "$here/results"
for arm in original corrected; do
    for rep in 1 2 3; do
        go run ./evaluation/cmd/agent-workload-lowerability -root "evaluation/agentworkload-heldout-sheet/arms/$arm-r$rep" \
            -out "evaluation/agentworkload-heldout-sheet/results/$arm-r$rep.json"
    done
done
python3 "$here/analyze.py"
