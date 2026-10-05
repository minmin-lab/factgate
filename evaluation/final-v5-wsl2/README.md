# TaskGate final V5 WSL2 experiment framework

This is the preregistered, fail-closed home for the TKDE V5 + Parquet + Receipt V8 campaign. The publication campaign the paper cites is `formal-v113-publication-05` (contract release v1.13, sealed 2026-09-22, 172 cells x 3 rounds), bound by `publication-evidence-v1.json`; its raw samples and retained deployment bytes live outside git under `raw/` on the WSL2 host, and the files needed to recompute the paper are deposited (`raw-evidence-deposit.json`, `raw-evidence-manifest.sha256`). Earlier campaigns (`formal-v111-publication-03`, `formal-v113-publication-03`) were superseded and their raw directories no longer exist. Historical V4, V5 Outcome, RQ5, and ProvSQL evidence is read-only and is never copied or relabeled here.

## Safety boundary

- `pilot` always writes `publication_eligible=false` and a `PILOT-NOT-FOR-PUBLICATION` marker.
- `publication` requires the exact `TASKGATE_EXPERIMENT_CLASS`, unique campaign ID, and future frozen 40-character commit. Preflight requires Ubuntu 22.04 under WSL2, Linux storage, cgroup v2, Compose 2.24.4+, 24 GiB memory, eight processors, zero swap, 180 GiB free, and a clean measured tree.
- Raw JSONL and derived evidence use create-exclusive files. Failed and invalid samples remain in raw evidence. Task/request IDs are stored only as deployment-salted hashes; tokens, DSNs, keys, rows, and credentials are forbidden.
- Top-level `pipeline_ms` phases are non-overlapping. `diagnostic_ms` entries such as `ordinal_stream` and `bitmap_derivation` may overlap and must not be summed.
- Finalization recomputes Type-7 p50/p95 and a deterministic bootstrap from raw samples. Pilot or incomplete evidence cannot be sealed and emits no numeric LaTeX macros.

## Engineering checks

```sh
make eval-v5-final-validate
make eval-v5-final-preflight
make eval-v5-final-smoke
```

`eval-v5-final-smoke` uses `config/smoke.example.json` with `pilot_kind=synthetic_smoke`; it remains a scheduler/schema smoke and is not a Pilot. The first real-system gate uses the separate `pilot_kind=real_system` config:

```sh
make eval-v5-final-real-pilot
```

It creates a unique fresh Compose project, captures Docker/PostgreSQL/MinIO freshness evidence, provisions real tasks through public OA approval, and runs the source-controlled adapter through Direct PostgreSQL plus public `query_sql`, semantic replay, idempotent replay, and AVAILABLE delivery. One evaluation-only `VerifyReleasedArtifact` call then reconciles the independently read Control binding/effect projection, V8 signature, terminal/registration/availability audit path, raw MinIO canonical ciphertext, and delivered Parquet hash/size/identity/schema. A dedicated recovery arm forces the real Control `PENDING -> AVAILABLE` transaction to fail after the canonical object exists, then proves from `pg_stat_statements`, Control counters, the object store, and the unchanged V8 intent that recovery neither re-queries nor re-settles. It remains `PILOT-NOT-FOR-PUBLICATION` and currently covers only baseline/S1/tiny.

## Runner/adapter contract

All runner entrypoints share `evaluation/internal/experiment`. A runner owns strict config validation, exact source-controlled protocol/cell binding, matched-pair identity and randomized arm order, warmup exclusion, process replicates, publication gating, fresh-root hash uniqueness, raw overwrite protection, and failure retention. `evaluation/finalv5oracle` independently computes exact trace unions and the fixed 70% RLS budgets without importing production exposure code.

