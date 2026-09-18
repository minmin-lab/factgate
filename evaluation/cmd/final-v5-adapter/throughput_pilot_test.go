package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func TestThroughputSQLShapes(t *testing.T) {
	if got := throughputSQL("disjoint", 1, 7); !strings.HasSuffix(got, "WHERE row_id = 7") {
		t.Fatalf("disjoint: %s", got)
	}
	if got := throughputSQL("disjoint", 2, 7); !strings.HasSuffix(got, "WHERE row_id = 1007") {
		t.Fatalf("disjoint round offset: %s", got)
	}
	if got := throughputSQL("nested", 1, 3); !strings.HasSuffix(got, "WHERE row_id <= 30") {
		t.Fatalf("nested: %s", got)
	}
	a, b := throughputSQL("identical", 1, 1), throughputSQL("identical", 1, 50)
	if a != b || !strings.HasSuffix(a, "WHERE row_id = 1") {
		t.Fatalf("identical: %s vs %s", a, b)
	}
	if throughputSQL("bogus", 1, 1) != "" {
		t.Fatal("unknown overlap must produce no SQL")
	}
	// the route is the benign-x4 exact product set
	products, columns, _ := throughputRouteProducts()
	if len(products) != 7 || len(columns) != 7 {
		t.Fatalf("route products %v", products)
	}
	if !containsString(columns[throughputProduct], "row_id") || !containsString(columns[throughputProduct], "amount") {
		t.Fatalf("result_heavy grant must cover row_id and amount: %v", columns[throughputProduct])
	}
}

func TestThroughputSummaryLedgerCheck(t *testing.T) {
	record := &throughputRoundRecord{DrainMS: 2000, Ledgers: []throughputRootLedger{{Root: 1,
		Before: experiment.RootLedgerSnapshot{ReleaseCardinality: 10, DependencyCardinality: 20, OutcomeCardinality: 5},
		After:  experiment.RootLedgerSnapshot{ReleaseCardinality: 12, DependencyCardinality: 26, OutcomeCardinality: 9}}}}
	record.Requests = []throughputRequestRecord{
		{Root: 1, Outcome: "settled", ClientMS: 40, ChargedRelease: 1, ChargedDep: 3, ChargedOutcome: 2, CASAttempts: 1},
		{Root: 1, Outcome: "settled", ClientMS: 60, ChargedRelease: 1, ChargedDep: 3, ChargedOutcome: 2, CASAttempts: 2, CASConflicts: 1, CASRetries: 1},
		{Root: 1, Outcome: "settled", ClientMS: 50, SemanticReplay: true},
		{Root: 1, Outcome: "refused", ClientMS: 30, Code: "EXPOSURE_BUDGET_EXHAUSTED"},
	}
	s := summarizeThroughputRound(record)
	if s.Settled != 3 || s.Novel != 2 || s.ZeroNovelty != 1 || s.Refused != 1 || s.Errors != 0 {
		t.Fatalf("counts %+v", s)
	}
	if s.SettledPerSecond != 1.5 || s.NovelPerSecond != 1.0 {
		t.Fatalf("rates %+v", s)
	}
	if s.CASAttempts != 3 || s.CASConflicts != 1 || s.CASRetries != 1 {
		t.Fatalf("cas %+v", s)
	}
	if !s.LedgerMatchesCharge {
		t.Fatalf("ledger movement 2/6/4 equals the charges 2/6/4")
	}
	if s.ClientP50MS != 45 || s.ClientMaxMS != 60 {
		t.Fatalf("latency %+v", s)
	}
	record.Ledgers[0].After.OutcomeCardinality = 10
	if summarizeThroughputRound(record).LedgerMatchesCharge {
		t.Fatal("a ledger that moved more than the responses charged must be flagged")
	}
}

func TestQuantileType7(t *testing.T) {
	data := []float64{1, 2, 3, 4}
	if q := quantileType7(data, 0.5); q != 2.5 {
		t.Fatalf("p50 %v", q)
	}
	if q := quantileType7(data, 0.95); q < 3.849 || q > 3.851 {
		t.Fatalf("p95 %v", q)
	}
	if q := quantileType7(data, 1); q != 4 {
		t.Fatalf("p100 %v", q)
	}
}
