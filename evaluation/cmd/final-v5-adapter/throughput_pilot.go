package main

// P10.B6 throughput pilot: successful settlement throughput of shared and
// independent root families under an ample budget, outside the campaign plan.
//
// The sealed concurrency profile measures one root at its budget boundary,
// where exactly one contender per round can settle novel work. A reviewer
// asked the complementary question: when the budget is ample, how many
// legitimate novel queries does the system keep completing per second, with
// what tail latency and how many CAS conflicts and retries, when contenders
// share one root head (footprints disjoint, nested, or identical) and when
// they are spread over independent roots.
//
// Cells: roots K x width N x overlap. Each cell provisions K root tasks on the
// benign-x4 route (ample budgets) with N delegated child tasks each; every
// round, every child issues one query_sql concurrently and the round drains
// when every call has returned. Per request the record keeps client latency,
// the response's charged facts, semantic/idempotent replay flags, the
// Outcome-radix CAS telemetry, and the refusal code if any; per round it keeps
// the drain time and, per root, the ledger cardinalities before and after the
// round so the charges can be checked against the ledger. Nothing is
// resampled or discarded.
//
// Class: pilot; publication_eligible false.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const (
	throughputPilotVersion = 1
	throughputProduct      = "final_v5_result_heavy"
	throughputRoundStride  = 1000 // row_id offset between rounds, so rounds never repeat a footprint
)

var throughputOverlaps = []string{"disjoint", "nested", "identical"}

type throughputRequestRecord struct {
	Root             int     `json:"root"`
	Contender        int     `json:"contender"`
	RequestIDHash    string  `json:"request_id_hash"`
	LogicalSQLSHA256 string  `json:"logical_sql_sha256"`
	StartedOffsetMS  float64 `json:"started_offset_ms"`
	ClientMS         float64 `json:"client_ms"`
	Outcome          string  `json:"outcome"` // settled | refused | error
	Code             string  `json:"code,omitempty"`
	Message          string  `json:"message,omitempty"`
	RowCount         int64   `json:"row_count"`
	SemanticReplay   bool    `json:"semantic_replay"`
	IdempotentReplay bool    `json:"idempotent_replay"`
	ChargedRelease   int64   `json:"charged_release_facts"`
	ChargedDep       int64   `json:"charged_dependency_facts"`
	ChargedOutcome   int64   `json:"charged_outcome_facts"`
	CASAttempts      int64   `json:"cas_attempts"`
	CASConflicts     int64   `json:"cas_conflicts"`
	CASRetries       int64   `json:"cas_retries"`
	RootEpoch        int64   `json:"root_epoch"`
	// P10-R2.C4b client resubmission: attempts made (1 = no resubmission),
	// the latency of each attempt that ended in CONFLICT, and the end-to-end
	// time from the first attempt's start to the final attempt's return
	// (backoff included). ClientMS stays the final attempt's own latency so
	// the B6 columns keep their meaning.
	Attempts          int       `json:"attempts"`
	ConflictAttemptMS []float64 `json:"conflict_attempt_ms,omitempty"`
	EndToEndMS        float64   `json:"end_to_end_ms"`
}

type throughputRootLedger struct {
	Root   int                           `json:"root"`
	Before experiment.RootLedgerSnapshot `json:"before"`
	After  experiment.RootLedgerSnapshot `json:"after"`
}

type throughputRoundRecord struct {
	SchemaVersion int                       `json:"schema_version"`
	CampaignClass string                    `json:"campaign_class"`
	DeploymentID  string                    `json:"deployment_id"`
	Cell          string                    `json:"cell"`
	Roots         int                       `json:"roots"`
	Width         int                       `json:"width"`
	Overlap       string                    `json:"overlap"`
	Round         int                       `json:"round"`
	Product       string                    `json:"product"`
	BudgetProfile string                    `json:"budget_profile"`
	RootTaskHash  []string                  `json:"root_task_id_hash"`
	ClientRetries int                       `json:"client_retries"`
	DrainMS       float64                   `json:"drain_ms"`
	Requests      []throughputRequestRecord `json:"requests"`
	Ledgers       []throughputRootLedger    `json:"ledgers"`
	Summary       throughputRoundSummary    `json:"summary"`
	Error         string                    `json:"error,omitempty"`
}

