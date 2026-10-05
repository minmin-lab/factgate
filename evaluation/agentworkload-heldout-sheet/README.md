# Held-out workload under a product sheet that matches the lowerer (design frozen 2026-10-05 09:22 UTC+8, before any SQL was generated)

## Why

The held-out study (`evaluation/agentworkload-heldout`, 22 of 40 admitted) gave
the agent the product sheet as `describe_data_product` renders the Catalog. For
the two expense Products that sheet lists `date_trunc`, `to_char` and `/` as
allowed (`config/catalog.yaml` `allowed_functions` / `allowed_operators`), and
the reporting-SQL lowerer has no rule for any of them. Seven of the eighteen
first rejections of that study fall on one of those constructs, so its rate
mixes the language boundary with a wrong instruction. The third simulated
review (2026-10-05) asked which of the two the failures come from; the author
chose to rerun the questions under a sheet that matches the lowerer.

## What is varied and what is not

- **Varied: the sheet, and nothing else.** `products-original.md` is
  byte-identical to the held-out study's sheet. `products-corrected.md`
  differs in exactly two lines: the two expense Products lose
  `Allowed functions: date_trunc, to_char.` and lose `/` from their operator
  list. Nothing is added: no hint of the fragment, of literal forms, of alias
  rules, or of what replaces a removed function. The sheet still
  under-advertises (the lowerer also admits `avg` and `count(distinct)`); that
  is left as it is, because the correction removes false statements only.
- **Not varied:** the 40 questions (frozen file of the held-out study, digest
  in `FROZEN.sha256` there), the lowerer (the current tree; `internal/` is
  unchanged since the held-out freeze commit cf49c49, which `run.sh`'s caller
  checks with `git diff --quiet cf49c49 -- internal/sqllowering`), the model
  (`claude-opus-5`, the main model the held-out generation log records), the
  prompt (`prompt.txt`), the invocation (`gen.py`: headless Claude Code, no
  tools, no settings, empty working directory, clean environment, one call
  per question, output unedited).
- **Both arms are run now.** The original study's generation script and exact
  prompt wording lived in a session scratch directory and are lost, and the
  CLI version has moved (2.1.233 then, recorded in `generation.log` now), so a
  corrected-sheet run alone could not be compared with the 22/40: prompt and
  CLI would differ as well. The `original` arm repeats the original condition
  under today's prompt and CLI; the sheet effect is the difference between the
  two arms of this study. The 22/40 of the held-out study is not replaced and
  stays the held-out figure.

## Size and stop rule

Three repetitions per arm, 40 questions each, 240 statements, generated in one
invocation of `run.sh`. Three repetitions because one LLM pass per arm cannot
separate a sheet effect from generation noise. No extension, no rerun on
data, no selection of repetitions. A call is repeated only on a harness
failure (non-zero exit, error result, empty output), at most twice, and each
such retry is logged; three failures void the whole invocation. Everything
generated is committed.

## What is reported

Per run: statements admitted of 40 and first-rejection reasons
(`results/<arm>-r<k>.json`, from `evaluation/cmd/agent-workload-lowerability`).
Per arm: the three admitted counts, their median and range; how many first
rejections fall on `to_char`/`date_trunc`/`/`; how many statements use those
functions at all. Per question: in how many repetitions of each arm it was
admitted, and which questions were gained or lost in every repetition
(`summary.json`, from `analyze.py`).

This remains an admission rate of first-attempt statements. Admitted
statements are not executed and no answer is graded.

## Priors (written before generation; not fitted)

| Quantity | Prior | What would refute it |
|---|---|---|
| `original` arm, admitted per repetition | 19 to 25 of 40 (the held-out 22, plus or minus generation noise and the change of prompt and CLI) | outside 17 to 27: the new prompt or CLI changes the task, and the comparison with the held-out 22 must be dropped altogether |
| `corrected` arm, first rejections on the three removed constructs | 0 to 3 per repetition: without the false permission the agent still has no admitted way to bucket a date by month, week or quarter, so some statements will use a date function anyway | 5 or more: removing the permission does not change what the agent writes |
| `corrected` minus `original`, median admitted | between 0 and +4. Of the seven sheet-attributed rejections, the two year filters (h12, h36) have an admitted rewrite as a date range if the agent writes plain string literals; the bucketing questions (h06, h14, h40) and the two ratio questions (h09, h38) need a function, a division or `avg` under a sheet that lists only sum, count, min, max | +6 or more: the sheet was the dominant limiter and the held-out 55% understates the language; negative beyond the repetition range of the `original` arm: correcting the sheet hurts |
| where the remaining rejections fall | typed `DATE` literals, `OR`, column-to-column comparisons, alias rules: the language boundary, as in the held-out study | most rejections in a class the held-out study did not show |

## Files

`products-original.md`, `products-corrected.md`, `prompt.txt`, `gen.py`,
`analyze.py`, `run.sh` are committed before generation. `arms/`,
`results/`, `generation.log`, `summary.json` are produced by the one run.
Class: pilot (supplementary), never publication-eligible.
