// b5-naive-scale-replay (P10-R2 B5, same-input ledger comparison) settles the
// sealed campaign's Scale-profile inputs through the naive exact-set ledger
// (evaluation/internal/naiveledger) and sets the result beside the V5
// settlement times the sealed campaign measured on the same inputs.
//
// Same input means: for each of the twelve Scale Dependency cells (candidate
// of N facts against a pre-seeded history of N facts at 0/50/90/100% overlap)
// the candidate and history Dependency sets are the independent oracle's
// canonical facts (finalv5oracle.StreamExposureScaleFacts), the sets the
// campaign finalizer linked member by member to the production ledger's
// committed sets. Release and Outcome carry no scale in these cells (1 and 5
// facts); they are represented by synthetic hashes whose cardinalities and
// history overlap reproduce the charges of the sealed samples.
//
// Before any time is compared the tool checks, per cell and trial, that the
// naive ledger's per-dimension charge equals the charge recorded in every
// sealed sample of that cell, that the root's Dependency cardinality equals
// the oracle union, and that the digest of the root's stored Dependency set
// equals the digest of the oracle union. A failed check aborts the run.
//
// Pilot class: one host, single writer, a standalone PostgreSQL of the
// campaign's image; not campaign evidence.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/naiveledger"
)

type sealedCell struct {
	Samples             int       `json:"samples"`
	SettlementMS        []float64 `json:"-"`
	FactStoreMS         []float64 `json:"-"`
	SettlementP50MS     float64   `json:"control_settlement_p50_ms"`
	SettlementMinMS     float64   `json:"control_settlement_min_ms"`
	SettlementMaxMS     float64   `json:"control_settlement_max_ms"`
	FactStoreP50MS      float64   `json:"exposure_fact_store_p50_ms"`
	ActualRelease       int64     `json:"actual_release_facts"`
	ActualDependency    int64     `json:"actual_dependency_facts"`
	ActualOutcome       int64     `json:"actual_outcome_facts"`
	ChargedRelease      int64     `json:"charged_release_facts"`
	ChargedDependency   int64     `json:"charged_dependency_facts"`
	ChargedOutcome      int64     `json:"charged_outcome_facts"`
	uniform             bool
	firstSampleRecorded bool
}

type naiveTrial struct {
	Trial             int     `json:"trial"`
	SeedHistoryMS     float64 `json:"seed_history_total_ms"`
	LockMS            float64 `json:"candidate_lock_ms"`
	InsertMS          float64 `json:"candidate_insert_ms"`
	CommitMS          float64 `json:"candidate_commit_ms"`
	TotalMS           float64 `json:"candidate_total_ms"`
	ResettleTotalMS   float64 `json:"resettle_total_ms"`
	ChargedRelease    int64   `json:"charged_release_facts"`
	ChargedDependency int64   `json:"charged_dependency_facts"`
	ChargedOutcome    int64   `json:"charged_outcome_facts"`
	RootDependency    int64   `json:"root_dependency_cardinality"`
	RootSetSHA256     string  `json:"root_dependency_set_sha256"`
	FactRows          int64   `json:"fact_rows"`
	TotalBytes        int64   `json:"total_bytes"`
	FactTableBytes    int64   `json:"fact_table_bytes"`
	FactIndexBytes    int64   `json:"fact_index_bytes"`
}

