package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

// smokeUsability checks, on the deployed gateway, what the 2026-10-20 data
// consumer feedback found missing (KB1-LOOP-137): the knowledge product on
// the discovery surface with its task requirements, request-time readiness,
// the task-flow hints and the voiding of a pending task, the precise refusals
// for a relation name and for missing columns, and LIKE in query_sql. Every
// check names the evidence it read; refusals cost nothing, and the one query
// is LIMIT 3 so a small budget profile is not drawn down. The deficient task
// is scoped to classifications; likePattern must match a title in scope.
func smokeUsability(ctx context.Context, summary map[string]any, provisioner *smokeProvisioner, alice *kbformal.MCPClient,
	product catalog.KnowledgeProduct, fullTask string, classifications []string, likePattern string) error {
	chunks, documents := product.Products.Chunks, product.Products.Documents
	wantColumns := fmt.Sprintf(`{%q:["chunk_index","doc_id","text_sha256","version"],%q:["doc_id","version"]}`, chunks, documents)
	listed, err := alice.Call(ctx, "list_data_products", map[string]any{})
	if err != nil {
		return fmt.Errorf("list_data_products: %w", err)
	}
	page, err := knowledgePage(listed, product.Name)
	if err != nil {
		return err
	}
	if got := jsonOf(dig(page, "task_requirements", "search", "columns")); got != wantColumns {
		return fmt.Errorf("published search columns %s, want %s", got, wantColumns)
	}
	if err := checkPublishedEnums(page, product); err != nil {
		return err
	}
	described, err := alice.Call(ctx, "describe_data_product", map[string]any{"name": product.Name})
	if err != nil || described["kind"] != "knowledge_product" {
		return fmt.Errorf("describe_data_product(%s): %v %v", product.Name, described["kind"], err)
	}
	capabilities, err := alice.Call(ctx, "get_sql_capabilities", map[string]any{})
	if err != nil || !strings.Contains(jsonOf(capabilities["where_predicates"]), `"predicate":"LIKE"`) {
		return fmt.Errorf("get_sql_capabilities lists no LIKE predicate: %v", err)
	}

	// The grant alice was approved on 2026-10-20 before her first search
	// worked: both relations, identity columns only.
	deficient := kbformal.TaskRequest{Objective: "KB1-LOOP-137 smoke: identity columns only",
		Products: []string{chunks, documents},
		Columns:  map[string][]string{chunks: {"doc_id", "version", "chunk_index"}, documents: {"doc_id", "version"}},
		Scope:    map[string]any{"kb_department": []string{"finance"}, "classification": classifications}}
	pending, err := alice.Call(ctx, "request_data_task", map[string]any{"objective": deficient.Objective,
		"data_products": deficient.Products, "columns": deficient.Columns, "scopes": deficient.Scope})
	if err != nil {
		return fmt.Errorf("request a deficient task: %w", err)
	}
	readiness, _ := pending["knowledge_readiness"].([]any)
	if len(readiness) != 1 || dig(readiness[0], "search", "ready") != false ||
		jsonOf(dig(readiness[0], "search", "missing_columns")) != fmt.Sprintf(`{%q:["text_sha256"]}`, chunks) {
		return fmt.Errorf("request-time readiness %s does not foretell the missing text_sha256", jsonOf(pending["knowledge_readiness"]))
	}
	pendingID, _ := pending["task_id"].(string)
	status, err := alice.Call(ctx, "get_task_status", map[string]any{"task_id": pendingID})
	if agentSubmitted := pending["state"] == "AWAITING_APPROVAL"; agentSubmitted != provisioner.agentSubmits {
		return fmt.Errorf("pending task created %v, but the profile Catalog's agent_submits is %v", pending["state"], provisioner.agentSubmits)
	} else if agentSubmitted {
		// Under agent_submits (KB1-LOOP-149) the agent submitted the task at
		// creation: request and status both name an approval step, never a
		// submission.
		if err != nil || !approvalNextAction(pending["next_action"]) || !approvalNextAction(status["next_action"]) {
			return fmt.Errorf("agent-submitted task flow: request %v, status %v, %v", pending["next_action"], status["next_action"], err)
		}
		summary["usability_pending_agent_submitted"] = true
	} else if err != nil || pending["next_action"] != "human_submit_in_oa" || status["next_action"] != "human_submit_in_oa" {
		return fmt.Errorf("pending task flow: request %v, status %v, %v", pending["next_action"], status["next_action"], err)
	}
	summary["usability_pending_oa_url_rebuilt"] = status["oa_url"] != nil
	revoked, err := alice.Call(ctx, "revoke_task", map[string]any{"task_id": pendingID, "reason": "KB1-LOOP-137 smoke: voiding a pending task"})
	if err != nil || revoked["terminal_reason"] != "revoked" {
		return fmt.Errorf("revoke a pending task: %v %v", revoked["terminal_reason"], err)
	}
	if status, err = alice.Call(ctx, "get_task_status", map[string]any{"task_id": pendingID}); err != nil || status["next_action"] != "none_terminal" {
		return fmt.Errorf("revoked task flow: %v %v", status["next_action"], err)
	}

	deficientTask, _, err := provisioner.Provision(ctx, deficient)
	if err != nil {
		return fmt.Errorf("provision the deficient task: %w", err)
	}
	search := map[string]any{"task_id": deficientTask, "request_id": "smoke-usability-columns",
		"knowledge_product": product.Name, "query": map[string]any{"text": "财务部 差旅 报销 标准"}, "k": 5}
	_, err = alice.Call(ctx, "search", search)
	var refusal *kbformal.ToolError
	if !errors.As(err, &refusal) || refusal.Code != "POLICY_DENIED" || refusal.Reason != "COLUMNS_NOT_GRANTED" ||
		jsonOf(refusal.Details["missing_columns"]) != fmt.Sprintf(`{%q:["text_sha256"]}`, chunks) {
		return fmt.Errorf("search on identity columns: want COLUMNS_NOT_GRANTED naming text_sha256, got %v %v", err, detailsOf(refusal))
	}
	search["task_id"], search["request_id"], search["knowledge_product"] = fullTask, "smoke-usability-relation-name", chunks
	_, err = alice.Call(ctx, "search", search)
	if !errors.As(err, &refusal) || refusal.Code != "INVALID_REQUEST" || refusal.Reason != "UNKNOWN_KNOWLEDGE_PRODUCT" ||
		refusal.Details["suggested_knowledge_product"] != product.Name {
		return fmt.Errorf("search naming the chunks relation: want INVALID_REQUEST suggesting %s, got %v %v", product.Name, err, detailsOf(refusal))
	}

	// likePattern carries no quote (checked at flag parsing), so it is a
	// literal as is.
	like := fmt.Sprintf("SELECT doc_id, title FROM %s WHERE title LIKE '%s' ORDER BY doc_id LIMIT 3", documents, likePattern)
	queried, err := alice.Call(ctx, "query_sql", map[string]any{"task_id": fullTask, "request_id": "smoke-usability-like", "sql": like})
	if err != nil {
		return fmt.Errorf("query_sql with LIKE: %w", err)
	}
	if queried["status"] != "COMPLETED" || fmt.Sprint(queried["row_count"]) == "0" {
		return fmt.Errorf("query_sql with LIKE returned status %v, %v rows", queried["status"], queried["row_count"])
	}
	summary["usability_like_rows"] = queried["row_count"]
	summary["usability_refusals"] = []string{"COLUMNS_NOT_GRANTED", "UNKNOWN_KNOWLEDGE_PRODUCT"}
	summary["usability_published_search_columns"] = wantColumns
	return nil
}

