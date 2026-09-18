//go:build taskgate_scale

// This case prepares an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Most
// of those are scanned out of the Business database, so like the callback
// scale cases it lives behind taskgate_scale and runs where the full snapshot
// registry is materialised.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

// P9.D end-to-end: a derived arithmetic projection lowers, executes, settles
// with the derived Release identity and the argument-cell dependency, and
// replays at zero charge.
func TestDerivedProjectionSettlesEndToEnd(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)
	taskID := "task-derived-projection-e2e"
	harness.createExposureV4SummaryTask(t, taskID,
		control.ExposureLimits{ReleaseFacts: 100, InfluenceFacts: 100, OutcomeFacts: 10})

	sql := `SELECT month, (total_amount * 2) AS doubled FROM expense_summary ORDER BY month ASC`
	products := map[string]queryplan.Product{}
	grant, err := harness.store.GetGrant(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range grant.ApprovedProducts {
		product, found := harness.catalog.LookupProduct(name)
		if !found {
			t.Fatalf("catalog misses product %q", name)
		}
		products[name] = relationalQueryProduct(product, stringSetFromSlice(grant.ApprovedColumns[name]))
	}
	lowered, lowerErr := sqllowering.Lower(sql, products)
	if lowerErr != nil {
		t.Fatalf("derived SQL must lower: %v", lowerErr)
	}
	if len(lowered.Plan.Derived) != 1 {
		t.Fatalf("plan derived = %+v", lowered.Plan.Derived)
	}
	bound := prepareOrdinalForTest(t, harness, taskID, lowered.Plan)
	row := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票",
		"total_amount": json.Number("1680.00"), "doubled": json.Number("3360.00"),
	}
	visible := scanVisibleResult(t, bound.Program, []map[string]any{row})
	for index := range visible.Columns {
		switch visible.Columns[index].Name {
		case "month":
			visible.Columns[index].DataTypeOID = 25
		default:
			visible.Columns[index].DataTypeOID = 1700
		}
	}
	visible.DatabaseTime = 2 * time.Millisecond
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRow := make([]any, len(provenanceColumns))
	for _, source := range bound.Program.Sources {
		entityKey := ordinalFixtureEntityKey(t, source, row)
		handle, present := bound.Indexes[source.SourceAlias].LookupRowHandle(entityKey)
		if !present {
			t.Fatalf("snapshot index misses entity %q", entityKey)
		}
		provenanceRow[positions[source.HandleAlias]] = uint64(handle)
		for _, field := range source.EvidenceFields {
			provenanceRow[positions[field.ProvenanceAlias]] = row[field.Column]
		}
	}
	harness.connector.result = visible
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}

	firstResult, firstErr := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": "derived-e2e-novel", "sql": sql,
	})
	if firstErr != nil {
		var tool *mcp.ToolError
		if errors.As(firstErr, &tool) && tool.Cause != nil {
			t.Fatalf("novel derived query failed: %v (cause: %v)", firstErr, tool.Cause)
		}
		t.Fatalf("novel derived query failed: %v", firstErr)
	}
	first := firstResult
	if first["row_count"] != int64(1) || first["semantic_replay"] == true {
		t.Fatalf("novel derived query = %#v", first)
	}
	charge := first["exposure"].(control.ExposureCharge)
	if charge.ChargedReleaseFacts < 2 {
		t.Fatalf("derived novel release charge = %+v, want month cell plus derived fact", charge)
	}
	if charge.ChargedInfluenceFacts < 2 {
		t.Fatalf("derived novel influence charge = %+v, want row plus argument cell", charge)
	}

	second := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": taskID, "request_id": "derived-e2e-replay", "sql": sql,
	})
	if second["semantic_replay"] != true {
		t.Fatalf("second derived query did not replay: %#v", second)
	}
	replayCharge := second["exposure"].(control.ExposureCharge)
	if replayCharge.ChargedReleaseFacts != 0 || replayCharge.ChargedInfluenceFacts != 0 ||
		replayCharge.ChargedOutcomeFacts != 0 {
		t.Fatalf("derived replay charged novelty: %+v", replayCharge)
	}
}
