// b5-naive-replay (P10-R2.E1, B5 arm iii on the benign trace) replays the
// frozen benign corpus's closed-form footprints through the naive exact-set
// ledger (evaluation/internal/naiveledger) at a budget multiplier of the
// owner-derived recipe, and reports per-statement settlement time, novelty and
// the ledger's storage. The admission sequence must equal the budget-utility
// sweep's exact replay at the same multiplier (same sets, same rule), which the
// tool checks against evaluation/budget-utility-sweep/results.json.
//
// Dependency facts are the corpus's closed-form fact hashes (the same sets the
// sweep unions). Release and Outcome are recorded by the corpus as counts, not
// sets, so they are represented by statement-scoped synthetic hashes that never
// overlap across statements: the ledger then charges exactly the per-statement
// sums the recipe uses, and the rows it stores for them are the honest cost of
// storing counted facts one row each.
//
// This is a single-host measurement of the naive representation, not campaign
// evidence: it runs against whatever PostgreSQL the DSN names.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
	"taskbound.local/agent-data-gateway/evaluation/internal/naiveledger"
)

type statementPoint struct {
	Index         int     `json:"index"`
	ID            string  `json:"id"`
	Candidates    int     `json:"candidate_dependency_facts"`
	NovelD        int64   `json:"novel_dependency_facts"`
	NovelR        int64   `json:"novel_release_facts"`
	NovelO        int64   `json:"novel_outcome_facts"`
	Admitted      bool    `json:"admitted"`
	LockMS        float64 `json:"lock_ms"`
	InsertMS      float64 `json:"insert_ms"`
	CommitMS      float64 `json:"commit_ms"`
	TotalMS       float64 `json:"total_ms"`
	RowsInserted  int64   `json:"rows_inserted"`
	MicrosPerFact float64 `json:"insert_micros_per_candidate_fact"`
}

