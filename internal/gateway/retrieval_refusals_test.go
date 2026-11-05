package gateway

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// requireToolDetails asserts the refusal's code and returns its details.
func requireToolDetails(t *testing.T, err error, code string) map[string]any {
	t.Helper()
	requireToolCode(t, err, code)
	var toolErr *mcp.ToolError
	errors.As(err, &toolErr)
	if toolErr.Details == nil {
		t.Fatalf("%s refusal carries no details: %v", code, err)
	}
	return toolErr.Details
}

// detailsJSON compares details through their wire encoding, as the agent
// receives them.
func detailsJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func kbRefusalTask(t *testing.T, harness *gatewayHarness, taskID string, products []string, columns map[string][]string) {
	t.Helper()
	harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
		control.ExposureLimits{ReleaseFacts: 400, InfluenceFacts: 2000, OutcomeFacts: 64}, exposure.ProfileV5,
		products, columns, domain.SensitivityHigh,
		map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal"}})
}

func TestRetrievalRefusalsNameWhatTheRequestOrTaskLacks(t *testing.T) {
	harness, fullTask := kbSearchHarness(t, exposure.ProfileV5)
	search := func(taskID, requestID string, overrides map[string]any) error {
		args := map[string]any{"task_id": taskID, "request_id": requestID, "knowledge_product": "policy_kb",
			"query": map[string]any{"vector": kbVector(256)}, "k": 5}
		for key, value := range overrides {
			args[key] = value
		}
		_, err := callGatewayTool(harness.service, harness.alice, "search", args)
		return err
	}

	t.Run("relation name instead of the knowledge product", func(t *testing.T) {
		details := requireToolDetails(t, search(fullTask, "refusal-relation-name", map[string]any{"knowledge_product": "policy_kb_chunks"}), apierr.CodeInvalidRequest)
		if details["reason"] != "UNKNOWN_KNOWLEDGE_PRODUCT" || details["suggested_knowledge_product"] != "policy_kb" || details["relation_role"] != "chunks" {
			t.Fatalf("details %v do not point at policy_kb", details)
		}
		unknown := requireToolDetails(t, search(fullTask, "refusal-unknown", map[string]any{"knowledge_product": "nope"}), apierr.CodeInvalidRequest)
		if _, suggested := unknown["suggested_knowledge_product"]; suggested || detailsJSON(t, unknown["knowledge_products"]) != `["policy_kb"]` {
			t.Fatalf("unknown-name details %v", unknown)
		}
	})

	t.Run("documents relation not granted", func(t *testing.T) {
		kbRefusalTask(t, harness, "task-kb-refusal-chunks-only", []string{"policy_kb_chunks"},
			map[string][]string{"policy_kb_chunks": {"doc_id", "version", "chunk_index"}})
		details := requireToolDetails(t, search("task-kb-refusal-chunks-only", "refusal-relations", nil), apierr.CodePolicyDenied)
		if details["reason"] != "RELATIONS_NOT_GRANTED" || detailsJSON(t, details["missing_products"]) != `["policy_kb_documents"]` {
			t.Fatalf("details %v", details)
		}
	})

	// The task alice approved on 2026-10-20 (task_82dbe638): both relations,
	// identity columns only. The refusal names text_sha256, plus the column of
	// every attribute the search filters on.
	kbRefusalTask(t, harness, "task-kb-refusal-identity-only", []string{"policy_kb_chunks", "policy_kb_documents"},
		map[string][]string{"policy_kb_chunks": {"doc_id", "version", "chunk_index"}, "policy_kb_documents": {"doc_id", "version"}})
	t.Run("columns not granted", func(t *testing.T) {
		details := requireToolDetails(t, search("task-kb-refusal-identity-only", "refusal-columns", nil), apierr.CodePolicyDenied)
		if details["reason"] != "COLUMNS_NOT_GRANTED" || detailsJSON(t, details["missing_columns"]) != `{"policy_kb_chunks":["text_sha256"]}` {
			t.Fatalf("details %v", details)
		}
		filtered := requireToolDetails(t, search("task-kb-refusal-identity-only", "refusal-columns-filtered",
			map[string]any{"filters": []map[string]any{{"attribute": "job_role", "op": "=", "value": "staff"}}}), apierr.CodePolicyDenied)
		if detailsJSON(t, filtered["missing_columns"]) != `{"policy_kb_chunks":["job_role","text_sha256"]}` {
			t.Fatalf("filtered details %v", filtered)
		}
	})

	t.Run("has has no plan rendering", func(t *testing.T) {
		details := requireToolDetails(t, search(fullTask, "refusal-has",
			map[string]any{"filters": []map[string]any{{"attribute": "labels", "op": "has", "value": "urgent"}}}), apierr.CodeRetrievalFilterUnsupported)
		if details["reason"] != "OPERATOR_NOT_EXECUTABLE" || details["attribute"] != "labels" || details["operator"] != "has" {
			t.Fatalf("details %v", details)
		}
	})

	t.Run("fetch keeps its returned-chunk check first", func(t *testing.T) {
		_, err := callGatewayTool(harness.service, harness.alice, "fetch", map[string]any{"task_id": "task-kb-refusal-identity-only",
			"request_id": "refusal-fetch", "knowledge_product": "policy_kb",
			"chunks": []map[string]any{{"doc_id": "D-1", "version": 1, "chunk_index": 0}}})
		// This harness activates no chunks publication, so the returned-chunk
		// check refuses closed before any column check could run.
		requireToolCode(t, err, apierr.CodeConflict)
	})

	t.Run("aggregate columns not granted", func(t *testing.T) {
		kbRefusalTask(t, harness, "task-kb-refusal-values", []string{"policy_kb_values"},
			map[string][]string{"policy_kb_values": {"doc_id", "version", "field", "value_type", "kb_department", "classification"}})
		aggregate := func(requestID, function string) error {
			_, err := callGatewayTool(harness.service, harness.alice, "aggregate", map[string]any{"task_id": "task-kb-refusal-values",
				"request_id": requestID, "knowledge_product": "policy_kb", "field": "amount", "aggregate": function})
			return err
		}
		sum := requireToolDetails(t, aggregate("refusal-aggregate-sum", "sum"), apierr.CodePolicyDenied)
		if sum["reason"] != "COLUMNS_NOT_GRANTED" || detailsJSON(t, sum["missing_columns"]) != `{"policy_kb_values":["value_numeric"]}` {
			t.Fatalf("sum details %v", sum)
		}
		count := requireToolDetails(t, aggregate("refusal-aggregate-count", "count"), apierr.CodePolicyDenied)
		if detailsJSON(t, count["missing_columns"]) != `{"policy_kb_values":["canonical_value"]}` {
			t.Fatalf("count details %v", count)
		}
		_, err := callGatewayTool(harness.service, harness.alice, "aggregate", map[string]any{"task_id": "task-kb-refusal-identity-only",
			"request_id": "refusal-aggregate-relation", "knowledge_product": "policy_kb", "field": "amount", "aggregate": "count"})
		if details := requireToolDetails(t, err, apierr.CodePolicyDenied); details["reason"] != "RELATIONS_NOT_GRANTED" {
			t.Fatalf("values relation details %v", details)
		}
	})

	for _, taskID := range []string{fullTask, "task-kb-refusal-identity-only", "task-kb-refusal-values"} {
		budget := mustCallGatewayTool(t, harness.service, harness.alice, "get_budget", map[string]any{"task_id": taskID})
		if used, _ := budget["used"].(map[string]any); used != nil {
			if queries, _ := used["queries"].(float64); queries != 0 {
				t.Fatalf("a refusal consumed query budget on %s: %#v", taskID, budget)
			}
		}
	}
}

func TestListedSearchToolBoundsKByTheCatalog(t *testing.T) {
	harness, _ := kbSearchHarness(t, exposure.ProfileV5)
	var listed map[string]any
	for _, tool := range harness.service.ListTools(harness.alice) {
		if tool.Name == "search" {
			listed = tool.InputSchema
		}
	}
	encoded := detailsJSON(t, listed)
	if !strings.Contains(encoded, `"k":{"maximum":20,"minimum":1,"type":"integer"}`) {
		t.Fatalf("listed search schema does not bound k by the Catalog's k_max: %s", encoded)
	}
	if strings.Contains(encoded, `"has"`) {
		t.Fatalf("listed search schema still advertises has: %s", encoded)
	}
	// The package-level tool is shared; the bound must be a copy.
	if shared := detailsJSON(t, searchTool.InputSchema); strings.Contains(shared, `"maximum":20`) {
		t.Fatalf("bounding the listed tool mutated the shared schema: %s", shared)
	}
	if !reflect.DeepEqual(retrievalToolsFor(nil), retrievalTools) {
		t.Fatal("without a Catalog the retrieval tools are not the shared ones")
	}
}
