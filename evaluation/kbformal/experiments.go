package kbformal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// Deployment is everything an experiment needs from one fresh deployment,
// reached only through its public interfaces: the gateway's MCP endpoint as
// Alice and as the file principals, the OA forms, the published receipt
// keyring, the business database's read-only role (for the direct arms), and
// the deployment's embedding service.
type Deployment struct {
	Index       int
	Catalog     *catalog.Catalog
	Product     catalog.KnowledgeProduct
	Alice       *MCPClient
	Provisioner *Provisioner
	Principals  map[string]*MCPClient
	Verifier    ReceiptVerifier
	Business    *pgx.Conn
	Timeout     time.Duration
}

// Retrieval is one verified search/fetch/aggregate call: its response, the
// verified receipt, and the delivered rows of the canonical result. CallMS is
// the client time until the tool response (which carries the rows inline)
// arrived; DeliveredMS adds receipt verification, the artifact delivery and
// the row checks.
type Retrieval struct {
	Response    map[string]any
	Rows        [][]any
	Charge      Charge
	CallMS      float64
	DeliveredMS float64
}

// Charge is the exposure part of a response.
type Charge struct {
	ActualRelease, ActualInfluence, ActualOutcome    int64
	ChargedRelease, ChargedInfluence, ChargedOutcome int64
}

func number(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case float64:
		return int64(typed)
	case int64:
		return typed
	}
	return 0
}

func chargeOf(response map[string]any) Charge {
	exposure, _ := response["exposure"].(map[string]any)
	return Charge{ActualRelease: number(exposure["actual_release_facts"]), ActualInfluence: number(exposure["actual_influence_facts"]),
		ActualOutcome: number(exposure["actual_outcome_facts"]), ChargedRelease: number(exposure["charged_release_facts"]),
		ChargedInfluence: number(exposure["charged_influence_facts"]), ChargedOutcome: number(exposure["charged_outcome_facts"])}
}

// Retrieve calls a retrieval tool as client and returns the verified result:
// the receipt must verify against the keyring and name the profile Catalog,
// and the rows are those of the delivered canonical artifact.
func (d *Deployment) Retrieve(ctx context.Context, client *MCPClient, tool string, arguments map[string]any) (Retrieval, error) {
	started := time.Now()
	response, err := client.Call(ctx, tool, arguments)
	callMS := elapsedMS(started)
	if err != nil {
		return Retrieval{}, err
	}
	receipt, err := VerifyReceipt(d.Verifier, response["receipt"])
	if err != nil {
		return Retrieval{}, fmt.Errorf("%s receipt: %w", tool, err)
	}
	if receipt.CatalogDigest != d.Catalog.SHA256 {
		return Retrieval{}, fmt.Errorf("%s receipt names catalog %s, not the profile's %s", tool, receipt.CatalogDigest, d.Catalog.SHA256)
	}
	rows, err := DeliverRows(ctx, client, receipt, d.Timeout)
	if err != nil {
		return Retrieval{}, fmt.Errorf("%s delivery: %w", tool, err)
	}
	if tool == "search" || tool == "fetch" {
		for index, row := range rows {
			if err := checkChunkRow(row); err != nil {
				return Retrieval{}, fmt.Errorf("%s row %d: %w", tool, index, err)
			}
		}
	}
	return Retrieval{Response: response, Rows: rows, Charge: chargeOf(response), CallMS: callMS, DeliveredMS: elapsedMS(started)}, nil
}

