//go:build taskgate_scale

// These cases prepare an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so they belong on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

func TestDelegatedTaskSharesRootExposureAndStopsWithParent(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV5SummaryTask(t, "task-family-root", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 5})
	bob := mcp.Principal{ID: "principal-bob-agent", Subject: "bob-agent", Role: "query"}
	if err := harness.store.CreatePrincipal(context.Background(), control.Principal{
		ID: bob.ID, Subject: bob.Subject, Role: bob.Role, CreatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create delegated principal: %v", err)
	}

	request := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":      "delegate the approved monthly summary",
		"parent_task_id": "task-family-root", "delegate_principal_id": bob.ID,
		"data_products": []string{"expense_summary"},
		"columns":       map[string][]string{"expense_summary": {"month", "total_amount"}},
		"scopes":        map[string]any{"department": []any{"销售部"}},
	})
	// expense_summary resolves through the default low route now that the frozen
	// benchmark relations share it. Only the profile name moved: the assertions
	// below still require every delegated limit to be the parent's, because
	// constrainDelegatedBudget intersects rather than inherits.
	if request["budget_source"] != "catalog_profile_intersect_parent_grant" || request["budget_profile"] != "final-v5-baseline-low-v1" {
		t.Fatalf("delegated budget provenance = %#v", request)
	}
	delegatedBudget, ok := request["budget"].(map[string]any)
	if !ok || delegatedBudget["max_release_facts"] != int64(20) || delegatedBudget["max_influence_facts"] != int64(20) || delegatedBudget["max_outcome_facts"] != int64(5) {
		t.Fatalf("delegated budget was not intersected with the parent grant: %#v", request["budget"])
	}
	if delegatedBudget["exposure_profile_version"] != exposure.ProfileV5 || delegatedBudget["predicate_footprint"] == nil {
		t.Fatalf("delegated task changed its parent root-ledger semantics: %#v", request["budget"])
	}
	childID := request["task_id"].(string)
	child, err := harness.store.GetTask(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child task: %v", err)
	}
	if child.RootTaskID != "task-family-root" || child.ParentTaskID != "task-family-root" || child.PrincipalID != bob.ID {
		t.Fatalf("child lineage = %+v", child)
	}
	draft := harness.approval.requests[len(harness.approval.requests)-1]
	if draft.Manifest.RootTaskID != "task-family-root" || draft.Manifest.ParentTaskID != "task-family-root" ||
		draft.Manifest.HumanSubject != harness.alice.Subject || draft.Manifest.AgentID != bob.ID {
		t.Fatalf("delegated manifest lineage = %+v", draft.Manifest)
	}

	submitted := oaCallbackEvent{
		EventID: "oa-family-submit", TaskID: childID, DraftID: child.ApprovalRef,
		Status: "submitted", Actor: harness.alice.Subject, OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
	}
	if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
		t.Fatalf("delegated submit = %d %s", response.Code, response.Body.String())
	}
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build delegated core: %v", err)
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("hash delegated core: %v", err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "oa-family-receipt", TaskID: childID,
		Decision: approval.ApprovalDecisionApprove, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: coreDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign delegated approval: %v", err)
	}
	approved := oaCallbackEvent{
		EventID: "oa-family-approve", TaskID: childID, DraftID: child.ApprovalRef,
		Status: "approved", Actor: "bob", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &core, ApprovalReceipt: &receipt,
	}
	if response := sendGatewayCallback(t, harness, approved, ""); response.Code != http.StatusOK {
		t.Fatalf("delegated approval = %d %s", response.Code, response.Body.String())
	}

	indexes := harness.installCatalogV4SnapshotRegistry(t)
	ordinalPlan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	bound := prepareOrdinalForTest(t, harness, childID, ordinalPlan)
	entityKey, err := exposure.ComposeCanonicalKeyV2("base-entity",
		"travel.expense_summary",
		"month", "text", "s:2026-01",
		"department", "text", "s:销售部",
		"expense_type", "text", "s:机票",
	)
	if err != nil {
		t.Fatalf("compose V4 fixture entity key: %v", err)
	}
	handle, found := indexes["expense-summary-v1"].LookupRowHandle(entityKey)
	if !found {
		t.Fatalf("V4 fixture entity %q has no row handle", entityKey)
	}
	provenanceValues := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票", "total_amount": "1680.00",
		bound.Program.Sources[0].HandleAlias: uint64(handle),
	}
	provenanceColumns := make([]dataconnector.Column, 0, len(bound.ProvenanceFields))
	provenanceRow := make([]any, 0, len(bound.ProvenanceFields))
	for _, field := range bound.ProvenanceFields {
		value, present := provenanceValues[field]
		if !present {
			t.Fatalf("V4 fixture has no provenance value for %q", field)
		}
		provenanceColumns = append(provenanceColumns, dataconnector.Column{Name: field})
		provenanceRow = append(provenanceRow, value)
	}
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month"}, {Name: "total_amount"}},
		Rows:    [][]any{{"2026-01", "1680.00"}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	plan := map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}}
	rootResult := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-family-root", "request_id": "family-root-query", "plan": plan,
	})
	childResult := mustCallGatewayTool(t, harness.service, bob, "execute_plan", map[string]any{
		"task_id": childID, "request_id": "family-child-query", "plan": plan,
	})
	if rootResult["exposure"].(control.ExposureCharge).ChargedReleaseFacts == 0 ||
		childResult["exposure"].(control.ExposureCharge).ChargedReleaseFacts != 0 ||
		childResult["exposure"].(control.ExposureCharge).RootTaskID != "task-family-root" {
		t.Fatalf("family exposure was not conserved: root=%+v child=%+v", rootResult["exposure"], childResult["exposure"])
	}

	mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{
		"task_id": "task-family-root", "reason": "delegation test",
	})
	_, err = callGatewayTool(harness.service, bob, "execute_plan", map[string]any{
		"task_id": childID, "request_id": "family-child-after-revoke", "plan": plan,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
}
