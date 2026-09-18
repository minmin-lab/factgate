# Budget-utility sweep (P10 B3; admission arithmetic, not measured)

`results.json` is produced by `go run ./evaluation/cmd/budget-utility-sweep`
from the frozen corpora and their oracles. For each multiplier m of the
owner-derived recipe it reports, on the benign side, how many of the 23
authorized agent-written statements the set ledger admits, and on the
adversary side, how many bits the bisection ladder recovers and how many
distinct Dependency facts greedy extraction reaches.

Two benign curves are reported because the corpus's closed-form footprints
over-approximate the production rule (they count pages' scanned rows, the
production rule counts output rows' cells): `benign_executed_increments`
uses the system's own charged novelty per statement from the executed x4 arm
(all statements accepted) and is an upper bound on acceptance under smaller
budgets; `benign_corpus_model` unions the corpus's closed-form sets exactly
and is a conservative lower bound. Validation at the executed points:
executed-increment arithmetic reproduces the pilot's 22/23 with first refusal
q26 at 1x and 23/23 at 2x and 4x (`pilot-benign-06`); the adversary
arithmetic reproduces owner 6 bits / greedy 18, loosened 11 bits, and, at the
tightened tier's budgets, 4 bits / greedy 12 (`pilot-adversary-04`).
Nothing here is an executed measurement at multipliers other than those.
