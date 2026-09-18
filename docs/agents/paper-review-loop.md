# 论文修订—独立审稿循环（作者 2026-08-30 定）

目标：在分支 `tkde-review-revisions` 上持续修改 `paper/tkde/main.tex` / `supplement.tex`，每轮由**独立子代理**以 IEEE TKDE
审稿人视角评审，按意见修改，直到模拟评审通过。启动词：`/loop 30m 按 docs/agents/paper-review-loop.md 跑一轮论文修订—审稿循环`。

## 不可违背的边界（与 CLAUDE.md 同源）

1. **改主张不改证据。** 论文里每个数字来自 `paper/tkde/generated/evidence.tex` 的宏；不得手写数字、不得改样本/记录/回执。
2. **不改 measured paths** 除非必要：`V5_MEASURED_PATHS`（`paper/tkde/generate_evidence.py:79-83`，含 generate_evidence.py 本身、
   scripts/integration-test.sh、record-compose-e2e.sh、cmd/、internal/、config/…）。改了就要在 WSL 重跑 v5-outcome 链（约 55 分钟）
   才能 `make paper-final-check`。循环里优先只改 tex 与 `final_v5_publication_evidence.py` 以外的纯文本；若确需 generator 新宏，
   集中一次改完再重跑链。
3. **P7.4 措辞底线**：不得声称"首次按字段计费/首次同一信息只收一次/首次等价 SQL 不套利/首次非货币数据预算/首次 provenance+Gateway"。
4. **需要新实验或新证据的意见不在循环内做**（同机成本参照、可接纳率研究、对抗片段驱动、专家研究等）：记入
   `tmp/reports/reviews/open-items.md`，在 open-items.md 与台账记录待作者裁决项，循环继续处理其余意见。
5. **不在 WSL 跑 campaign/门禁期间构建 PDF 以外的重活**；构建（`make paper-tkde`）是轻活可以跑。
6. 每轮改动提交到分支并推送；不合回 `tkde-artifact-rerun`（合回由作者或最终 final-check 绿后单独做）。

## 一轮的步骤

1. **对表与状态**：`TZ=Asia/Shanghai date`；NAS 在分支 `tkde-review-revisions` 且树净；WSL 可达且同分支；有无正在跑的链
   （`~/stage-e/logs/post-fix-chain.out` 是否 `CHAIN_DONE`）。上一轮未完成的修改先做完。
2. **构建当前稿**（WSL）：`git pull --ff-only` → `make paper-tkde` → 记录 `main.pdf`/`supplement.pdf` 页数与 LaTeX error 数 → 
   `pdftotext -layout` 两份 PDF 到 `tmp/reports/reviews/round-N/{main,supplement}.txt`（scp 回 NAS 同路径），
   同时 `python3 -m unittest` 在 `paper/tkde` 目录下跑 `test_generate_evidence` 与 `test_final_v5_publication_evidence`。
3. **独立评审**：用 Agent 工具起一个 `general-purpose` 子代理（不给它任何上一轮评审或修改记录），输入只有：
   round-N 的 main.txt、supplement.txt、`paper/tkde/generated/evidence.tex` 路径，以及固定的审稿指令（见下）。
   要求它输出 `tmp/reports/reviews/round-N/review.md`，结构固定：Summary / Strengths / Major (编号，每条附具体页码或章节与"改什么才算解决") /
   Minor / Questions / **Verdict ∈ {Accept, Minor revision, Major revision, Reject}** / **Blocking items 列表**（每条标 `text-only` 或
   `needs-new-evidence`）。
4. **裁决与修改**：逐条处理 Major/Minor：
   - `text-only`：本轮改（改主张、补说明、调结构、删冗余、统一术语）；每条改动在 `round-N/changes.md` 记「意见 → 改在哪（文件:行）→ 怎么改」。
   - `needs-new-evidence`：记入 `open-items.md` 并上报，不改证据。
   - 评审若指出的"事实错误"与证据不符，以证据为准并在 changes.md 记明拒绝理由。
5. **复核**：`make paper-tkde` 无 error；宏引用与发射一致（`\FinalVFive…`/`\RQ…` 无未定义）；页数记录（目标主稿 12 页，超出时优先删冗余，
   不删证据表）；单测绿；tex 花括号/表环境平衡。
6. **提交推送**：`paper: review round N — <一句话>`（`-F` 文件），推送分支；台账追加一行 `P7-LOOP-ROUND-N`（verdict、blocking 数、页数、提交号）。
7. **停止条件**（满足其一即 `ScheduleWakeup stop` / 删 cron 并上报）：
   - 连续两轮 Verdict 为 Accept 或 Minor revision 且 blocking items 全为 `needs-new-evidence`（文本层面已无可改）；
   - 连续三轮 Major 意见集合没有变化（评审在原地打转）——上报作者裁决；
   - 作者叫停。
8. **每轮结束**：台账追加一行（轮次/verdict/blocking 数与页数；本轮改了什么；需作者裁决什么）。

## 给独立审稿子代理的固定指令（逐字）

> You are a reviewer for IEEE Transactions on Knowledge and Data Engineering. Review the manuscript whose rendered text is in
> `<main.txt>` and `<supplement.txt>` (the supplement is supplementary material; the main paper must stand alone). You may consult
> `<evidence.tex>` for the macro definitions behind every number. Do not consult any other file, prior review, or git history.
> Judge as a database-systems and data-security reviewer would: significance and novelty against the cited related work;
> soundness of definitions, theorems, and protocol claims; whether the evaluation supports the claims (baselines, scale,
> independence of the oracle, statistics); reproducibility; clarity, length (TKDE regular paper: 12 pages incl. references and
> biographies), and presentation. Be concrete: every Major item must say which section/page it concerns and what change would
> resolve it, and must be labeled `text-only` (resolvable by rewriting, restructuring, or clarifying) or `needs-new-evidence`
> (requires new experiments, users, or data). Do not reward disclaimers; penalize claims the evidence does not support.
> Output exactly these sections: Summary; Strengths; Major concerns (numbered); Minor concerns; Questions for the authors;
> Verdict (one of Accept / Minor revision / Major revision / Reject); Blocking items (numbered, each with its label).