type throughputRoundSummary struct {
	Settled             int     `json:"settled"`
	Novel               int     `json:"novel"`
	ZeroNovelty         int     `json:"zero_novelty"`
	Refused             int     `json:"refused"`
	Errors              int     `json:"errors"`
	SettledPerSecond    float64 `json:"settled_per_second"`
	NovelPerSecond      float64 `json:"novel_per_second"`
	ClientP50MS         float64 `json:"client_p50_ms"`
	ClientP95MS         float64 `json:"client_p95_ms"`
	ClientMaxMS         float64 `json:"client_max_ms"`
	CASAttempts         int64   `json:"cas_attempts"`
	CASConflicts        int64   `json:"cas_conflicts"`
	CASRetries          int64   `json:"cas_retries"`
	LedgerMatchesCharge bool    `json:"ledger_matches_charge"`
	// P10-R2.C4b: requests that settled only after at least one client
	// resubmission, total resubmissions issued, requests still refused after
	// the last permitted resubmission, and end-to-end latency quantiles over
	// all requests (first attempt start to final return, backoff included).
	SettledAfterRetry int     `json:"settled_after_retry"`
	Resubmissions     int     `json:"resubmissions"`
	RefusedAfterRetry int     `json:"refused_after_retry"`
	EndToEndP50MS     float64 `json:"end_to_end_p50_ms"`
	EndToEndP95MS     float64 `json:"end_to_end_p95_ms"`
	EndToEndMaxMS     float64 `json:"end_to_end_max_ms"`
}

// throughputSQL is the contender's statement: one row per contender when
// disjoint, nested prefixes of increasing length when nested, the same single
// row for everyone when identical. Rounds are offset by throughputRoundStride
// so a later round never repeats an earlier footprint within a root.
func throughputSQL(overlap string, round, contender int) string {
	base := throughputRoundStride * (round - 1)
	switch overlap {
	case "disjoint":
		return fmt.Sprintf("SELECT amount FROM %s WHERE row_id = %d", throughputProduct, base+contender)
	case "nested":
		return fmt.Sprintf("SELECT amount FROM %s WHERE row_id <= %d", throughputProduct, base+10*contender)
	case "identical":
		return fmt.Sprintf("SELECT amount FROM %s WHERE row_id = %d", throughputProduct, base+1)
	}
	return ""
}

// throughputRouteProducts is the benign-x4 approval route's exact product set
// (routes match exact sets); the ample budget is that route's profile.
func throughputRouteProducts() ([]string, map[string][]string, map[string]any) {
	products := []string{"expense_detail", "expense_summary", "provsql_orders", "provsql_lineitem", throughputProduct}
	columns := map[string][]string{}
	for _, name := range products {
		columns[name] = append([]string(nil), benignTraceColumns[name]...)
	}
	for _, decoy := range benignRouteDecoys["x4"] {
		products = append(products, decoy)
		columns[decoy] = append([]string(nil), benignDecoyColumns[decoy]...)
	}
	scopes := map[string]any{
		"department":    []string{"销售部", "研发部", "财务部"},
		"partition_key": []string{"1"},
		"category":      []string{"alpha", "beta", "gamma", "delta"},
	}
	return products, columns, scopes
}

type throughputFamily struct {
	root     provisionedTask
	children []provisionedTask
}

func provisionThroughputFamily(ctx context.Context, real *realAdapter, cell string, root, width int) (throughputFamily, error) {
	products, columns, scopes := throughputRouteProducts()
	rootTask, err := real.provisionMultiProductTask(ctx, fmt.Sprintf("P10.B6 throughput %s root %d", cell, root),
		products, columns, "", scopes)
	if err != nil {
		return throughputFamily{}, err
	}
	if rootTask.RootTaskID != rootTask.TaskID {
		return throughputFamily{}, errors.New("throughput root task was not provisioned as a root")
	}
	if rootTask.BudgetProfile != benignBudgetProfiles["x4"] {
		return throughputFamily{}, fmt.Errorf("throughput root selected budget profile %q, want %q", rootTask.BudgetProfile, benignBudgetProfiles["x4"])
	}
	family := throughputFamily{root: rootTask}
	for index := 1; index <= width; index++ {
		child, err := real.provisionMultiProductTask(ctx, fmt.Sprintf("P10.B6 throughput %s root %d contender %d", cell, root, index),
			products, columns, rootTask.TaskID, scopes)
		if err != nil {
			return family, err
		}
		if child.RootTaskID != rootTask.TaskID {
			return family, errors.New("throughput contender does not share its root")
		}
		family.children = append(family.children, child)
	}
	return family, nil
}

// throughputBackoff is the client's pause before a resubmission: 50-150 ms,
// uniformly jittered, fixed a priori (not tuned on the B6 data) to be an
// order of magnitude above the settlement's own 1-10 ms CAS backoff so the
// resubmission lands in a different head epoch, and well below the round's
// drain time so it does not hide in the tail.
func throughputBackoff(rng *rand.Rand) time.Duration {
	return 50*time.Millisecond + time.Duration(rng.Int63n(int64(100*time.Millisecond)))
}

