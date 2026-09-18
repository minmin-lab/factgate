# Held-out agent-written workload (P10 B1)

Purpose: the original `evaluation/agentworkload` set became a development set
(HAVING, exact AVG, COUNT DISTINCT, and derived cells were admitted after its
first pass). This set was written after the language and implementation were
frozen and is never used to change either.

Protocol (frozen before any SQL was generated):
- `questions.md`: 40 new natural-language reporting questions over the same
  three Product families and in the same style as the original set (20
  expense, 8 orders/line-item, 7 result-heavy, 5 mixed). Written by the author;
  not tuned to the closed fragment; frozen by the digest recorded below before
  the agent saw them.
- `products.md`: byte-identical copy of the original product sheet.
- SQL: one statement per question written by an off-the-shelf LLM assistant
  (Claude via Claude Code, headless `claude -p`), given only `products.md`
  and the one question; no hint of the closed fragment; output not edited.
- Lowering: `go run ./evaluation/cmd/agent-workload-lowerability
  -root evaluation/agentworkload-heldout -out evaluation/agentworkload-heldout/results.json`
  at the frozen commit; the lowerer is not changed afterwards.
- Class: pilot (supplementary), not part of the sealed campaign.

Note: the lowering tool globs `q*.sql`, so the SQL for question hNN is stored as `queries/qNN.sql` (same number); `questions.md` keeps its frozen text and digest.