type cellResult struct {
	Cell              string       `json:"cell"`
	CandidateFacts    int64        `json:"candidate_dependency_facts"`
	HistoryFacts      int64        `json:"history_dependency_facts"`
	OverlapPercent    int          `json:"overlap_percent"`
	OverlapFacts      int64        `json:"overlap_dependency_facts"`
	OracleUnionFacts  int64        `json:"oracle_union_dependency_facts"`
	OracleUnionSHA256 string       `json:"oracle_union_set_sha256"`
	Sealed            sealedCell   `json:"sealed_v5"`
	Naive             []naiveTrial `json:"naive_trials"`
	ChecksPassed      bool         `json:"equality_checks_passed"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("B5_DSN"), "PostgreSQL DSN for the naive ledger (B5_DSN)")
	sealed := flag.String("sealed-samples", "evaluation/final-v5-wsl2/raw/formal-v113-publication-05/deployments/exposure-scale/*/raw/scale.jsonl", "glob of the sealed campaign's Scale sample files")
	trials := flag.Int("trials", 3, "independent trials; the ledger tables are dropped before every cell")
	image := flag.String("pg-image", "", "image reference of the PostgreSQL under -dsn, recorded in the output")
	out := flag.String("out", "", "output JSON path (stdout if empty)")
	flag.Parse()
	if *dsn == "" {
		fatal(fmt.Errorf("-dsn or B5_DSN is required"))
	}
	ctx := context.Background()
	sealedCells, sealedFiles, campaign, err := readSealed(*sealed)
	if err != nil {
		fatal(err)
	}
	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var pgVersion string
	if err := db.QueryRowContext(ctx, `SELECT version()`).Scan(&pgVersion); err != nil {
		fatal(err)
	}
	settings := map[string]string{}
	for _, name := range []string{"fsync", "synchronous_commit", "full_page_writes", "shared_buffers", "wal_level", "max_wal_size"} {
		var v string
		if err := db.QueryRowContext(ctx, `SELECT current_setting($1)`, name).Scan(&v); err != nil {
			fatal(err)
		}
		settings[name] = v
	}

	scales := []struct {
		Label string
		N     int64
	}{{"10k", finalv5oracle.DependencyScale10K}, {"100k", finalv5oracle.DependencyScale100K}, {"1035000", finalv5oracle.DependencyScale1035000}}
	overlaps := []int{0, 50, 90, 100}
	results := make([]cellResult, 0, len(scales)*len(overlaps))
	index := map[string]int{}
	type prepared struct {
		candidate, history []string
		unionSHA           string
	}
	inputs := map[string]prepared{}
	for _, s := range scales {
		// facts[0:2N) in the oracle's deterministic order; candidate is [0,N),
		// history is [N-K, 2N-K), exactly GenerateExposureScaleDependency's roles.
		facts := make([]string, 0, 2*s.N)
		if err := finalv5oracle.StreamExposureScaleFacts(0, 2*s.N, func(f finalv5oracle.CanonicalFact) error {
			facts = append(facts, f.SHA256)
			return nil
		}); err != nil {
			fatal(err)
		}
		for _, pct := range overlaps {
			k, err := finalv5oracle.ExposureScaleOverlapFacts(s.N, pct)
			if err != nil {
				fatal(err)
			}
			cell := fmt.Sprintf("dependency-e2e/%s-overlap-%d/novel", s.Label, pct)
			sc, ok := sealedCells[cell]
			if !ok {
				fatal(fmt.Errorf("sealed samples have no cell %s", cell))
			}
			if !sc.uniform {
				fatal(fmt.Errorf("sealed samples of %s disagree on fact cardinalities", cell))
			}
			if sc.ActualDependency != s.N || sc.ChargedDependency != s.N-k {
				fatal(fmt.Errorf("%s: sealed candidate %d charged %d, oracle expects %d and %d", cell, sc.ActualDependency, sc.ChargedDependency, s.N, s.N-k))
			}
			unionSHA, err := setDigestHex(facts[0 : 2*s.N-k])
			if err != nil {
				fatal(err)
			}
			inputs[cell] = prepared{candidate: facts[0:s.N], history: facts[s.N-k : 2*s.N-k], unionSHA: unionSHA}
			finishSealed(sc)
			index[cell] = len(results)
			results = append(results, cellResult{Cell: cell, CandidateFacts: s.N, HistoryFacts: s.N, OverlapPercent: pct, OverlapFacts: k,
				OracleUnionFacts: 2*s.N - k, OracleUnionSHA256: unionSHA, Sealed: *sc, ChecksPassed: true})
		}
	}

	limits := naiveledger.Limits{Release: math.MaxInt64 / 4, Influence: math.MaxInt64 / 4, Outcome: math.MaxInt64 / 4}
	for t := 1; t <= *trials; t++ {
		for i := range results {
			r := &results[i]
			in := inputs[r.Cell]
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
			root := fmt.Sprintf("scale-t%d-%s", t, r.Cell)
			if err := ledger.CreateRoot(ctx, root, limits); err != nil {
				fatal(err)
			}
			// Outcome: the candidate's ActualOutcome facts, of which
			// ActualOutcome-ChargedOutcome are already in the history.
			candOutcome := synthetic("outcome", r.Cell, r.Sealed.ActualOutcome)
			shared := candOutcome[:r.Sealed.ActualOutcome-r.Sealed.ChargedOutcome]
			histObs := naiveledger.Observation{QueryID: "history", Influence: in.history,
				Release: synthetic("release-history", r.Cell, 1), Outcome: append(append([]string{}, shared...), synthetic("outcome-history", r.Cell, 1)...)}
			_, seed, err := ledger.Settle(ctx, root, histObs)
			if err != nil {
				fatal(fmt.Errorf("%s trial %d seed history: %w", r.Cell, t, err))
			}
			candObs := naiveledger.Observation{QueryID: "candidate", Influence: in.candidate,
				Release: synthetic("release", r.Cell, r.Sealed.ActualRelease), Outcome: candOutcome}
			charge, m, err := ledger.Settle(ctx, root, candObs)
			if err != nil {
				fatal(fmt.Errorf("%s trial %d candidate: %w", r.Cell, t, err))
			}
			nt := naiveTrial{Trial: t, SeedHistoryMS: ms(seed.Total), LockMS: ms(m.Lock), InsertMS: ms(m.Insert), CommitMS: ms(m.Commit), TotalMS: ms(m.Total),
				ChargedRelease: charge.Release, ChargedDependency: charge.Influence, ChargedOutcome: charge.Outcome}
			storage, err := ledger.ReadStorage(ctx)
			if err != nil {
				fatal(err)
			}
			nt.FactRows, nt.TotalBytes, nt.FactTableBytes, nt.FactIndexBytes = storage.FactRows, storage.TotalBytes, storage.FactTableBytes, storage.FactIndexBytes
			if nt.RootDependency, nt.RootSetSHA256, err = rootDependency(ctx, db, root); err != nil {
				fatal(err)
			}
			// Settling the same candidate again must charge nothing.
			candObs.QueryID = "candidate-again"
			again, m2, err := ledger.Settle(ctx, root, candObs)
			if err != nil {
				fatal(fmt.Errorf("%s trial %d re-settle: %w", r.Cell, t, err))
			}
			nt.ResettleTotalMS = ms(m2.Total)
			ok := charge.Release == r.Sealed.ChargedRelease && charge.Influence == r.Sealed.ChargedDependency && charge.Outcome == r.Sealed.ChargedOutcome &&
				nt.RootDependency == r.OracleUnionFacts && nt.RootSetSHA256 == r.OracleUnionSHA256 &&
				again.Release == 0 && again.Influence == 0 && again.Outcome == 0
			r.Naive = append(r.Naive, nt)
			if !ok {
				r.ChecksPassed = false
				fatal(fmt.Errorf("%s trial %d: equality check failed: naive charge %d/%d/%d sealed %d/%d/%d, root %d (%s) oracle union %d (%s), re-settle %d/%d/%d",
					r.Cell, t, charge.Release, charge.Influence, charge.Outcome, r.Sealed.ChargedRelease, r.Sealed.ChargedDependency, r.Sealed.ChargedOutcome,
					nt.RootDependency, nt.RootSetSHA256, r.OracleUnionFacts, r.OracleUnionSHA256, again.Release, again.Influence, again.Outcome))
			}
			fmt.Fprintf(os.Stderr, "trial %d %-40s naive settle %.1f ms (seed %.1f, re-settle %.1f) v5 p50 %.1f ms rows %d bytes %d\n",
				t, r.Cell, nt.TotalMS, nt.SeedHistoryMS, nt.ResettleTotalMS, r.Sealed.SettlementP50MS, nt.FactRows, nt.TotalBytes)
		}
	}
	result := map[string]any{
		"schema_version":       1,
		"campaign_class":       "pilot",
		"publication_eligible": false,
		"design":               "docs/p10_r2_b5_ablation_design.md (same-input Scale replay, frozen before execution)",
		"method":               "naive exact-set ledger settling the sealed Scale cells' oracle Dependency sets (history then candidate, fresh ledger tables per cell, single writer); V5 side is pipeline_ms.control_settlement of the sealed campaign's novel samples of the same cells",
		"host":                 hostname(),
		"postgres_version":     pgVersion,
		"postgres_image":       *image,
		"postgres_settings":    settings,
		"git_head":             gitHead(),
		"sealed_campaign":      campaign,
		"sealed_sample_files":  sealedFiles,
		"trials":               *trials,
		"cells":                results,
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

// readSealed collects, per novel Scale cell, the settlement timings and fact
// cardinalities of the sealed campaign's measured samples.
func readSealed(glob string) (map[string]*sealedCell, map[string]string, string, error) {
	paths, err := filepath.Glob(glob)
	if err != nil || len(paths) == 0 {
		return nil, nil, "", fmt.Errorf("no sealed sample files match %s", glob)
	}
	sort.Strings(paths)
	cells := map[string]*sealedCell{}
	files := map[string]string{}
	campaign := ""
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, "", err
		}
		sum := sha256.Sum256(raw)
		files[path] = hex.EncodeToString(sum[:])
		scanner := bufio.NewScanner(strings.NewReader(string(raw)))
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<28)
		for scanner.Scan() {
			var line struct {
				Sample struct {
					CampaignID string             `json:"campaign_id"`
					CellID     string             `json:"cell_id"`
					Mode       string             `json:"mode"`
					Warmup     bool               `json:"warmup"`
					Status     string             `json:"status"`
					Pipeline   map[string]float64 `json:"pipeline_ms"`
					Diagnostic map[string]float64 `json:"diagnostic_ms"`
					ActualR    int64              `json:"actual_release_facts"`
					ActualD    int64              `json:"actual_dependency_facts"`
					ActualO    int64              `json:"actual_outcome_facts"`
					ChargedR   int64              `json:"charged_release_facts"`
					ChargedD   int64              `json:"charged_dependency_facts"`
					ChargedO   int64              `json:"charged_outcome_facts"`
				} `json:"sample"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				return nil, nil, "", fmt.Errorf("%s: %w", path, err)
			}
			s := line.Sample
			if s.Warmup || s.Mode != "novel" {
				continue
			}
			if s.Status != "pass" {
				return nil, nil, "", fmt.Errorf("%s: sealed sample of %s has status %q", path, s.CellID, s.Status)
			}
			if campaign == "" {
				campaign = s.CampaignID
			} else if campaign != s.CampaignID {
				return nil, nil, "", fmt.Errorf("sealed samples mix campaigns %s and %s", campaign, s.CampaignID)
			}
			c := cells[s.CellID]
			if c == nil {
				c = &sealedCell{uniform: true}
				cells[s.CellID] = c
			}
			if !c.firstSampleRecorded {
				c.ActualRelease, c.ActualDependency, c.ActualOutcome = s.ActualR, s.ActualD, s.ActualO
				c.ChargedRelease, c.ChargedDependency, c.ChargedOutcome = s.ChargedR, s.ChargedD, s.ChargedO
				c.firstSampleRecorded = true
			} else if c.ActualRelease != s.ActualR || c.ActualDependency != s.ActualD || c.ActualOutcome != s.ActualO ||
				c.ChargedRelease != s.ChargedR || c.ChargedDependency != s.ChargedD || c.ChargedOutcome != s.ChargedO {
				c.uniform = false
			}
			c.Samples++
			c.SettlementMS = append(c.SettlementMS, s.Pipeline["control_settlement"])
			c.FactStoreMS = append(c.FactStoreMS, s.Diagnostic["exposure_fact_store"])
		}
		if err := scanner.Err(); err != nil {
			return nil, nil, "", fmt.Errorf("%s: %w", path, err)
		}
	}
	return cells, files, campaign, nil
}

