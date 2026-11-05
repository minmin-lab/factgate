package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

// The repository Catalog (config/catalog.yaml) declares the eligibility rule
// these tests exercise: expense_detail requires job_family in
// {hr, finance, finance_controller} and bounds department to the subject's own
// department unless the subject is a finance_controller. The default harness
// loads it, so the tests run against exactly what the demo deployment activates.
func requireDemoEligibilityRule(t *testing.T, harness *gatewayHarness) {
	t.Helper()
	for _, product := range harness.catalog.Products {
		if product.Name != "expense_detail" {
			if product.Eligibility != nil {
				t.Fatalf("product %s unexpectedly declares eligibility; tests assume only expense_detail does", product.Name)
			}
			continue
		}
		rule := product.Eligibility
		if rule == nil || len(rule.Require) != 1 || rule.Require[0].Attribute != "job_family" ||
			strings.Join(rule.Require[0].AnyOf, ",") != "hr,finance,finance_controller" {
			t.Fatalf("expense_detail require rule = %+v, want job_family in hr,finance,finance_controller", rule)
		}
		envelope, ok := rule.ScopeEnvelope["department"]
		if !ok || envelope.FromAttribute != "department" || len(envelope.WidenIf) != 1 ||
			envelope.WidenIf[0].Attribute != "job_family" || strings.Join(envelope.WidenIf[0].AnyOf, ",") != "finance_controller" {
			t.Fatalf("expense_detail department envelope = %+v", rule.ScopeEnvelope)
		}
		return
	}
	t.Fatal("expense_detail not found in the repository Catalog")
}

func attributeSnapshot(t *testing.T, jobFamily string, withGrant bool) *principalattr.Snapshot {
	t.Helper()
	text := "version: taskgate-principal-attributes-v1\nsubjects:\n  - subject: alice\n    attributes:\n      department: [财务部]\n      job_family: [" + jobFamily + "]\n"
	if withGrant {
		text += "    grants:\n      - attribute: department\n        value: 销售部\n        valid_from: \"2026-07-21T12:10:00Z\"\n        valid_until: \"2026-07-21T12:20:00Z\"\n"
	}
	snapshot, err := principalattr.Parse([]byte(text))
	if err != nil {
		t.Fatalf("parse attribute snapshot: %v", err)
	}
	return snapshot
}

func newEligibilityHarness(t *testing.T, jobFamily string, withGrant bool) (*gatewayHarness, *principalattr.Snapshot) {
	t.Helper()
	snapshot := attributeSnapshot(t, jobFamily, withGrant)
	harness := newGatewayHarnessWithOptions(t, harnessOptions{principalAttributes: snapshot})
	requireDemoEligibilityRule(t, harness)
	return harness, snapshot
}

func detailRequest(departments ...string) map[string]any {
	values := make([]any, 0, len(departments))
	for _, department := range departments {
		values = append(values, department)
	}
	return map[string]any{
		"objective":     "audit reimbursements of my own department",
		"data_products": []string{"expense_detail"},
		"columns":       map[string][]string{"expense_detail": {"receipt_no", "amount"}},
		"scopes":        map[string]any{"department": values},
	}
}

func requireNothingReachedApproval(t *testing.T, harness *gatewayHarness) {
	t.Helper()
	if len(harness.approval.requests) != 0 {
		t.Fatalf("ineligible request reached OA; drafts = %d", len(harness.approval.requests))
	}
	tasks, err := harness.store.ListTasks(context.Background(), control.TaskFilter{PrincipalID: harness.alice.ID, Limit: 10})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("ineligible request was persisted; tasks = %d", len(tasks))
	}
}

func persistedVerdict(t *testing.T, harness *gatewayHarness, taskID string) *eligibilityVerdict {
	t.Helper()
	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load task: %v", err)
	}
	pending, err := decodePending(task)
	if err != nil {
		t.Fatalf("decode pending context: %v", err)
	}
	return pending.Eligibility
}

