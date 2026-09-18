# Counter comparators under one aligned failure policy (P10.B4)

Admission arithmetic only: no new run. `replay.py` replays the four
comparator arms of the executed pilot (`pilot-counter-rigor-02`: exact set
floors, release-set only, cumulative row counter, query counter) with their
a-priori budgets (`config/profiles/counter-*.catalog.yaml`) and their three
frozen orders (`evaluation/finalv5counter/corpus-v1.json`) over the sealed
publication campaign's unlimited RLS sample, whose independent oracle records
every statement's Release/Dependency/Outcome fact sets and row count.

One failure policy for every arm: refuse the crossing query whole, release
nothing, keep the task alive, keep evaluating later queries. The executed
default products differ here (row and query crossings truncate, settle and
archive the task); the exact-floor arm already has the aligned semantics, and
the script asserts that its aligned replay equals the executed expectation in
all three orders.

Reported per arm and order: admitted, refused, first refusal, refusals whose
facts lie entirely inside the arm's own final released union (a request the
agent paid for and would have gained nothing from), and the released union
sizes. Output `results.json`, digest-bound by `paper/tkde/generate_p10_evidence.py`.

Reproduce: `python3 evaluation/counter-aligned/replay.py`.
Class: pilot-derived arithmetic; publication_eligible false.