type trial struct {
	Trial      int                 `json:"trial"`
	Statements []statementPoint    `json:"statements"`
	Admitted   int                 `json:"admitted"`
	Refused    int                 `json:"budget_refused"`
	Storage    naiveledger.Storage `json:"storage"`
	// StorageAfterVacuum is read after VACUUM (ANALYZE): refused statements roll
	// back their inserted rows, which stay as dead tuples until maintenance.
	StorageAfterVacuum naiveledger.Storage `json:"storage_after_vacuum"`
	TotalMS            float64             `json:"total_settlement_ms"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("B5_DSN"), "PostgreSQL DSN for the naive ledger (B5_DSN)")
	workload := flag.String("agent-workload", "evaluation/agentworkload", "frozen benign workload directory")
	liveCatalog := flag.String("catalog", "config/catalog.yaml", "live catalog path")
	sweep := flag.String("sweep", "evaluation/budget-utility-sweep/results.json", "sweep results to cross-check admission against")
	multiplier := flag.Float64("multiplier", 1, "budget multiplier of the owner-derived recipe")
	trials := flag.Int("trials", 3, "independent trials (the ledger is reset between trials)")
	out := flag.String("out", "", "output JSON path (stdout if empty)")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "b5-naive-replay: -dsn or B5_DSN is required")
		os.Exit(2)
	}
	ctx := context.Background()
	feet, err := finalv5benign.StatementFootprints(finalv5benign.BuildInput{AgentWorkloadDir: *workload, LiveCatalogPath: *liveCatalog})
	if err != nil {
		fatal(err)
	}
	manifest, err := finalv5benign.Load()
	if err != nil {
		fatal(err)
	}
	recipe := manifest.Budgets[0]
	scale := func(v int64) int64 { return int64(math.Ceil(float64(v) * *multiplier)) }
	limits := naiveledger.Limits{Release: scale(recipe.MaxReleaseFacts), Influence: scale(recipe.MaxInfluence), Outcome: scale(recipe.MaxOutcome)}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	var pgVersion string
	if err := db.QueryRowContext(ctx, `SELECT version()`).Scan(&pgVersion); err != nil {
		fatal(err)
	}
	var trialsOut []trial
	for t := 1; t <= *trials; t++ {
		ledger, err := naiveledger.Open(ctx, db)
		if err != nil {
			fatal(err)
		}
		if err := ledger.Reset(ctx); err != nil {
			fatal(err)
		}
		if ledger, err = naiveledger.Open(ctx, db); err != nil {
			fatal(err)
		}
		root := fmt.Sprintf("benign-trial-%d", t)
		if err := ledger.CreateRoot(ctx, root, limits); err != nil {
			fatal(err)
		}
		tr := trial{Trial: t}
		queries := int64(0)
		for _, f := range feet {
			if f.Classification == finalv5benign.ClassPolicyRefused {
				continue
			}
			queries++
			p := statementPoint{Index: f.Index, ID: f.ID, Candidates: len(f.Dependency)}
			if queries > scale(recipe.MaxQueries) {
				// The sweep's query-count dimension: refused before settlement.
				tr.Refused++
				tr.Statements = append(tr.Statements, p)
				continue
			}
			obs := naiveledger.Observation{QueryID: f.ID, Influence: f.Dependency,
				Release: synthetic("release", f.ID, f.ReleaseFacts), Outcome: synthetic("outcome", f.ID, 1+f.PredicateAtoms)}
			charge, m, err := ledger.Settle(ctx, root, obs)
			p.LockMS, p.InsertMS, p.CommitMS, p.TotalMS = ms(m.Lock), ms(m.Insert), ms(m.Commit), ms(m.Total)
			p.RowsInserted = m.RowsInserted
			if len(f.Dependency) > 0 {
				p.MicrosPerFact = float64(m.Insert.Microseconds()) / float64(len(f.Dependency))
			}
			switch {
			case err == nil:
				p.Admitted = true
				p.NovelD, p.NovelR, p.NovelO = charge.Influence, charge.Release, charge.Outcome
				tr.Admitted++
			case errors.Is(err, naiveledger.ErrBudgetExhausted):
				tr.Refused++
			default:
				fatal(fmt.Errorf("statement %s: %w", f.ID, err))
			}
			tr.TotalMS += p.TotalMS
			tr.Statements = append(tr.Statements, p)
		}
		if tr.Storage, err = ledger.ReadStorage(ctx); err != nil {
			fatal(err)
		}
		if err := ledger.Vacuum(ctx); err != nil {
			fatal(err)
		}
		if tr.StorageAfterVacuum, err = ledger.ReadStorage(ctx); err != nil {
			fatal(err)
		}
		trialsOut = append(trialsOut, tr)
		fmt.Fprintf(os.Stderr, "trial %d: admitted %d refused %d total_settle_ms=%.0f fact_rows=%d total_bytes=%d\n",
			t, tr.Admitted, tr.Refused, tr.TotalMS, tr.Storage.FactRows, tr.Storage.TotalBytes)
	}
	crossCheck := crossCheckSweep(*sweep, *multiplier, trialsOut[0])
	result := map[string]any{
		"schema_version":       1,
		"campaign_class":       "pilot",
		"publication_eligible": false,
		"method":               "naive exact-set ledger (evaluation/internal/naiveledger) replaying the benign corpus's closed-form footprints; Dependency = corpus fact hashes, Release/Outcome = statement-scoped synthetic hashes (counts); single host, not a deployment",
		"host":                 hostname(),
		"postgres_version":     pgVersion,
		"git_head":             gitHead(),
		"benign_corpus_sha256": finalv5benign.CorpusSHA256(),
		"multiplier":           *multiplier,
		"limits":               limits,
		"sweep_cross_check":    crossCheck,
		"trials":               trialsOut,
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	encoded = append(encoded, '\n')
	if *out == "" {
		os.Stdout.Write(encoded)
		return
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fatal(err)
	}
}

// synthetic returns n distinct hashes scoped to one statement, so counted
// dimensions charge exactly the per-statement sums the recipe uses.
func synthetic(dim, id string, n int64) []string {
	out := make([]string, 0, n)
	for i := int64(0); i < n; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("b5-synthetic/%s/%s/%d", dim, id, i)))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

// crossCheckSweep compares this replay's admission sequence with the sweep's
// exact replay at the same multiplier.
func crossCheckSweep(path string, multiplier float64, tr trial) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"status": "sweep results unreadable: " + err.Error()}
	}
	var sw struct {
		Benign []struct {
			Multiplier float64 `json:"multiplier"`
			Admitted   int     `json:"admitted"`
			Refusals   int     `json:"budget_refusals"`
			First      string  `json:"first_budget_refusal"`
		} `json:"benign"`
	}
	if err := json.Unmarshal(raw, &sw); err != nil {
		return map[string]any{"status": "sweep results undecodable: " + err.Error()}
	}
	first := ""
	for _, s := range tr.Statements {
		if !s.Admitted {
			first = s.ID
			break
		}
	}
	for _, b := range sw.Benign {
		if b.Multiplier == multiplier {
			return map[string]any{
				"status":         map[bool]string{true: "match", false: "MISMATCH"}[b.Admitted == tr.Admitted && b.Refusals == tr.Refused && b.First == first],
				"sweep_admitted": b.Admitted, "sweep_refusals": b.Refusals, "sweep_first_refusal": b.First,
				"naive_admitted": tr.Admitted, "naive_refusals": tr.Refused, "naive_first_refusal": first,
			}
		}
	}
	return map[string]any{"status": "multiplier not in sweep"}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "b5-naive-replay:", err)
	os.Exit(1)
}
