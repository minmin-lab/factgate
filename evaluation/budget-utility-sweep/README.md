# Budget-utility sweep (P10 B3; admission arithmetic, not measured)

`results.json` (schema 2) is produced by `go run ./evaluation/cmd/budget-utility-sweep`
from the frozen corpora and their oracles. For each multiplier m of the
owner-derived recipe it reports, on the benign side, how many of the 23
authorized agent-written statements the set ledger admits, and on the
adversary side, how many bits the bisection ladder recovers and how many
distinct Dependency facts greedy extraction reaches.

The benign curve is an exact replay of the corpus's closed-form footprints in
trace order: a statement is admitted iff every dimension stays within budget,
a refused statement adds nothing, and each statement's Dependency novelty is
the set difference against the history the replay itself admitted (so a
refusal changes what later statements cost). Release and Outcome follow the
recipe's per-statement sums because the corpus records their counts, not
their fact sets. The closed-form footprints over-approximate the production
rule statement by statement (they count a page's scanned rows where the rule
counts output cells). That containment does not order the admitted counts:
the curve is a scenario simulation under the closed-form model, neither a
lower nor an upper bound on what the deployed ledger would admit (see
"Withdrawn: the lower-bound reading" below). `admitted_pct` is admitted
authorized statements over authorized statements; it is not a business-task
completion rate, and the replay does not model an agent that rewrites its
next statement after a refusal.

Validation at the executed points: the replay admits 21/23 at 1x (first
refusal q23) where the executed pilot admitted 22/23 (first refusal q26), and
23/23 at 2x and 4x as the pilot did (`pilot-benign-07`); the adversary
arithmetic reproduces owner 6 bits / greedy 18, loosened 11 bits, and, at the
tightened tier's budgets, 4 bits / greedy 12 (`pilot-adversary-05`). Nothing
here is an executed measurement at multipliers other than those.

## Withdrawn: the schema-1 "executed-increment" curve

Schema 1 (commits up to 3327996) also carried a second benign curve that read
each statement's charged Release/Dependency/Outcome increment from the
executed 4x arm (every statement accepted) and replayed those fixed
increments under smaller budgets, labelling the result an upper bound on
admission. That label was wrong, and the results file showed it: at 0.25x the
"lower bound" corpus curve admitted 6/23 while the "upper bound" admitted
5/23. The per-statement monotonicity the label relied on (a refusal leaves a
later statement's true novelty at least as large as its recorded increment)
holds, but it does not bound the admitted count: a statement admitted at an
understated cost can consume budget that several later, cheaper statements
would otherwise have used. Counter-example with one binding dimension and
budget 5: candidate sets {1..10}, {1..14}, then five singletons {15}..{19};
recorded increments in the all-accepted history are [10,4,1,1,1,1,1]; the
fixed-increment replay admits 2 statements, the exact replay admits 5. The
curve is removed rather than relabelled, and the executed sample it read is
no longer needed by this tool.

## Withdrawn: the lower-bound reading of the schema-2 curve

Schema 2 as first committed (through b200a55) said that, because every
closed-form footprint is a superset of the production footprint, the replay
admits no more statements than the deployed ledger at each multiplier. That
inference is wrong for the same reason the schema-1 upper bound was wrong:
admission is path-dependent, and a per-statement containment does not order
the admitted counts of two trajectories. A statement the model refuses at an
overstated cost leaves budget for later statements that a ledger which
admitted it would refuse. Counter-example with one binding dimension and
budget 3: exact candidate sets {a,b}, {c,d,e}, {c,d} admit 1 statement (the
first); with the first set overstated as {a,b,x,y} the same sequence admits 2
(the second and third). A random search over small instances (2 to 5
statements over an 8-fact universe, each model set a superset of its exact
set) finds the model admitting more in 1,890 of 200,000 instances. The
numbers in `results.json` are unchanged; only the reading is withdrawn, in
the `benign_note`, here, and in the supplement.

Standing rule for this tool: a claim that one admission trajectory admits
at most (or at least) as many statements as another needs a proof of that
trajectory-level statement or an exhaustive check; per-statement dominance
is not such a proof.