func runThroughputRound(ctx context.Context, real *realAdapter, families []throughputFamily, record *throughputRoundRecord) error {
	for index, family := range families {
		before, err := real.rootLedgerSnapshot(ctx, family.root.TaskID)
		if err != nil {
			return err
		}
		record.Ledgers = append(record.Ledgers, throughputRootLedger{Root: index + 1, Before: before})
	}
	type call struct {
		root, contender int
		task            provisionedTask
	}
	var calls []call
	for r, family := range families {
		for c, child := range family.children {
			calls = append(calls, call{root: r + 1, contender: c + 1, task: child})
		}
	}
	results := make([]throughputRequestRecord, len(calls))
	launched := time.Now()
	var wait sync.WaitGroup
	for i := range calls {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			c := calls[i]
			sql := throughputSQL(record.Overlap, record.Round, c.contender)
			baseRequestID := fmt.Sprintf("p10-b6-%s-r%02d-%d-%03d", sha(record.Cell)[:12], record.Round, c.root, c.contender)
			rec := throughputRequestRecord{Root: c.root, Contender: c.contender,
				RequestIDHash: sha(baseRequestID), LogicalSQLSHA256: sha(sql)}
			firstStarted := time.Now()
			rec.StartedOffsetMS = durationMS(firstStarted.Sub(launched))
			rng := rand.New(rand.NewSource(int64(i) + firstStarted.UnixNano()))
			var response queryResponse
			var err error
			for attempt := 1; ; attempt++ {
				rec.Attempts = attempt
				// A CONFLICT-refused query is settled FAILED under its request_id, so an
				// idempotent retry would replay the failure; a resubmission is a new
				// request from the client's point of view and carries a new request_id.
				requestID := baseRequestID
				if attempt > 1 {
					requestID = fmt.Sprintf("%s-a%d", baseRequestID, attempt)
				}
				started := time.Now()
				response = queryResponse{}
				err = real.alice.call(ctx, "query_sql", map[string]any{
					"task_id": c.task.TaskID, "request_id": requestID, "sql": sql}, &response)
				rec.ClientMS = durationMS(time.Since(started))
				var structured *mcpCallError
				if err != nil && errors.As(err, &structured) && structured.Code == "CONFLICT" && attempt <= record.ClientRetries {
					rec.ConflictAttemptMS = append(rec.ConflictAttemptMS, rec.ClientMS)
					time.Sleep(throughputBackoff(rng))
					continue
				}
				break
			}
			rec.EndToEndMS = durationMS(time.Since(firstStarted))
			if err != nil {
				var structured *mcpCallError
				if errors.As(err, &structured) {
					rec.Outcome, rec.Code, rec.Message = "refused", structured.Code, structured.Message
				} else {
					rec.Outcome, rec.Code, rec.Message = "error", "HARNESS_ERROR", err.Error()
				}
				results[i] = rec
				return
			}
			rec.Outcome = "settled"
			rec.RowCount = response.RowCount
			rec.SemanticReplay, rec.IdempotentReplay = response.SemanticReplay, response.IdempotentReplay
			rec.ChargedRelease = response.Exposure.ChargedReleaseFacts
			rec.ChargedDep = response.Exposure.ChargedInfluenceFacts
			rec.ChargedOutcome = response.Exposure.ChargedOutcomeFacts
			rec.CASAttempts, rec.CASConflicts, rec.CASRetries = response.OutcomeRadix.CASAttempts, response.OutcomeRadix.CASConflicts, response.OutcomeRadix.CASRetries
			rec.RootEpoch = response.Exposure.RootEpoch
			results[i] = rec
		}()
	}
	wait.Wait()
	record.DrainMS = durationMS(time.Since(launched))
	record.Requests = results
	for index, family := range families {
		after, err := real.rootLedgerSnapshot(ctx, family.root.TaskID)
		if err != nil {
			return err
		}
		record.Ledgers[index].After = after
	}
	record.Summary = summarizeThroughputRound(record)
	return nil
}