func TestEligibilityAdmitsRequestInsideEnvelopeAndRecordsVerdict(t *testing.T) {
	harness, snapshot := newEligibilityHarness(t, "hr", false)
	request := detailRequest("财务部")
	request["data_products"] = []string{"expense_summary", "expense_detail"}
	request["columns"] = map[string][]string{"expense_summary": {"month", "total_amount"}, "expense_detail": {"receipt_no", "amount"}}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", request)
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA drafts = %d, want 1", len(harness.approval.requests))
	}
	draft := harness.approval.requests[0]
	if draft.Manifest.Version != domain.AuthorizationManifestV1Version {
		t.Fatalf("manifest version changed: %s", draft.Manifest.Version)
	}
	verdict := persistedVerdict(t, harness, result["task_id"].(string))
	if verdict == nil {
		t.Fatal("pending context carries no eligibility verdict")
	}
	if verdict.Version != EligibilityVerdictV1 || verdict.SnapshotSHA256 != snapshot.SHA256 || verdict.Subject != harness.alice.Subject ||
		!verdict.EvaluatedAt.Equal(harness.clock.value) {
		t.Fatalf("verdict header = %+v", verdict)
	}
	if len(verdict.Rules) != 1 || verdict.Rules[0].Product != "expense_detail" {
		t.Fatalf("verdict rules = %+v, want expense_detail only (expense_summary declares no eligibility)", verdict.Rules)
	}
	rule := verdict.Rules[0]
	if len(rule.Require) != 1 || rule.Require[0].Attribute != "job_family" || strings.Join(rule.Require[0].Matched, ",") != "hr" {
		t.Fatalf("require trace = %+v", rule.Require)
	}
	envelope, ok := rule.Envelope["department"]
	if !ok || envelope.Source != envelopeSourceAttribute || strings.Join(envelope.Admissible, ",") != "财务部" || strings.Join(envelope.Requested, ",") != "财务部" {
		t.Fatalf("envelope trace = %+v", rule.Envelope)
	}
}

func TestEligibilityRejectsSubjectOutsideRequire(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "analyst", false)
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("财务部"))
	requireToolCode(t, err, apierr.CodeNotEligible)
	if !strings.Contains(err.Error(), "expense_detail") || strings.Contains(err.Error(), "analyst") {
		t.Fatalf("rejection must name the product and not echo attribute values: %v", err)
	}
	requireNothingReachedApproval(t, harness)
	// A Product without eligibility stays open to the same subject.
	mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective": "monthly summary", "data_products": []string{"expense_summary"},
		"columns": map[string][]string{"expense_summary": {"month", "total_amount"}},
		"scopes":  map[string]any{"department": []any{"销售部"}},
	})
}

func TestEligibilityRejectsScopeOutsideEnvelope(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "hr", false)
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("销售部"))
	requireToolCode(t, err, apierr.CodeNotEligible)
	// Partial overreach is still overreach: the envelope is a subset test.
	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("财务部", "销售部"))
	requireToolCode(t, err, apierr.CodeNotEligible)
	requireNothingReachedApproval(t, harness)
}

func TestEligibilityWidensEnvelopeWhenConditionHolds(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "finance_controller", false)
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", detailRequest("研发部", "销售部"))
	verdict := persistedVerdict(t, harness, result["task_id"].(string))
	envelope := verdict.Rules[0].Envelope["department"]
	if envelope.Source != envelopeSourceWidened || strings.Join(envelope.Admissible, ",") != "研发部,财务部,销售部" ||
		strings.Join(envelope.Requested, ",") != "研发部,销售部" {
		t.Fatalf("widened envelope trace = %+v", envelope)
	}
	// Widening admits only Catalog values; normalizeScopes already rejects others.
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("人事部"))
	requireToolCode(t, err, apierr.CodeInvalidRequest)
}

func TestEligibilityHonoursTemporaryGrantWindow(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "hr", true)
	windowStart := time.Date(2026, 7, 21, 12, 10, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 7, 21, 12, 20, 0, 0, time.UTC)

	harness.clock.value = windowStart.Add(-time.Second)
	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("销售部"))
	requireToolCode(t, err, apierr.CodeNotEligible)

	harness.clock.value = windowStart.Add(5 * time.Minute)
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", detailRequest("销售部"))
	verdict := persistedVerdict(t, harness, result["task_id"].(string))
	envelope := verdict.Rules[0].Envelope["department"]
	if envelope.Source != envelopeSourceAttribute || strings.Join(envelope.Admissible, ",") != "财务部,销售部" ||
		!verdict.EvaluatedAt.Equal(harness.clock.value) {
		t.Fatalf("grant-window envelope trace = %+v at %v", envelope, verdict.EvaluatedAt)
	}

	harness.clock.value = windowEnd
	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("销售部"))
	requireToolCode(t, err, apierr.CodeNotEligible)
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA drafts = %d, want exactly the in-window request", len(harness.approval.requests))
	}
}

