package gateway

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbdownload"
)

// The document download task (docs/kb-design.md §12): the request path puts
// the approved list into the manifest under the reserved key, and search,
// download and queries are refused across task kinds before anything runs.

func TestRequestDownloadTaskCarriesTheApprovedList(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: kbCatalogRewrite(t),
		principalAttributes: kbSubjectSnapshot(t, "finance", []string{"public", "internal"}, "")})
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_download_task", map[string]any{
		"objective": "下载两份合同", "knowledge_product": "policy_kb", "include_values": true,
		"documents": []map[string]any{{"doc_id": "D-2", "version": 1}, {"doc_id": "D-1", "version": 1}},
	})
	if result["task_kind"] != "download" || !reflect.DeepEqual(result["documents"], []string{"D-1@1", "D-2@1"}) {
		t.Fatalf("result %v", result)
	}
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA drafts = %d, want 1", len(harness.approval.requests))
	}
	manifest := harness.approval.requests[0].Manifest
	if !reflect.DeepEqual(manifest.Products, []string{"policy_kb_chunks", "policy_kb_documents", "policy_kb_values"}) {
		t.Fatalf("products %v", manifest.Products)
	}
	if !reflect.DeepEqual(manifest.ApprovedColumns["policy_kb_documents"], []string{"chunk_count", "classification", "doc_id", "kb_department", "title", "version"}) {
		t.Fatalf("document columns %v", manifest.ApprovedColumns["policy_kb_documents"])
	}
	scope, _ := json.Marshal(manifest.MandatoryScope)
	var decoded map[string]any
	_ = json.Unmarshal(scope, &decoded)
	// The default scope is the subject's eligibility envelope.
	want := map[string]any{kbdownload.ScopeKey: []any{"D-1@1", "D-2@1"}, "kb_department": []any{"finance"},
		"classification": []any{"internal", "public"}}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("mandatory scope %v, want %v", decoded, want)
	}

	for name, request := range map[string]map[string]any{
		"no documents": {"objective": "x", "knowledge_product": "policy_kb", "documents": []map[string]any{}},
		"duplicate": {"objective": "x", "knowledge_product": "policy_kb",
			"documents": []map[string]any{{"doc_id": "D-1", "version": 1}, {"doc_id": "D-1", "version": 1}}},
		"unknown product": {"objective": "x", "knowledge_product": "nope", "documents": []map[string]any{{"doc_id": "D-1", "version": 1}}},
	} {
		_, err := callGatewayTool(harness.service, harness.alice, "request_download_task", request)
		requireToolCode(t, err, apierr.CodeInvalidRequest)
		if len(harness.approval.requests) != 1 {
			t.Fatalf("%s produced an OA draft", name)
		}
	}
	// The original-file right is the files relation, and it brings the values
	// relation with it.
	result = mustCallGatewayTool(t, harness.service, harness.alice, "request_download_task", map[string]any{
		"objective": "下载原文件", "knowledge_product": "policy_kb", "original_file": true,
		"documents": []map[string]any{{"doc_id": "D-1", "version": 1}},
	})
	if result["original_file"] != true || len(harness.approval.requests) != 2 {
		t.Fatalf("original-file request %v, drafts %d", result, len(harness.approval.requests))
	}
	manifest = harness.approval.requests[1].Manifest
	if !reflect.DeepEqual(manifest.Products, []string{"policy_kb_chunks", "policy_kb_documents", "policy_kb_files", "policy_kb_values"}) ||
		!reflect.DeepEqual(manifest.ApprovedColumns["policy_kb_files"], []string{"byte_size", "classification", "doc_id", "file_sha256", "kb_department", "media_type", "version"}) {
		t.Fatalf("original-file manifest products %v files columns %v", manifest.Products, manifest.ApprovedColumns["policy_kb_files"])
	}
	// Explicit scopes pass the same eligibility gate as any request.
	_, err := callGatewayTool(harness.service, harness.alice, "request_download_task", map[string]any{
		"objective": "x", "knowledge_product": "policy_kb", "documents": []map[string]any{{"doc_id": "D-1", "version": 1}},
		"scopes": map[string]any{"kb_department": []any{"hr"}, "classification": []any{"internal"}},
	})
	requireToolCode(t, err, apierr.CodeNotEligible)
	// The reserved key cannot be smuggled through an ordinary request.
	request := kbTaskRequest("finance", []string{"internal"})
	request["scopes"].(map[string]any)[kbdownload.ScopeKey] = []any{"D-1@1"}
	_, err = callGatewayTool(harness.service, harness.alice, "request_data_task", request)
	requireToolCode(t, err, apierr.CodeInvalidRequest)
	if len(harness.approval.requests) != 2 {
		t.Fatalf("refused requests produced OA drafts: %d", len(harness.approval.requests))
	}
}