func summarizeThroughputRound(record *throughputRoundRecord) throughputRoundSummary {
	var s throughputRoundSummary
	var latencies, endToEnd []float64
	chargedPerRoot := map[int][3]int64{}
	for _, r := range record.Requests {
		latencies = append(latencies, r.ClientMS)
		endToEnd = append(endToEnd, r.EndToEndMS)
		if r.Attempts > 1 {
			s.Resubmissions += r.Attempts - 1
			if r.Outcome == "settled" {
				s.SettledAfterRetry++
			}
		}
		if record.ClientRetries > 0 && r.Outcome == "refused" && r.Code == "CONFLICT" && r.Attempts > record.ClientRetries {
			s.RefusedAfterRetry++
		}
		switch r.Outcome {
		case "settled":
			s.Settled++
			if r.ChargedRelease+r.ChargedDep+r.ChargedOutcome > 0 {
				s.Novel++
			} else {
				s.ZeroNovelty++
			}
			c := chargedPerRoot[r.Root]
			c[0] += r.ChargedRelease
			c[1] += r.ChargedDep
			c[2] += r.ChargedOutcome
			chargedPerRoot[r.Root] = c
		case "refused":
			s.Refused++
		default:
			s.Errors++
		}
		s.CASAttempts += r.CASAttempts
		s.CASConflicts += r.CASConflicts
		s.CASRetries += r.CASRetries
	}
	sort.Float64s(latencies)
	if n := len(latencies); n > 0 {
		s.ClientP50MS = quantileType7(latencies, 0.50)
		s.ClientP95MS = quantileType7(latencies, 0.95)
		s.ClientMaxMS = latencies[n-1]
	}
	sort.Float64s(endToEnd)
	if n := len(endToEnd); n > 0 {
		s.EndToEndP50MS = quantileType7(endToEnd, 0.50)
		s.EndToEndP95MS = quantileType7(endToEnd, 0.95)
		s.EndToEndMaxMS = endToEnd[n-1]
	}
	if record.DrainMS > 0 {
		s.SettledPerSecond = float64(s.Settled) / (record.DrainMS / 1000)
		s.NovelPerSecond = float64(s.Novel) / (record.DrainMS / 1000)
	}
	// Charges reported by the responses must equal the ledger movement of each
	// root over the round; a mismatch is recorded, never repaired.
	s.LedgerMatchesCharge = true
	for _, l := range record.Ledgers {
		c := chargedPerRoot[l.Root]
		if l.After.ReleaseCardinality-l.Before.ReleaseCardinality != c[0] ||
			l.After.DependencyCardinality-l.Before.DependencyCardinality != c[1] ||
			l.After.OutcomeCardinality-l.Before.OutcomeCardinality != c[2] {
			s.LedgerMatchesCharge = false
		}
	}
	return s
}

// quantileType7 is Hyndman-Fan type 7 on sorted data, the paper's convention.
func quantileType7(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	h := float64(n-1) * q
	lo := int(h)
	if lo+1 >= n {
		return sorted[n-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

func runThroughputPilot(ctx context.Context, outPath, deploymentID string, roots, widths []int, overlaps []string, rounds, clientRetries int) error {
	if clientRetries < 0 {
		return fmt.Errorf("client retries must be >= 0, got %d", clientRetries)
	}
	for _, o := range overlaps {
		if !containsString(throughputOverlaps, o) {
			return fmt.Errorf("unknown overlap %q", o)
		}
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return err
	}
	defer real.Close()
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	encoder := json.NewEncoder(out)
	for _, k := range roots {
		for _, n := range widths {
			for _, overlap := range overlaps {
				cell := fmt.Sprintf("roots%d-width%d-%s", k, n, overlap)
				families := make([]throughputFamily, 0, k)
				var provisionErr error
				for r := 1; r <= k && provisionErr == nil; r++ {
					var family throughputFamily
					family, provisionErr = provisionThroughputFamily(ctx, real, cell, r, n)
					families = append(families, family)
				}
				for round := 1; round <= rounds; round++ {
					record := throughputRoundRecord{SchemaVersion: throughputPilotVersion, CampaignClass: "pilot",
						DeploymentID: deploymentID, Cell: cell, Roots: k, Width: n, Overlap: overlap, Round: round,
						Product: throughputProduct, ClientRetries: clientRetries}
					for _, f := range families {
						record.RootTaskHash = append(record.RootTaskHash, sha(f.root.TaskID))
						record.BudgetProfile = f.root.BudgetProfile
					}
					if provisionErr != nil {
						record.Error = "provisioning: " + provisionErr.Error()
					} else if err := runThroughputRound(ctx, real, families, &record); err != nil {
						record.Error = err.Error()
					}
					if err := encoder.Encode(record); err != nil {
						return err
					}
					fmt.Fprintf(os.Stderr, "throughput-pilot %s round %d drain=%.0fms settled=%d novel=%d refused=%d err=%d p95=%.0fms cas=%d/%d/%d ledger_ok=%v retries=%d settled_after_retry=%d resubmissions=%d refused_after_retry=%d e2e_p95=%.0fms %s\n",
						cell, round, record.DrainMS, record.Summary.Settled, record.Summary.Novel, record.Summary.Refused, record.Summary.Errors,
						record.Summary.ClientP95MS, record.Summary.CASAttempts, record.Summary.CASConflicts, record.Summary.CASRetries,
						record.Summary.LedgerMatchesCharge, clientRetries, record.Summary.SettledAfterRetry, record.Summary.Resubmissions,
						record.Summary.RefusedAfterRetry, record.Summary.EndToEndP95MS, strings.TrimSpace(record.Error))
					if provisionErr != nil {
						break
					}
				}
			}
		}
	}
	return nil
}
