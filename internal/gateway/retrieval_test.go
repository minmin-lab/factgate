package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

// kbCatalogRewrite merges the knowledge-product fragment used by the catalog
// and kbpublish tests into the demo Catalog text.
func kbCatalogRewrite(t *testing.T) func(string) string {
	t.Helper()
	fragment, err := os.ReadFile(filepath.Join("..", "catalog", "testdata", "kb-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	segmentation, _ := catalog.CanonicalDeclarationDigest(catalog.KBSegmentation{Function: catalog.KBSegmentationSectionWindowV1, WindowChars: 800})
	embedding, _ := catalog.CanonicalDeclarationDigest(catalog.KBEmbedding{Model: catalog.KBEmbeddingHashProjectionV1, Dimension: 256})
	rules, _ := catalog.CanonicalDeclarationDigest([]catalog.KBExtractionRule{
		{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
		{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
	})
	text := strings.NewReplacer("{{SEGMENTATION}}", segmentation, "{{EMBEDDING}}", embedding, "{{RULES}}", rules).Replace(string(fragment))
	return func(base string) string {
		merged, err := catalog.MergeYAMLFragment([]byte(base), []byte(text))
		if err != nil {
			t.Fatalf("merge knowledge fragment: %v", err)
		}
		return string(merged)
	}
}

func kbSearchHarness(t *testing.T, profile string) (*gatewayHarness, string) {
	t.Helper()
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: kbCatalogRewrite(t)})
	taskID := "task-kb-search-" + strings.ReplaceAll(profile, "-", "")
	harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
		control.ExposureLimits{ReleaseFacts: 400, InfluenceFacts: 2000, OutcomeFacts: 64}, profile,
		[]string{"policy_kb_chunks", "policy_kb_documents"},
		map[string][]string{
			"policy_kb_chunks":    {"doc_id", "version", "chunk_index", "text", "kb_department", "classification", "job_role", "valid_until"},
			"policy_kb_documents": {"doc_id", "version", "title", "kb_department", "classification"},
		}, domain.SensitivityHigh,
		map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal"}})
	return harness, taskID
}

func kbVector(dimension int) []float64 {
	vector := make([]float64, dimension)
	for i := range vector {
		vector[i] = float64(i%7) * 0.01
	}
	return vector
}

func TestSearchToolIsListedAndRejectsClosedForm(t *testing.T) {
	harness, taskID := kbSearchHarness(t, exposure.ProfileV5)
	listed := map[string]bool{}
	for _, tool := range harness.service.ListTools(harness.alice) {
		listed[tool.Name] = true
	}
	if !listed["search"] || !listed["fetch"] {
		t.Fatalf("search and fetch are not both listed to query principals: %v", listed)
	}
	base := func(overrides map[string]any) map[string]any {
		args := map[string]any{"task_id": taskID, "request_id": "kb-search-1", "knowledge_product": "policy_kb",
			"query": map[string]any{"vector": kbVector(256)}, "k": 5}
		for key, value := range overrides {
			args[key] = value
		}
		return args
	}
	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"unknown knowledge product", base(map[string]any{"knowledge_product": "nope"}), apierr.CodePolicyDenied},
		{"unknown attribute", base(map[string]any{"filters": []map[string]any{{"attribute": "colour", "op": "=", "value": "red"}}}), apierr.CodeRetrievalFilterUnsupported},
		{"unsupported operator on enum", base(map[string]any{"filters": []map[string]any{{"attribute": "job_role", "op": ">", "value": "staff"}}}), apierr.CodeRetrievalFilterUnsupported},
		{"k above k_max", base(map[string]any{"k": 21}), apierr.CodeRetrievalKExceeded},
		{"scope not intersected", base(map[string]any{"filters": []map[string]any{{"attribute": "kb_department", "op": "=", "value": "hr"}}}), apierr.CodeRetrievalScopeExceeded},
		{"text query without embedder", base(map[string]any{"query": map[string]any{"text": "差旅报销政策"}}), apierr.CodeRetrievalQueryUnsupported},
		{"vector of the wrong dimension", base(map[string]any{"query": map[string]any{"vector": kbVector(3)}}), apierr.CodeRetrievalQueryUnsupported},
		{"unknown request field", base(map[string]any{"rerank": true}), apierr.CodeRetrievalQueryUnsupported},
	}
	for index, tc := range cases {
		tc.args["request_id"] = "kb-search-" + strings.ReplaceAll(tc.name, " ", "-")
		_, err := callGatewayTool(harness.service, harness.alice, "search", tc.args)
		if err == nil {
			t.Fatalf("case %d %q: search accepted", index, tc.name)
		}
		t.Logf("case %d %q: %v", index, tc.name, err)
		requireToolCode(t, err, tc.code)
	}
}

func TestSearchRequiresV5ProfileAndSnapshotEvidence(t *testing.T) {
	harness, taskID := kbSearchHarness(t, exposure.ProfileV4)
	args := map[string]any{"task_id": taskID, "request_id": "kb-search-v4", "knowledge_product": "policy_kb",
		"query": map[string]any{"vector": kbVector(256)}, "k": 5}
	_, err := callGatewayTool(harness.service, harness.alice, "search", args)
	requireToolCode(t, err, apierr.CodeExposureEvidenceRequired)

	harness5, task5 := kbSearchHarness(t, exposure.ProfileV5)
	args["task_id"], args["request_id"] = task5, "kb-search-v5"
	// This harness publishes no knowledge snapshot, so a well-formed search
	// must stop at the snapshot evidence, closed, before any reservation.
	_, err = callGatewayTool(harness5.service, harness5.alice, "search", args)
	requireToolCode(t, err, apierr.CodeConflict)
	budget := mustCallGatewayTool(t, harness5.service, harness5.alice, "get_budget", map[string]any{"task_id": task5})
	if used, _ := budget["used"].(map[string]any); used != nil {
		if queries, _ := used["queries"].(float64); queries != 0 {
			t.Fatalf("a refused search consumed query budget: %#v", budget)
		}
	}
}

func TestMandatoryScopeValuesAndRetrievalBody(t *testing.T) {
	values, err := mandatoryScopeValues(control.TaskGrant{MandatoryScope: json.RawMessage(`{"kb_department":["finance","hr"],"classification":"internal","expense_date":{"from":"2026-01-01","to":"2026-02-01"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(values["kb_department"]) != 2 || values["classification"][0] != "internal" || len(values["expense_date"]) != 0 {
		t.Fatalf("scope values %#v", values)
	}
	if _, err := mandatoryScopeValues(control.TaskGrant{MandatoryScope: json.RawMessage(`{"kb_department":[1]}`)}); err == nil {
		t.Fatal("non-string scope value accepted")
	}
	left, err := retrievalBody(json.RawMessage(`{"task_id":"t","request_id":"r","k":5,"knowledge_product":"policy_kb","query":{"text":"a"}}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := retrievalBody(json.RawMessage(`{"query":{"text":"a"},"knowledge_product":"policy_kb","request_id":"other","k":5,"task_id":"t"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatalf("request bodies differ by key order:\n%s\n%s", left, right)
	}
	if strings.Contains(string(left), "task_id") || strings.Contains(string(left), "request_id") {
		t.Fatalf("envelope keys leaked into the retrieval body: %s", left)
	}
}
