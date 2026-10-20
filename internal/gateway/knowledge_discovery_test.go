package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// asJSON round-trips a discovery value through its wire encoding.
func asJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func attributeNamed(t *testing.T, public map[string]any, name string) map[string]any {
	t.Helper()
	for _, raw := range public["attributes"].([]any) {
		if attribute := raw.(map[string]any); attribute["name"] == name {
			return attribute
		}
	}
	t.Fatalf("attribute %s not published", name)
	return nil
}

func TestKnowledgeProductDiscoveryReadsTheEnforcingCode(t *testing.T) {
	service := kbPlanService(t)
	kp, found := service.catalog.LookupKnowledgeProduct("policy_kb")
	if !found {
		t.Fatal("fixture declares no policy_kb")
	}
	public := asJSON(t, service.publicKnowledgeProduct(kp))
	for _, private := range []string{"collection", "source_namespace", "eligibility", "embedding", "weights_sha256"} {
		if _, leaked := public[private]; leaked || strings.Contains(detailsJSON(t, public), `"`+private+`"`) {
			t.Fatalf("discovery publishes the private %q: %v", private, public)
		}
	}
	// search and fetch requirements are exactly the retrieval plan's columns.
	required, err := physicalquery.RetrievalRequiredColumns(kp)
	if err != nil {
		t.Fatal(err)
	}
	requirements := public["task_requirements"].(map[string]any)
	for _, tool := range []string{"search", "fetch"} {
		if got := detailsJSON(t, requirements[tool].(map[string]any)["columns"]); got != detailsJSON(t, required) {
			t.Fatalf("%s requirement columns %s, retrieval plan columns %s", tool, got, detailsJSON(t, required))
		}
	}
	if detailsJSON(t, required) != `{"policy_kb_chunks":["chunk_index","doc_id","text_sha256","version"],"policy_kb_documents":["doc_id","version"]}` {
		t.Fatalf("retrieval plan columns %v", required)
	}
	retrievalPage := public["retrieval"].(map[string]any)
	if retrievalPage["k_max"] != float64(20) || retrievalPage["keyword"] != false {
		t.Fatalf("retrieval page %v", retrievalPage)
	}
	jobRole := attributeNamed(t, public, "job_role")
	if detailsJSON(t, jobRole["allowed_values"]) != `["staff","manager","director","executive","hr"]` ||
		!reflect.DeepEqual(jobRole["search_operators"], []any{"<>", "=", "in"}) ||
		detailsJSON(t, jobRole["filter_columns"]) != `["policy_kb_chunks.job_role"]` {
		t.Fatalf("job_role %v", jobRole)
	}
	department := attributeNamed(t, public, "kb_department")
	if department["scope"] != "kb_department" || detailsJSON(t, department["allowed_values"]) != `["sales","engineering","finance","hr"]` {
		t.Fatalf("kb_department %v", department)
	}
	labels := attributeNamed(t, public, "labels")
	if detailsJSON(t, labels["search_operators"]) != `[]` || labels["search_note"] == nil || labels["filter_columns"] != nil {
		t.Fatalf("labels (text_set, has has no plan rendering) %v", labels)
	}
	// Aggregate requirements are aggregatePlan's own plans; the fixture admits
	// count and sum, and sum needs a numeric field.
	fields := map[string]map[string]any{}
	for _, raw := range public["extracted_fields"].([]any) {
		field := raw.(map[string]any)
		fields[field["field"].(string)] = field
	}
	if detailsJSON(t, fields["amount"]["aggregates"]) != `["count","sum"]` ||
		detailsJSON(t, fields["amount"]["columns"]) != `["canonical_value","field","value_numeric","value_type"]` {
		t.Fatalf("amount %v", fields["amount"])
	}
	if detailsJSON(t, fields["effective_on"]["aggregates"]) != `["count"]` {
		t.Fatalf("effective_on %v", fields["effective_on"])
	}
	grant := control.TaskGrant{ApprovedProducts: kp.Products.Relations(),
		MandatoryScope: json.RawMessage(`{"kb_department":["finance"],"classification":["internal"]}`)}
	for _, function := range []string{"count", "sum"} {
		request, err := retrieval.DecodeRequest(retrieval.ToolAggregate, []byte(`{"knowledge_product":"policy_kb","field":"amount","aggregate":"`+function+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		plan, err := service.aggregatePlan(grant, request)
		if err != nil {
			t.Fatal(err)
		}
		for _, column := range singleProductPlanColumns(plan)[plan.Product] {
			if !strings.Contains(detailsJSON(t, fields["amount"]["columns"]), `"`+column+`"`) {
				t.Fatalf("amount %s reads %s, which discovery does not list", function, column)
			}
		}
	}
}

func TestKnowledgeReadinessForetellsWhatATaskCanUse(t *testing.T) {
	service := kbPlanService(t)
	chunks, documents := "policy_kb_chunks", "policy_kb_documents"
	readiness := func(products []string, columns map[string][]string, profile string) map[string]any {
		entries := service.knowledgeReadiness(products, columns, profile, "requested")
		if len(entries) != 1 {
			t.Fatalf("readiness entries %d, want one for policy_kb", len(entries))
		}
		return asJSON(t, entries[0])
	}
	// The requests alice made on 2026-10-20 before her first search worked.
	identityOnly := readiness([]string{chunks, documents},
		map[string][]string{chunks: {"doc_id", "version", "chunk_index"}, documents: {"doc_id", "version"}}, exposure.ProfileV5)
	search := identityOnly["search"].(map[string]any)
	if search["ready"] != false || detailsJSON(t, search["missing_columns"]) != `{"policy_kb_chunks":["text_sha256"]}` {
		t.Fatalf("identity-only readiness %v", search)
	}
	chunksOnly := readiness([]string{chunks}, map[string][]string{chunks: {"doc_id", "version", "chunk_index"}}, exposure.ProfileV5)
	search = chunksOnly["search"].(map[string]any)
	if detailsJSON(t, search["missing_products"]) != `["policy_kb_documents"]` || detailsJSON(t, search["missing_columns"]) != `{"policy_kb_chunks":["text_sha256"]}` {
		t.Fatalf("chunks-only readiness %v", search)
	}
	sufficient := map[string][]string{chunks: {"doc_id", "version", "chunk_index", "text_sha256", "job_role"}, documents: {"doc_id", "version"}}
	ready := readiness([]string{chunks, documents}, sufficient, exposure.ProfileV5)
	search = ready["search"].(map[string]any)
	if search["ready"] != true || detailsJSON(t, search["filterable_attributes"]) != `["job_role"]` {
		t.Fatalf("sufficient readiness %v", search)
	}
	if aggregate := ready["aggregate"].(map[string]any)["amount"].(map[string]any); aggregate["ready"] != false ||
		detailsJSON(t, aggregate["missing_products"]) != `["policy_kb_values"]` {
		t.Fatalf("aggregate readiness without the values relation %v", aggregate)
	}
	wrongProfile := readiness([]string{chunks, documents}, sufficient, exposure.ProfileV4)
	if search = wrongProfile["search"].(map[string]any); search["ready"] != false || search["requires_exposure_profile"] != exposure.ProfileV5 {
		t.Fatalf("V4 readiness %v", search)
	}
	if entries := service.knowledgeReadiness([]string{"expense_summary"}, nil, exposure.ProfileV5, "requested"); len(entries) != 0 {
		t.Fatalf("a request touching no knowledge product got readiness %v", entries)
	}
}

func TestKnowledgeProductsAreDiscoverableAndReadinessIsReturned(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: kbCatalogRewrite(t),
		principalAttributes: kbSubjectSnapshot(t, "finance", []string{"public", "internal"}, "")})
	listed := mustCallGatewayTool(t, harness.service, harness.alice, "list_data_products", map[string]any{})
	if knowledge, _ := listed["knowledge_products"].([]map[string]any); len(knowledge) != 1 || knowledge[0]["name"] != "policy_kb" {
		t.Fatalf("list_data_products knowledge products %v", listed["knowledge_products"])
	}
	pointed := false
	for _, product := range listed["products"].([]map[string]any) {
		if product["name"] == "policy_kb_chunks" {
			pointed = reflect.DeepEqual(product["knowledge_product"], map[string]any{"name": "policy_kb", "role": "chunks"})
		}
	}
	if !pointed {
		t.Fatal("policy_kb_chunks does not point back to its knowledge product")
	}
	described := mustCallGatewayTool(t, harness.service, harness.alice, "describe_data_product", map[string]any{"name": "policy_kb"})
	if described["kind"] != "knowledge_product" || described["task_requirements"] == nil {
		t.Fatalf("describe_data_product(policy_kb) %v", described)
	}

	request := kbTaskRequest("finance", []string{"internal"})
	request["columns"] = map[string][]string{"policy_kb_chunks": {"doc_id", "version", "chunk_index"}, "policy_kb_documents": {"doc_id", "version"}}
	created := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", request)
	entries, _ := created["knowledge_readiness"].([]map[string]any)
	if len(entries) != 1 {
		t.Fatalf("request_data_task readiness %v", created["knowledge_readiness"])
	}
	search := asJSON(t, entries[0])["search"].(map[string]any)
	if search["ready"] != false || detailsJSON(t, search["missing_columns"]) != `{"policy_kb_chunks":["text_sha256"]}` {
		t.Fatalf("request-time readiness %v", search)
	}

	harness.createTaskWithGrantScopeAndExposureProfile(t, "task-kb-readiness", nil,
		control.ExposureLimits{ReleaseFacts: 400, InfluenceFacts: 2000, OutcomeFacts: 64}, exposure.ProfileV5,
		[]string{"policy_kb_chunks", "policy_kb_documents"},
		map[string][]string{"policy_kb_chunks": {"doc_id", "version", "chunk_index", "text_sha256"}, "policy_kb_documents": {"doc_id", "version"}},
		domain.SensitivityHigh, map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal"}})
	context := mustCallGatewayTool(t, harness.service, harness.alice, "get_task_context", map[string]any{"task_id": "task-kb-readiness"})
	approved, _ := context["knowledge_readiness"].([]map[string]any)
	if len(approved) != 1 || approved[0]["basis"] != "approved" || asJSON(t, approved[0])["search"].(map[string]any)["ready"] != true {
		t.Fatalf("get_task_context readiness %v", context["knowledge_readiness"])
	}
}