// approvalNextAction reports whether a task flow's next_action is one of the
// AWAITING_APPROVAL branch's (internal/gateway/task_flow.go): an approver's
// decision, or a person's own confirmation.
func approvalNextAction(action any) bool {
	return action == "await_approver_decision" || action == "human_confirm_in_oa"
}

// knowledgePage finds one knowledge product in list_data_products.
func knowledgePage(listed map[string]any, name string) (map[string]any, error) {
	pages, _ := listed["knowledge_products"].([]any)
	for _, raw := range pages {
		if page, _ := raw.(map[string]any); page != nil && page["name"] == name {
			return page, nil
		}
	}
	return nil, fmt.Errorf("list_data_products publishes no knowledge product %s", name)
}

// checkPublishedEnums requires every enum attribute the Catalog declares with
// its own values to be published with exactly those values.
func checkPublishedEnums(page map[string]any, product catalog.KnowledgeProduct) error {
	published := map[string]string{}
	attributes, _ := page["attributes"].([]any)
	for _, raw := range attributes {
		if attribute, _ := raw.(map[string]any); attribute != nil {
			published[fmt.Sprint(attribute["name"])] = jsonOf(attribute["allowed_values"])
		}
	}
	for _, attribute := range product.Attributes {
		if attribute.Type != "enum" || len(attribute.AllowedValues) == 0 {
			continue
		}
		if want := jsonOf(attribute.AllowedValues); published[attribute.Name] != want {
			return fmt.Errorf("attribute %s published values %s, Catalog declares %s", attribute.Name, published[attribute.Name], want)
		}
	}
	return nil
}

func dig(value any, path ...string) any {
	for _, key := range path {
		object, _ := value.(map[string]any)
		if object == nil {
			return nil
		}
		value = object[key]
	}
	return value
}

func jsonOf(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func detailsOf(refusal *kbformal.ToolError) string {
	if refusal == nil {
		return ""
	}
	return jsonOf(refusal.Details)
}
