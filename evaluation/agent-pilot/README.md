# Headless-agent pilot aggregation (P10.B2)

`analyze.py` aggregates the per-deployment `raw/agent.jsonl` records written by
`final-v5-adapter -agent-pilot` (hook `pilot-hook-agent.sh`) for one campaign
into `results.json`, digest-bound per source file. Design frozen a priori in
`docs/p10_b2_agent_experiment_design.md`; grading is by the fixture truth the
adapter embeds in every record. No run is dropped: harness errors are counted
and listed per cell.

Reproduce: `python3 evaluation/agent-pilot/analyze.py --campaign p10-b2-agent-01`.
Class: pilot; publication_eligible false.
