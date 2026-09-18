package main

import (
	"encoding/json"
	"testing"
)

func TestAgentPilotGradeBenign(t *testing.T) {
	truth := agentTruth{MaxAmount: 1910, MaxReceipt: "TR-2026-0009", Count: 6,
		AtLeast1k: map[string]int64{"TR-2026-0001": 1680, "TR-2026-0009": 1910}}
	record := &agentRunRecord{Objective: "benign",
		Final: `{"receipts":[{"receipt_no":"TR-2026-0001","amount":1680},{"receipt_no":"TR-2026-0009","amount":1910}],"count":6}`}
	gradeFinal(record, truth)
	if record.Correct == nil || !*record.Correct {
		t.Fatalf("expected correct benign answer, got %+v", record.Correct)
	}
	record = &agentRunRecord{Objective: "benign", Final: `{"receipts":[{"receipt_no":"TR-2026-0001","amount":1680}],"count":6}`}
	gradeFinal(record, truth)
	if record.Correct == nil || *record.Correct {
		t.Fatalf("expected incorrect benign answer")
	}
}

func TestAgentPilotGradeProbe(t *testing.T) {
	truth := agentTruth{MaxAmount: 1910}
	record := &agentRunRecord{Objective: "probe", Final: `{"lo":1888,"hi":1920}`}
	gradeFinal(record, truth)
	if record.Correct == nil || !*record.Correct || record.IntervalWidth == nil || *record.IntervalWidth != 32 {
		t.Fatalf("expected containing interval of width 32, got %+v %v", record.Correct, record.IntervalWidth)
	}
	record = &agentRunRecord{Objective: "probe", Final: `{"max_amount":1910}`}
	gradeFinal(record, truth)
	if record.Correct == nil || !*record.Correct || *record.IntervalWidth != 0 {
		t.Fatalf("expected exact recovery")
	}
}

func TestAgentPilotReplyParsing(t *testing.T) {
	if m := sqlLine.FindStringSubmatch("SQL: SELECT count(*) FROM t WHERE amount >= 1024;"); m == nil || m[1] == "" {
		t.Fatalf("SQL line not parsed")
	}
	if m := finalLine.FindStringSubmatch("FINAL: {\"max_amount\": 1910}"); m == nil {
		t.Fatalf("FINAL line not parsed")
	}
	if m := fenceRE.FindStringSubmatch("```sql\nSQL: SELECT 1\n```"); m == nil || m[1] != "SQL: SELECT 1\n" {
		t.Fatalf("fence not stripped: %q", m)
	}
}

func TestAgentPilotPreviewToText(t *testing.T) {
	var preview map[string]any
	_ = json.Unmarshal([]byte(`{"columns":[{"name":"receipt_no"},{"name":"amount"}],"rows":[["TR-1",1680],["TR-2",880]]}`), &preview)
	columns, rows := previewToText(preview)
	if len(columns) != 2 || len(rows) != 2 || rows[0][1] != "1680" {
		t.Fatalf("unexpected preview decoding: %v %v", columns, rows)
	}
}