// checkChunkRow requires the retrieval shape (doc_id, version, chunk_index,
// text_sha256, text) and that the delivered text hashes to the released
// digest (正式轮规程 §5).
func checkChunkRow(row []any) error {
	if len(row) != 5 {
		return fmt.Errorf("%d columns, want doc_id, version, chunk_index, text_sha256, text", len(row))
	}
	text, ok := row[4].(string)
	if !ok {
		return errors.New("delivered text is not a string")
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != fmt.Sprint(row[3]) {
		return errors.New("delivered text does not hash to the released text_sha256")
	}
	return nil
}

// ---- E-KB5: document-scoped versus collection-scoped values ----

// EKB5Observation matches the pilot report's observation entries.
type EKB5Observation struct {
	Publication      string  `json:"publication"`
	Deployment       int     `json:"deployment"`
	RelationUsed     string  `json:"relation_used"`
	Answer           string  `json:"answer"`
	DirectRows       int64   `json:"direct_value_rows"`
	DirectDistinct   int64   `json:"direct_distinct_values"`
	ActualRelease    int64   `json:"actual_release"`
	ActualInfluence  int64   `json:"actual_influence"`
	ActualOutcome    int64   `json:"actual_outcome"`
	ReplayCharged    int64   `json:"replay_charged_total"`
	WitnessPerAnswer float64 `json:"dependency_per_answer"`
}

// RunEKB5 counts counterparty values over the finance department's internal
// contracts, then repeats the request (pre-registered: the repeat charges
// nothing). label is "document" or "collection", the scope the profile's
// Catalog declares for the field.
func RunEKB5(ctx context.Context, d *Deployment, label string) (EKB5Observation, error) {
	department, classification, field := "finance", "internal", "counterparty"
	product := d.Product
	var directRows, directDistinct int64
	if err := d.Business.QueryRow(ctx, `SELECT count(*), count(DISTINCT canonical_value) FROM kb.extracted_values v JOIN kb.documents d ON d.collection = v.collection AND d.doc_id = v.doc_id AND d.version = v.version
		WHERE v.collection = $1 AND v.field = $2 AND d.department = $3 AND d.classification = $4`, product.Collection, field, department, classification).Scan(&directRows, &directDistinct); err != nil {
		return EKB5Observation{}, err
	}
	relation := product.Products.Values
	for _, rule := range product.ExtractionRules {
		if rule.Field == field && rule.Scope == catalog.KBExtractionScopeCollection {
			relation = product.Products.CollectionValues
		}
	}
	products := []string{product.Products.Values}
	columns := map[string][]string{product.Products.Values: {"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"}}
	if product.Products.CollectionValues != "" {
		products = append(products, product.Products.CollectionValues)
		columns[product.Products.CollectionValues] = []string{"field", "value_type", "canonical_value", "value_text", "value_numeric", "value_date", "kb_department", "classification"}
	}
	taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: "E-KB5 " + label + " count", Products: products, Columns: columns,
		Scope: map[string]any{"kb_department": []string{department}, "classification": []string{classification, "public"}}})
	if err != nil {
		return EKB5Observation{}, fmt.Errorf("provision: %w", err)
	}
	arguments := map[string]any{"task_id": taskID, "request_id": taskID + "-count", "knowledge_product": product.Name, "field": field, "aggregate": "count",
		"filters": []map[string]any{{"attribute": "classification", "op": "=", "value": classification}}}
	first, err := d.Retrieve(ctx, d.Alice, "aggregate", arguments)
	if err != nil {
		return EKB5Observation{}, err
	}
	if len(first.Rows) != 1 || len(first.Rows[0]) == 0 {
		return EKB5Observation{}, errors.New("aggregate delivered no single answer row")
	}
	observation := EKB5Observation{Publication: label, Deployment: d.Index, RelationUsed: relation, Answer: fmt.Sprint(first.Rows[0][len(first.Rows[0])-1]),
		DirectRows: directRows, DirectDistinct: directDistinct, ActualRelease: first.Charge.ActualRelease,
		ActualInfluence: first.Charge.ActualInfluence, ActualOutcome: first.Charge.ActualOutcome}
	arguments["request_id"] = taskID + "-count-again"
	again, err := d.Retrieve(ctx, d.Alice, "aggregate", arguments)
	if err != nil {
		return EKB5Observation{}, err
	}
	observation.ReplayCharged = again.Charge.ChargedRelease + again.Charge.ChargedInfluence + again.Charge.ChargedOutcome
	if first.Charge.ActualRelease > 0 {
		observation.WitnessPerAnswer = float64(first.Charge.ActualInfluence) / float64(first.Charge.ActualRelease)
	}
	return observation, nil
}

// ---- E-KB2 (a): the eligibility gate ----

// EKB2Outcome matches the pilot report's outcome entries. TasksAfter replaces
// the pilot's in-memory draft count: the gateway creates a task and its OA
// draft together, so the growth of a subject's task list (list_my_tasks, from
// a baseline taken before its first request) is the deployment-observable
// count of drafts its requests left behind.
type EKB2Outcome struct {
	Subject     string `json:"subject"`
	Role        string `json:"role"`
	Type        string `json:"type"`
	Code        string `json:"code,omitempty"`
	TaskCreated bool   `json:"task_created"`
	TasksAfter  int    `json:"tasks_after"`
}