func TestDownloadAndSearchTaskKindsAreSeparate(t *testing.T) {
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: kbCatalogRewrite(t)})
	columns := map[string][]string{
		"policy_kb_chunks":    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification"},
		"policy_kb_documents": {"doc_id", "version", "title", "chunk_count", "kb_department", "classification"},
	}
	limits := control.ExposureLimits{ReleaseFacts: 1000, InfluenceFacts: 10000, OutcomeFacts: 100}
	products := []string{"policy_kb_chunks", "policy_kb_documents"}
	harness.createTaskWithGrantScopeAndExposureProfile(t, "task-download", nil, limits, exposure.ProfileV5, products, columns,
		domain.SensitivityHigh, map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal"},
			kbdownload.ScopeKey: []any{"D-1@1"}})
	harness.createTaskWithGrantScopeAndExposureProfile(t, "task-search", nil, limits, exposure.ProfileV5, products, columns,
		domain.SensitivityHigh, map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal"}})

	_, err := callGatewayTool(harness.service, harness.alice, "search", map[string]any{"task_id": "task-download", "request_id": "s-1",
		"knowledge_product": "policy_kb", "query": map[string]any{"keywords": []string{"合同"}}, "k": 3})
	requireToolCode(t, err, apierr.CodeTaskKindMismatch)
	_, err = callGatewayTool(harness.service, harness.alice, "fetch", map[string]any{"task_id": "task-download", "request_id": "f-1",
		"knowledge_product": "policy_kb", "chunks": []map[string]any{{"doc_id": "D-1", "version": 1, "chunk_index": 0}}})
	requireToolCode(t, err, apierr.CodeTaskKindMismatch)
	_, err = callGatewayTool(harness.service, harness.alice, "aggregate", map[string]any{"task_id": "task-download", "request_id": "a-1",
		"knowledge_product": "policy_kb", "field": "amount", "aggregate": "count"})
	requireToolCode(t, err, apierr.CodeTaskKindMismatch)
	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{"task_id": "task-download", "request_id": "q-1",
		"sql": "SELECT doc_id FROM policy_kb_documents"})
	requireToolCode(t, err, apierr.CodeTaskKindMismatch)
	_, err = callGatewayTool(harness.service, harness.alice, "download_document", map[string]any{"task_id": "task-download", "request_id": "d-1",
		"knowledge_product": "policy_kb", "doc_id": "D-2", "version": 1})
	requireToolCode(t, err, apierr.CodeDownloadNotApproved)
	_, err = callGatewayTool(harness.service, harness.alice, "download_document", map[string]any{"task_id": "task-search", "request_id": "d-2",
		"knowledge_product": "policy_kb", "doc_id": "D-1", "version": 1})
	requireToolCode(t, err, apierr.CodeTaskKindMismatch)
	// Nothing was reserved or settled by any refusal.
	for _, task := range []string{"task-download", "task-search"} {
		queries, err := harness.store.ListQueries(context.Background(), task, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(queries) != 0 {
			t.Fatalf("%s has %d query records after refusals", task, len(queries))
		}
	}
}

func TestDocumentDownloadSignatureIsDomainSeparated(t *testing.T) {
	service := &Service{deliverySigningKey: []byte("0123456789abcdef0123456789abcdef")}
	ref := kbdownload.Ref{DocID: "D-1", Version: 1}
	assembled := kbdownload.FormatAssembled
	base := service.documentDownloadSignature("task", "req", "policy_kb", ref, assembled, "aa", 100)
	for name, other := range map[string]string{
		"task":    service.documentDownloadSignature("task2", "req", "policy_kb", ref, assembled, "aa", 100),
		"request": service.documentDownloadSignature("task", "req2", "policy_kb", ref, assembled, "aa", 100),
		"product": service.documentDownloadSignature("task", "req", "other", ref, assembled, "aa", 100),
		"doc":     service.documentDownloadSignature("task", "req", "policy_kb", kbdownload.Ref{DocID: "D-1", Version: 2}, assembled, "aa", 100),
		"format":  service.documentDownloadSignature("task", "req", "policy_kb", ref, kbdownload.FormatOriginal, "aa", 100),
		"digest":  service.documentDownloadSignature("task", "req", "policy_kb", ref, assembled, "ab", 100),
		"expires": service.documentDownloadSignature("task", "req", "policy_kb", ref, assembled, "aa", 101),
		"result":  service.deliverySignature("task", "req", 100),
	} {
		if other == base {
			t.Fatalf("signature does not bind %s", name)
		}
	}
}