The unified experiment executable is `evaluation/cmd/final-v5-adapter`. The runner invokes it with `--experiment`; it reads operation JSONL and returns exactly one sample per line. The separate `evaluation/cmd/final-v5-observer` executable is the source-controlled out-of-process probe for the scale, artifact, and ProvSQL paths. The deployment controller builds both from the frozen commit and binds their build manifests and binary SHA-256 identities into durable evidence. The observer receives no database credentials and resolves only the exact formal Compose project through Docker Engine. Non-streaming Engine stats provide cumulative Gateway CPU and all-interface network counters. Because Docker Desktop Engine stats do not expose the exact cgroup-v2 `memory.peak` and `memory.events` files, one minimal read-only command inside the Gateway's private cgroup namespace reads those files after verifying the unified namespace and CPU/memory controllers. The `before` phase runs that memory probe before Engine stats, while the `after` phase runs Engine stats before the memory probe, keeping both probe executions outside the measured CPU/network delta. Read-only commands inside the PostgreSQL containers supply Business SQL and WAL counters; Docker Engine supplies project-wide restart counters. Every measured transition is an explicit `--phase before`/`--phase after` pair with one stable runtime-identity digest. Missing, malformed, regressing, or replaced-runtime counters fail closed. Its `gateway_memory_peak_bytes` is the cumulative kernel peak since that fresh Gateway cgroup was created, includes mmap-backed memory and the minimal memory probes themselves, and is therefore a conservative deployment-cumulative upper bound at the observation point rather than a per-cell reset peak.

`--capabilities` is fail-closed: a formal campaign cannot start until all nine experiment implementations report complete. Only source-controlled real constructors may be registered; a registered Adapter still returns structured invalid evidence when its required private deployment binding or backend is absent.

For Outcome-Merkle cells, the `x1` candidate cannot express 50% or 90% overlap exactly. The source-controlled parser uses `nearest_integer_half_up`, so both `x1-o50` and `x1-o90` contain one overlapping candidate member and retain the target label plus realized integer overlap in evidence. These cells must never be described as having an exact realized 50% or 90% fraction.

Outcome-Merkle uses a recorded `warm_immutable_content_after_fixture_prefill` policy: deterministic content-addressed root and candidate objects are prepared before the measured merge, warmups populate reusable immutable content, and measured samples exercise the production load/difference/union/persist-and-verify path against that warm content. Physical growth can therefore be zero after warmup and is reported separately from logically changed objects. This policy must not be relabeled as a cold first-persist measurement.

Outcome-Merkle and extreme kernel/storage samples do not claim a Task-root identity: `root_task_id_hash` remains empty. Their run identity is retained in workload-specific verification evidence. The in-process memory field for these microbenchmarks is `heap_alloc_bytes_after`, a boundary snapshot of Go's `HeapAlloc`; it is not reported as a process or container peak.

Private material is not checked in. Prepare these directories after the code/config freeze:

```text
private-config/
  baseline.json scale.json artifact.json rls.json attack.json
  provsql.json compiler.json concurrency.json rq5.json
dataset-bindings/
  deployment-01.json deployment-02.json deployment-03.json
```

Both private directories must be mode `0700`; every config and deployment binding must be mode `0600`. The three deployment binding files must be byte-for-byte identical. A binding has exactly the four top-level keys `dataset_sha256`, `dataset_probe_sha256`, `catalog_sha256`, and `final_v5_adapter_v2`: the first two separately bind the full five-Product live typed-stream Dataset identity (which must agree with the reviewed formula) and the live SQL sanity-probe result. The strict schema-v2 adapter section contains scale, Artifact, and ProvSQL bindings and must provide the exact 12 dependency-scale cells, six Artifact result cells, and 105 ProvSQL nonce cells. Every Scale cell binds a full `N`-Fact history (including zero-overlap), an `N`-Fact candidate, and the independent `2N-5K` union summary, with the concrete candidate `count(*)` and history `sum(metric)` member-rank intervals fixed by the cell. Artifact query/result expectations cannot express dependency facts or a dependency digest; Artifact exposure remains a production observation rather than an independently verified quantity. Unknown or duplicate JSON keys, binding-v1 input, `deployment_volume_id_sha256`, private probe SQL, observer argv, executable paths, credentials, and extra cells are rejected.