// EKB2Counts are per-type totals.
type EKB2Counts struct {
	Requests, NotEligible, Admitted, Drafts, OtherErrors int
}

// EKB2Deployment matches the pilot report's deployments_detail entries.
type EKB2Deployment struct {
	Deployment int                    `json:"deployment"`
	ByType     map[string]*EKB2Counts `json:"by_type"`
	Outcomes   []EKB2Outcome          `json:"outcomes"`
	Seconds    float64                `json:"seconds"`
}

// RunEKB2a issues the pre-registered request matrix as the 60 file
// principals: own department and clearance; another department (the
// finance_controller's widening admits it); one classification above the
// clearance.
func RunEKB2a(ctx context.Context, d *Deployment) (EKB2Deployment, error) {
	started := time.Now()
	product := d.Product
	departments := []string{"engineering", "finance", "hr", "it", "legal", "marketing", "operations", "procurement", "product", "research", "sales", "support"}
	higher := map[string]string{"internal": "confidential", "confidential": "leadership"}
	request := func(department string, clearance []string) map[string]any {
		// The pilot runner's request (kbTaskRequest), verbatim.
		return map[string]any{"objective": "查阅本部门制度与合同", "data_products": []string{product.Products.Chunks, product.Products.Documents},
			"columns": map[string][]string{
				product.Products.Chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification"},
				product.Products.Documents: {"doc_id", "version", "title", "kb_department", "classification"},
			},
			"scopes": map[string]any{"kb_department": []string{department}, "classification": clearance}}
	}
	report := EKB2Deployment{Deployment: d.Index, ByType: map[string]*EKB2Counts{}}
	for _, subject := range EligibilitySubjects() {
		client := d.Principals[subject.Name]
		if client == nil {
			return report, fmt.Errorf("no token for subject %s", subject.Name)
		}
		controller := false
		for _, family := range subject.Families {
			controller = controller || family == "finance_controller"
		}
		baseline, err := client.Call(ctx, "list_my_tasks", map[string]any{})
		if err != nil {
			return report, fmt.Errorf("list_my_tasks %s: %w", subject.Name, err)
		}
		baselineList, _ := baseline["tasks"].([]any)
		before := len(baselineList)
		record := func(kind string, response map[string]any, err error) error {
			counts := report.ByType[kind]
			if counts == nil {
				counts = &EKB2Counts{}
				report.ByType[kind] = counts
			}
			counts.Requests++
			outcome := EKB2Outcome{Subject: subject.Name, Role: subject.Role, Type: kind}
			var toolErr *ToolError
			switch {
			case err == nil:
				counts.Admitted++
				outcome.TaskCreated = response["task_id"] != nil
			case errors.As(err, &toolErr):
				outcome.Code = toolErr.Code
				if toolErr.Code == "NOT_ELIGIBLE" {
					counts.NotEligible++
				} else {
					counts.OtherErrors++
				}
			default:
				return err
			}
			tasks, listErr := client.Call(ctx, "list_my_tasks", map[string]any{})
			if listErr != nil {
				return fmt.Errorf("list_my_tasks %s: %w", subject.Name, listErr)
			}
			list, _ := tasks["tasks"].([]any)
			outcome.TasksAfter = len(list) - before
			counts.Drafts = outcome.TasksAfter
			report.Outcomes = append(report.Outcomes, outcome)
			return nil
		}
		other := departments[(indexOf(departments, subject.Department)+1)%len(departments)]
		response, err := client.Call(ctx, "request_data_task", request(subject.Department, subject.Clearance))
		if err := record("own-department-own-clearance", response, err); err != nil {
			return report, err
		}
		kind := "other-department"
		if controller {
			kind = "controller-other-department"
		}
		response, err = client.Call(ctx, "request_data_task", request(other, subject.Clearance))
		if err := record(kind, response, err); err != nil {
			return report, err
		}
		if next, ok := higher[subject.Clearance[len(subject.Clearance)-1]]; ok {
			response, err = client.Call(ctx, "request_data_task", request(subject.Department, []string{next}))
			if err := record("own-department-higher-classification", response, err); err != nil {
				return report, err
			}
		}
	}
	report.Seconds = time.Since(started).Seconds()
	return report, nil
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
