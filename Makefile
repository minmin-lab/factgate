.PHONY: verify test build up down logs formal eval-validate eval-exposure eval-exposure-performance eval-exposure-scale eval-exposure-storage eval-provenance-baseline eval-daily-publication eval-daily-publication-online eval-daily-publication-validate eval-smoke eval-full eval-v5-final-validate eval-v5-final-preflight eval-v5-final-smoke eval-v5-final-real-pilot eval-v5-final-finalize eval-v5-final-campaign-finalize eval-v5-final-evidence artifacts fuzz paper-evidence paper paper-tkde paper-refresh-exposure paper-final-check paper-tdsc

FINAL_V5_SMOKE_RUNNER ?= evaluation/final-v5-wsl2/scripts/run-pilot.sh

verify: hygiene
	docker build --target verify -t taskbound-agent-data-gateway-verify .
	./scripts/compose-test.sh

# -count=1 is load-bearing, not caution. The repository-root invariant is a
# property of the git index, and `go test` cannot make the index a cache input
# (see internal/repohygiene). Staging a binary that .gitignore already covers
# changes nothing the cache can observe, so a warm cache would replay the
# previous pass. This target is the gate's only trustworthy reading.
.PHONY: hygiene
hygiene:
	GOFLAGS=-buildvcs=false go test -count=1 ./internal/repohygiene/...

test:
	docker build --target verify -t taskbound-agent-data-gateway-verify .

build:
	docker compose build

up:
	docker compose up --build -d --wait

down:
	docker compose down

logs:
	docker compose logs -f gateway oa-demo

formal:
	./formal/run.sh

eval-validate:
	./evaluation/run.sh validate

eval-exposure:
	./evaluation/run-exposure.sh

eval-exposure-performance:
	./evaluation/run-exposure-performance.sh

eval-exposure-scale:
	./evaluation/run-exposure-scale.sh

eval-exposure-storage:
	./evaluation/run-exposure-storage.sh

eval-provenance-baseline:
	./evaluation/provenance-baseline/run.sh

eval-tpch-lowerability:
	go run ./evaluation/cmd/tpch-lowerability

eval-ssb-lowerability:
	go run ./evaluation/cmd/ssb-lowerability

eval-agent-workload-lowerability:
	go run ./evaluation/cmd/agent-workload-lowerability

eval-generated-algebra:
	go run ./evaluation/cmd/generated-algebra

eval-daily-publication-validate:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication/test_harness.py
	PYTHONDONTWRITEBYTECODE=1 python3 evaluation/daily-publication/evidence/validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication/evidence/test_validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 evaluation/daily-publication-online/evidence/validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest evaluation/daily-publication-online/evidence/test_validate.py
	PYTHONDONTWRITEBYTECODE=1 python3 paper/tkde/rq5_evidence.py
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest paper.tkde.test_rq5_evidence
	bash -n evaluation/daily-publication/run.sh evaluation/daily-publication-online/run.sh
	docker compose --file evaluation/daily-publication-online/compose.yaml config --quiet
	docker run --rm -v "$(CURDIR):/src:ro" -w /src golang:1.25-bookworm@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 go test ./evaluation/daily-publication/cmd/phase ./evaluation/cmd/rq5-online-transition ./cmd/snapshot-sidecar-install

eval-daily-publication:
	./evaluation/daily-publication/run.sh

eval-daily-publication-online:
	./evaluation/daily-publication-online/run.sh

eval-smoke:
	./evaluation/run.sh smoke

eval-full:
	./evaluation/run.sh full
	./evaluation/security/run-full.sh

eval-v5-final-validate:
	./evaluation/final-v5-wsl2/scripts/validate.sh

eval-v5-final-preflight:
	./evaluation/final-v5-wsl2/scripts/preflight-wsl2.sh --mode pilot

eval-v5-final-smoke:
	@set -eu; \
	tmp_dir="$$(mktemp -d /tmp/taskgate-final-v5-smoke.XXXXXX)"; \
	run_dir="$$tmp_dir/run"; \
	trap 'rm -rf "$$tmp_dir"' EXIT; \
	"$(FINAL_V5_SMOKE_RUNNER)" "$$run_dir"; \
	test -f "$$run_dir/PILOT-NOT-FOR-PUBLICATION"; \
	grep -q 'publication_eligible=false' "$$run_dir/PILOT-NOT-FOR-PUBLICATION"; \
	if test -f "$$run_dir/generated/latex/evidence.tex"; then \
		! grep -q '\\newcommand' "$$run_dir/generated/latex/evidence.tex"; \
	fi

eval-v5-final-real-pilot:
	TASKGATE_REAL_PILOT_BUILD=1 ./evaluation/final-v5-wsl2/scripts/run-real-pilot.sh

eval-v5-final-finalize:
	@test -n "$(RUN_DIR)" || (echo "RUN_DIR is required" >&2; exit 2)
	./evaluation/final-v5-wsl2/scripts/finalize.sh "$(RUN_DIR)"

eval-v5-final-campaign-finalize:
	@test -n "$(CAMPAIGN_ROOT)" || (echo "CAMPAIGN_ROOT is required" >&2; exit 2)
	go run ./evaluation/cmd/final-v5 campaign-finalize --campaign-root "$(CAMPAIGN_ROOT)"

eval-v5-final-evidence:
	@test -n "$(RUN_DIR)" || (echo "RUN_DIR is required" >&2; exit 2)
	go run ./evaluation/cmd/final-v5 evidence --run-dir "$(RUN_DIR)"

artifacts:
	./evaluation/generate-artifacts.sh

fuzz:
	./evaluation/fuzz/campaign.sh

paper-evidence:
	python3 paper/tkde/generate_evidence.py

paper:
	./paper/tkde/build-container.sh

paper-tkde:
	./paper/tkde/build-container.sh

paper-refresh-exposure:
	./paper/tkde/build-container.sh refresh-exposure

paper-final-check:
	@worktree_status=$$(git --no-replace-objects status --porcelain --untracked-files=all) || exit $$?; \
		if test -n "$$worktree_status"; then echo "paper-final-check requires a clean worktree" >&2; exit 1; fi
	python3 paper/tkde/generate_evidence.py --evidence-mode final
	git --no-replace-objects diff --exit-code HEAD -- paper/tkde/generated/evidence.tex
	./paper/tkde/build-container.sh final

# Go writes a main package's executable into the working directory, so building
# a command from the repository root leaves the binary where `git add -A` will
# sweep it in. Everything that produces an executable goes here instead.
GENERATED_BIN := generated/bin

# -buildvcs=false because a linked worktree makes the VCS stamp unresolvable;
# it is the standard flag for building this repository outside a plain clone.
.PHONY: bin
bin:
	@mkdir -p $(GENERATED_BIN)
	GOFLAGS=-buildvcs=false go build -o $(GENERATED_BIN)/ ./cmd/... ./evaluation/cmd/...