Publication preflight parses every bound query as PostgreSQL SQL, permits one read-only `SELECT`, and proves its products, columns, scopes, observer relations, approval route, query/row/exposure budgets, and exact Catalog digest against source-controlled `config/catalog.yaml`. The Dataset transport, scalar probe, and observer are compiled from frozen source; private JSON cannot select any execution path. Fresh deployment proof retains a credential-free five-Product live/reference agreement separately from the scalar probe source and result, requires each independent identity to equal the reviewed binding, and derives the deployment-volume identity from the actual Compose volume set plus both PostgreSQL system identifiers. The finalizers independently recheck these identities across all three deployments and all nine experiment directories. The current Catalog does not yet authorize the complete formal scale/artifact matrix, so this gate intentionally blocks; do not weaken it or insert synthetic placeholders. Copy the example configs, replace the campaign ID and zero commit, and keep credentials in environment variables—not in configs or bindings. Stage B changed the frozen protocol hash to bind the profile-specific replicate matrix; any private config carrying the earlier hash must be regenerated and reviewed rather than silently reused.

## Commands

Offline validation:

```sh
go run ./evaluation/cmd/v5-full -config evaluation/final-v5-wsl2/config/publication.example.json -validate-only
go run ./evaluation/cmd/v5-scale -config evaluation/final-v5-wsl2/config/scale.example.json -validate-only
go run ./evaluation/cmd/v5-artifact -config evaluation/final-v5-wsl2/config/artifact.example.json -validate-only
go run ./evaluation/cmd/rls-adaptive -config evaluation/final-v5-wsl2/config/rls.example.json -validate-only
go run ./evaluation/cmd/adaptive-attacks -config evaluation/final-v5-wsl2/config/attacks.example.json -validate-only
go run ./evaluation/cmd/taskgate-provsql-pair -config evaluation/final-v5-wsl2/config/provsql-paired.example.json -validate-only
go run ./evaluation/cmd/view-scale -config evaluation/final-v5-wsl2/config/compiler-scale.example.json -validate-only
go run ./evaluation/cmd/v5-concurrency -config evaluation/final-v5-wsl2/config/concurrency.example.json -validate-only
go run ./evaluation/cmd/v5-rq5 -config evaluation/final-v5-wsl2/config/daily-publication.example.json -validate-only
```

A single formal runner has this exact shape; all three environment values are mandatory and must match the config:

```sh
TASKGATE_EXPERIMENT_CLASS=publication \
TASKGATE_SUBMISSION_COMMIT=<40-char-sha> \
TASKGATE_CAMPAIGN_ID=<unique-id> \
go run ./evaluation/cmd/v5-full \
  -config <campaign-run>/baseline/config.json \
  -deployment-id deployment-01 \
  -adapter /path/to/campaign/source-adapter/final-v5-adapter \
  -output <campaign-run>/baseline/raw/deployment-01.jsonl
```

Do not launch formal runners by hand for the final campaign. Start the three deployments from Windows PowerShell:

```powershell
evaluation/final-v5-wsl2/scripts/run-three-deployments.ps1 `
  -Distro Ubuntu-22.04 `
  -RepoPath /home/<user>/agent-scope/task_gateway `
  -CampaignId tkde-v5-<unique> `
  -FrozenCommit <40-char-sha> `
  -PrivateConfigDir /absolute/private-config `
  -DatasetBindingsDir /absolute/dataset-bindings `
  -Deployments 3
```

The controller copies the complete Windows-host manifest bytes into WSL evidence, not only its hash, and shuts WSL down between deployments. `run-deployment.sh` builds the unified adapter, Final-V5 observer, and RQ5 driver from the frozen commit; seals their build identities; checks all capabilities before starting expensive work; creates the fresh Compose project itself; captures freshness/environment/vmstat evidence; invokes every runner; and records swap/OOM/restart acceptance evidence.

## Finalize and seal

Each experiment is independently reconstructable below `raw/<campaign>/<experiment>/`. After all three deployments, finalize and review each directory separately:

```sh
make eval-v5-final-finalize RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
make eval-v5-final-evidence RUN_DIR=/absolute/path/to/raw/<campaign>/baseline
```

A passing run produces `generated/summary.json`, `summary.csv`, `paper-results.json`, `latex/evidence.tex`, `figures/`, and a sealed `evidence/manifest.json`. Updating `paper/tkde/generated/evidence.tex` remains a separate, explicit author action after all experiments are reviewed.

Only after all nine experiment directories pass and are sealed may the campaign-level gate run:

```sh
make eval-v5-final-campaign-finalize CAMPAIGN_ROOT=/absolute/path/to/raw/<campaign>
```