func finishSealed(c *sealedCell) {
	sort.Float64s(c.SettlementMS)
	sort.Float64s(c.FactStoreMS)
	c.SettlementP50MS, c.FactStoreP50MS = median(c.SettlementMS), median(c.FactStoreMS)
	c.SettlementMinMS, c.SettlementMaxMS = c.SettlementMS[0], c.SettlementMS[len(c.SettlementMS)-1]
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// setDigestHex is SHA-256 over the members' 32 raw bytes in ascending order.
func setDigestHex(members []string) (string, error) {
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	h := sha256.New()
	for i, m := range sorted {
		if i > 0 && sorted[i-1] == m {
			return "", fmt.Errorf("oracle set has a duplicate member %s", m)
		}
		raw, err := hex.DecodeString(m)
		if err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("oracle member %q is not a SHA-256", m)
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// rootDependency reads the root's stored Dependency set back from the naive
// ledger and digests it the way setDigestHex digests the oracle union.
func rootDependency(ctx context.Context, db *sql.DB, root string) (int64, string, error) {
	rows, err := db.QueryContext(ctx, `SELECT fact_sha256 FROM b5_naive_facts WHERE root_id = $1 AND dimension = $2 ORDER BY fact_sha256`, root, naiveledger.Influence)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, "", err
		}
		h.Write(raw)
		n++
	}
	return n, hex.EncodeToString(h.Sum(nil)), rows.Err()
}

func synthetic(dim, id string, n int64) []string {
	out := make([]string, 0, n)
	for i := int64(0); i < n; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("b5-scale-synthetic/%s/%s/%d", dim, id, i)))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
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
	fmt.Fprintln(os.Stderr, "b5-naive-scale-replay:", err)
	os.Exit(1)
}