func TestEligibilityDelegationIsBoundedByDelegatorAttributes(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "hr", true)
	// The parent grant admits 销售部 and lives 30 minutes from 12:00; the child
	// must still fit the delegator's envelope at request time, which only the
	// 12:10-12:20 temporary grant provides.
	harness.createTaskWithGrantAndExposureProfile(t, "task-detail-root", nil, control.ExposureLimits{}, "taskgate-exposure-v1",
		[]string{"expense_detail"}, map[string][]string{"expense_detail": {"receipt_no", "amount"}}, domain.SensitivityHigh)
	bob := mcp.Principal{ID: "principal-bob-agent", Subject: "bob-agent", Role: "query"}
	if err := harness.store.CreatePrincipal(context.Background(), control.Principal{
		ID: bob.ID, Subject: bob.Subject, Role: bob.Role, CreatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create delegated principal: %v", err)
	}
	child := detailRequest("销售部")
	child["parent_task_id"] = "task-detail-root"
	child["delegate_principal_id"] = bob.ID

	_, err := callGatewayTool(harness.service, harness.alice, "request_data_task", child)
	requireToolCode(t, err, apierr.CodeNotEligible)
	if len(harness.approval.requests) != 0 {
		t.Fatalf("ineligible delegation reached OA")
	}

	harness.clock.value = time.Date(2026, 7, 21, 12, 15, 0, 0, time.UTC)
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", child)
	if result["delegate_principal_id"] != bob.ID {
		t.Fatalf("delegation result = %#v", result)
	}
	verdict := persistedVerdict(t, harness, result["task_id"].(string))
	if verdict == nil || verdict.Subject != harness.alice.Subject {
		t.Fatalf("delegated verdict must be evaluated for the delegator; got %+v", verdict)
	}
}

func TestEligibilityFailsClosedForUnknownSubject(t *testing.T) {
	snapshot, err := principalattr.Parse([]byte("version: taskgate-principal-attributes-v1\nsubjects:\n  - subject: zed\n    attributes:\n      department: [财务部]\n      job_family: [hr]\n"))
	if err != nil {
		t.Fatal(err)
	}
	harness := newGatewayHarnessWithOptions(t, harnessOptions{principalAttributes: snapshot})
	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", detailRequest("财务部"))
	requireToolCode(t, err, apierr.CodeNotEligible)
	requireNothingReachedApproval(t, harness)
}

func TestNewRejectsEligibilityCatalogWithoutAttributeSnapshot(t *testing.T) {
	harness, _ := newEligibilityHarness(t, "hr", false)
	_, err := New(Config{
		Catalog: harness.catalog, Store: harness.store, Approval: harness.approval,
		Connector: harness.connector, CallbackSecret: harness.secret,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil || !strings.Contains(err.Error(), "principal attribute snapshot") {
		t.Fatalf("New error = %v, want missing-snapshot refusal", err)
	}
	// The default harness loads config/principal-attributes.yaml for the
	// repository Catalog, which declares eligibility on expense_detail.
	plain := newGatewayHarness(t)
	if plain.service.principalAttributes == nil || !plain.service.principalAttributes.Known("alice") {
		t.Fatal("default harness did not load the repository attribute snapshot")
	}
}

func TestLegacyPendingContextWithoutEligibilityStillDecodes(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-legacy")
	task, err := harness.store.GetTask(context.Background(), "task-legacy")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := decodePending(task)
	if err != nil {
		t.Fatalf("legacy pending context failed to decode: %v", err)
	}
	if pending.Eligibility != nil {
		t.Fatalf("legacy context unexpectedly carries eligibility: %+v", pending.Eligibility)
	}
}
