package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

// E-KB2 (a), the eligibility gate (docs/kb_experiment_design.md, pilot
// class): 60 synthetic subjects (12 departments × 5 roles, clearance derived
// from the role, one finance controller) each file four request types against
// the knowledge product; the gate must refuse every request outside the
// subject's envelope before any OA draft exists and admit every request inside
// it. Repeated on KB_EKB2_DEPLOYMENTS fresh control stores. No business
// database is involved: eligibility precedes approval and execution.
//
//	CONTROL_TEST_POSTGRES_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH (required)
//	KB_EKB2_REPORT        output JSON path (required)
//	KB_EKB2_DEPLOYMENTS   default 3
func TestEKB2EligibilityGate(t *testing.T) {
	artifactDir, epoch, reportPath := os.Getenv("KB_E2E_ARTIFACT_DIR"), os.Getenv("KB_E2E_EPOCH"), os.Getenv("KB_EKB2_REPORT")
	if os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || artifactDir == "" || epoch == "" || reportPath == "" {
		t.Skip("CONTROL_TEST_POSTGRES_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH and KB_EKB2_REPORT are required")
	}
	deployments := 3
	if v := os.Getenv("KB_EKB2_DEPLOYMENTS"); v != "" {
		deployments, _ = strconv.Atoi(v)
	}
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// kb-publish emits a default envelope (department and clearance, no
	// widening). The experiment's rule adds the finance_controller widening
	// the design pre-registers; it replaces the emitted block exactly as the
	// author would sign it into the Catalog.
	published := "    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n        classification:\n          from_attribute: clearance\n"
	eligibility := "    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n          widen_if:\n            - {attribute: job_family, any_of: [finance_controller]}\n        classification:\n          from_attribute: clearance\n"
	text := string(fragment)
	if strings.Count(text, published) != 1 {
		t.Fatal("fragment does not carry the expected published eligibility block once")
	}
	text = strings.Replace(text, published, eligibility, 1)
	departments := []string{"engineering", "finance", "hr", "it", "legal", "marketing", "operations", "procurement", "product", "research", "sales", "support"}
	roles := []string{"staff", "hr", "manager", "director", "executive"}
	clearanceOf := func(role string) []string {
		switch role {
		case "manager":
			return []string{"public", "internal", "confidential"}
		case "director", "executive":
			return []string{"public", "internal", "confidential", "leadership"}
		default:
			return []string{"public", "internal"}
		}
	}
	type subject struct {
		id, name, department, role string
		clearance                  []string
		controller                 bool
	}
	var subjects []subject
	var snapshotText strings.Builder
	snapshotText.WriteString("version: taskgate-principal-attributes-v1\nsubjects:\n")
	for _, department := range departments {
		for _, role := range roles {
			name := department + "-" + role
			controller := department == "finance" && role == "director"
			families := role
			if controller {
				families = role + ", finance_controller"
			}
			snapshotText.WriteString("  - subject: " + name + "\n    attributes:\n      department: [" + department + "]\n      job_family: [" + families + "]\n      clearance: [" + strings.Join(clearanceOf(role), ", ") + "]\n")
			subjects = append(subjects, subject{id: "principal-" + name, name: name, department: department, role: role, clearance: clearanceOf(role), controller: controller})
		}
	}
	snapshot, err := principalattr.Parse([]byte(snapshotText.String()))
	if err != nil {
		t.Fatal(err)
	}
	higher := map[string]string{"internal": "confidential", "confidential": "leadership"}
	type outcome struct {
		Subject     string `json:"subject"`
		Role        string `json:"role"`
		Type        string `json:"type"`
		Code        string `json:"code,omitempty"`
		TaskCreated bool   `json:"task_created"`
		Drafts      int    `json:"drafts_after"`
	}
	type counts struct{ Requests, NotEligible, Admitted, Drafts, OtherErrors int }
	type deploymentReport struct {
		Deployment int                `json:"deployment"`
		ByType     map[string]*counts `json:"by_type"`
		Outcomes   []outcome          `json:"outcomes"`
		Seconds    float64            `json:"seconds"`
	}
	var reports []deploymentReport
	started := time.Now()
	ctx := context.Background()
	for d := 1; d <= deployments; d++ {
		deploymentStarted := time.Now()
		harness := newGatewayHarnessWithOptions(t, harnessOptions{principalAttributes: snapshot, rewriteCatalog: func(base string) string {
			return kbOnlyCatalog(t, base, text, "", "")
		}})
		principals := map[string]mcp.Principal{}
		for _, s := range subjects {
			principal := mcp.Principal{ID: s.id, Subject: s.name, Role: "query"}
			if err := harness.store.CreatePrincipal(ctx, control.Principal{ID: s.id, Subject: s.name, Role: "query", TokenHash: tokenDigestFor(s.name)}); err != nil {
				t.Fatalf("create principal %s: %v", s.name, err)
			}
			principals[s.name] = principal
		}
		report := deploymentReport{Deployment: d, ByType: map[string]*counts{}}
		record := func(s subject, kind string, err error, result map[string]any) {
			c := report.ByType[kind]
			if c == nil {
				c = &counts{}
				report.ByType[kind] = c
			}
			c.Requests++
			o := outcome{Subject: s.name, Role: s.role, Type: kind, Drafts: len(harness.approval.requests)}
			if err != nil {
				var toolErr *mcp.ToolError
				if asErr, ok := err.(*mcp.ToolError); ok {
					toolErr = asErr
				}
				if toolErr != nil {
					o.Code = toolErr.Code
				} else {
					o.Code = "ERROR"
				}
				if o.Code == apierr.CodeNotEligible {
					c.NotEligible++
				} else {
					c.OtherErrors++
				}
			} else {
				c.Admitted++
				o.TaskCreated = result["task_id"] != nil
			}
			c.Drafts = len(harness.approval.requests)
			report.Outcomes = append(report.Outcomes, o)
		}
		for i, s := range subjects {
			other := departments[(indexOf(departments, s.department)+1)%len(departments)]
			principal := principals[s.name]
			// T1: own department, own clearance.
			result, err := callGatewayTool(harness.service, principal, "request_data_task", kbTaskRequest(s.department, s.clearance))
			record(s, "own-department-own-clearance", err, result)
			// T2: another department, own clearance.
			result, err = callGatewayTool(harness.service, principal, "request_data_task", kbTaskRequest(other, s.clearance))
			if s.controller {
				record(s, "controller-other-department", err, result)
			} else {
				record(s, "other-department", err, result)
			}
			// T3: own department, one classification above the clearance.
			top := s.clearance[len(s.clearance)-1]
			if next, ok := higher[top]; ok {
				result, err = callGatewayTool(harness.service, principal, "request_data_task", kbTaskRequest(s.department, []string{next}))
				record(s, "own-department-higher-classification", err, result)
			}
			_ = i
		}
		report.Seconds = time.Since(deploymentStarted).Seconds()
		reports = append(reports, report)
	}
	summary := map[string]any{}
	for kind := range reports[0].ByType {
		total := &counts{}
		for _, report := range reports {
			if c := report.ByType[kind]; c != nil {
				total.Requests += c.Requests
				total.NotEligible += c.NotEligible
				total.Admitted += c.Admitted
				total.OtherErrors += c.OtherErrors
			}
		}
		summary[kind] = map[string]any{"requests": total.Requests, "not_eligible": total.NotEligible, "admitted": total.Admitted, "other_errors": total.OtherErrors,
			"not_eligible_rate": float64(total.NotEligible) / float64(total.Requests), "admitted_rate": float64(total.Admitted) / float64(total.Requests)}
	}
	out := map[string]any{"version": "taskgate-kb-ekb2-report-v1", "campaign_class": "kb-pilot", "publication_eligible": false,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "publication_epoch": epoch, "snapshot_sha256": snapshot.SHA256,
		"subjects": len(subjects), "deployments": deployments, "eligibility_rule": eligibility, "summary": summary, "deployments_detail": reports,
		"seconds": time.Since(started).Seconds()}
	encoded, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	t.Logf("E-KB2 report %s sha256 %s; summary %v", reportPath, hex.EncodeToString(sum[:])[:16], summary)
	// Pre-registered expectations.
	expect := map[string]string{"own-department-own-clearance": "admitted", "controller-other-department": "admitted", "other-department": "not_eligible", "own-department-higher-classification": "not_eligible"}
	for kind, want := range expect {
		entry, ok := summary[kind].(map[string]any)
		if !ok {
			t.Fatalf("no requests of type %s", kind)
		}
		if entry[want+"_rate"].(float64) != 1 {
			t.Fatalf("type %s: %s rate %.4f, want 1.0 (%v)", kind, want, entry[want+"_rate"], entry)
		}
	}
	// No draft for any refused request: drafts only ever advance on admitted ones.
	for _, report := range reports {
		admitted := 0
		for _, o := range report.Outcomes {
			if o.Code == "" {
				admitted++
			}
			if o.Drafts != admitted {
				t.Fatalf("deployment %d: %d OA drafts after %d admitted requests (%s %s)", report.Deployment, o.Drafts, admitted, o.Subject, o.Type)
			}
		}
	}
}

func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}
	return -1
}

func tokenDigestFor(subject string) string {
	sum := sha256.Sum256([]byte("ekb2-token-" + subject))
	return hex.EncodeToString(sum[:])
}

// E-KB2 (b), the misapproval arm: an approver grants a request that is inside
// the subject's envelope but broader than the task's purpose (own department,
// every classification the clearance admits). The S2 partition scan runs
// under that grant until the budget refuses or the TTL (2 minutes, advanced on
// the test clock) expires. Pre-registered expectation: leaked chunks ≤
// min(B_R cells / 4, requestable), and after expiry no request adds a chunk.
//
//	KB_E2E_*, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT, KB_EKB2B_REPORT (required); KB_EKB2_DEPLOYMENTS (default 3)
func TestEKB2MisapprovalArm(t *testing.T) {
	businessDSN, artifactDir, epoch := os.Getenv("KB_E2E_BUSINESS_DSN"), os.Getenv("KB_E2E_ARTIFACT_DIR"), os.Getenv("KB_E2E_EPOCH")
	endpoint, reportPath := strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")), os.Getenv("KB_EKB2B_REPORT")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || endpoint == "" || reportPath == "" {
		t.Skip("KB_E2E_*, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT and KB_EKB2B_REPORT are required")
	}
	deployments := 3
	if v := os.Getenv("KB_EKB2_DEPLOYMENTS"); v != "" {
		deployments, _ = strconv.Atoi(v)
	}
	const (
		budgetQueries = 256
		budgetRelease = 800
		budgetInflu   = 2400
		budgetOutcome = 64
		kMax          = 20
		ttl           = 2 * time.Minute
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := catalog.Parse([]byte(kbOnlyCatalog(t, readDemoCatalog(t), string(fragment), "", "")))
	if err != nil {
		t.Fatal(err)
	}
	built, err := catalogschema.Build(probe)
	if err != nil {
		t.Fatal(err)
	}
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
	if err != nil {
		t.Fatal(err)
	}
	attestation, err := probeConnector.Attestation(ctx)
	probeConnector.Close()
	if err != nil {
		t.Fatal(err)
	}
	parsedDSN, _ := url.Parse(businessDSN)
	database := strings.Trim(parsedDSN.Path, "/")
	product := probe.KnowledgeProducts[0]
	embedder, err := kbpublish.NewHTTPEmbedder(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	departments := []string{"sales", "finance", "hr"}
	// Misapproved grant: a manager-level clearance (public, internal,
	// confidential) for a task whose purpose needed far less.
	granted := []any{"public", "internal", "confidential"}
	type arm struct {
		Deployment      int    `json:"deployment"`
		Department      string `json:"department"`
		Requestable     int    `json:"requestable_chunks"`
		Steps           int    `json:"steps"`
		Leaked          int    `json:"leaked_chunks"`
		LeakBound       int    `json:"leak_bound_chunks"`
		FirstRefusal    int    `json:"first_refusal_step"`
		RefusalCode     string `json:"refusal_code"`
		AfterExpiryCode string `json:"after_expiry_code"`
		AfterExpiryNew  int    `json:"after_expiry_new_chunks"`
		ChargedRelease  int64  `json:"charged_release"`
	}
	var arms []arm
	for d := 1; d <= deployments; d++ {
		harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
			return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
		}})
		harness.taskBudget = &domain.Budget{MaxQueries: budgetQueries, MaxRows: 1000000, MaxDBTime: 2 * time.Hour, PerQueryTimeout: 20 * time.Second, TaskTTL: ttl}
		builtD, err := catalogschema.Build(harness.catalog)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 20 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 1000, MaxConnections: 4,
			ExpectedSchema: builtD.Entries, ExpectedSchemaDigest: builtD.Source.SchemaDigest,
			ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: builtD.Source.DatasourceID, Database: builtD.Source.Database, User: builtD.Source.User, PostgreSQLMajorVersion: builtD.Source.PostgreSQLMajorVersion}})
		if err != nil {
			t.Fatal(err)
		}
		att, err := connector.Attestation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		harness.connector.attestation = att
		harness.service.connector = connector
		installKnowledgeRegistry(t, harness, artifactDir)
		harness.service.kbEmbedder = embedder
		chunks, documents := product.Products.Chunks, product.Products.Documents
		department := departments[(d-1)%len(departments)]
		taskID := fmt.Sprintf("ekb2b-d%d-%s", d, department)
		harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
			control.ExposureLimits{ReleaseFacts: budgetRelease, InfluenceFacts: budgetInflu, OutcomeFacts: budgetOutcome}, exposure.ProfileV5,
			[]string{chunks, documents}, map[string][]string{
				chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
				documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
			}, domain.SensitivityHigh, map[string]any{"kb_department": []any{department}, "classification": granted})
		var requestable int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal','confidential')`, product.Collection, department).Scan(&requestable); err != nil {
			t.Fatal(err)
		}
		a := arm{Deployment: d, Department: department, Requestable: requestable, LeakBound: budgetRelease / 4}
		if requestable < a.LeakBound {
			a.LeakBound = requestable
		}
		departmentZH := map[string]string{"sales": "销售部", "finance": "财务部", "hr": "人力资源部"}[department]
		generic := departmentZH + "制度 合同 审批 报告"
		// E-KB1's S2: job_role × six valid_until buckets (30 partitions), then
		// S1 vocabulary texts, so the arm keeps asking for new chunks until the
		// ledger refuses rather than repeating requests the ledger replays for free.
		var minDate, maxDate *time.Time
		if err := db.QueryRow(ctx, `SELECT min(valid_until), max(valid_until) FROM kb.chunks WHERE collection = $1 AND department = $2 AND valid_until IS NOT NULL`, product.Collection, department).Scan(&minDate, &maxDate); err != nil {
			t.Fatal(err)
		}
		type partition struct{ role, from, to string }
		var parts []partition
		for _, role := range []string{"staff", "manager", "director", "executive", "hr"} {
			span := maxDate.Sub(*minDate) / 6
			for b := 0; b < 6; b++ {
				from, to := minDate.Add(time.Duration(b)*span), minDate.Add(time.Duration(b+1)*span)
				if b == 5 {
					to = maxDate.AddDate(0, 0, 1)
				}
				parts = append(parts, partition{role, from.Format("2006-01-02"), to.Format("2006-01-02")})
			}
		}
		words := []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更"}
		seen := map[string]struct{}{}
		step := 0
		for step < 400 {
			step++
			args := map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-step%d", taskID, step), "knowledge_product": product.Name, "k": kMax}
			if step <= len(parts) {
				part := parts[step-1]
				args["query"] = map[string]any{"text": generic}
				args["filters"] = []map[string]any{{"attribute": "job_role", "op": "=", "value": part.role},
					{"attribute": "valid_until", "op": ">=", "value": part.from}, {"attribute": "valid_until", "op": "<", "value": part.to}}
			} else {
				word := words[(step-len(parts)-1)%len(words)]
				template := []string{"%s%s制度", "%s审批流程", "关于%s的规定", "%s报告"}[((step-len(parts)-1)/len(words))%4]
				text := fmt.Sprintf(template, word)
				if strings.Count(template, "%s") == 2 {
					text = fmt.Sprintf(template, departmentZH, word)
				}
				args["query"] = map[string]any{"text": text}
			}
			result, err := callGatewayTool(harness.service, harness.alice, "search", args)
			if err != nil {
				code := "ERROR"
				if toolErr, ok := err.(*mcp.ToolError); ok {
					code = toolErr.Code
				}
				a.FirstRefusal, a.RefusalCode = step, code
				break
			}
			for _, hit := range searchHits(t, result) {
				seen[hitID(hit)] = struct{}{}
			}
			addCharge(result, &a.ChargedRelease, new(int64), new(int64))
		}
		a.Steps, a.Leaked = step, len(seen)
		// Expiry: advance the clock past the TTL; a further request must be
		// refused and add nothing.
		harness.clock.value = harness.clock.value.Add(ttl + time.Second)
		before := len(seen)
		result, err := callGatewayTool(harness.service, harness.alice, "search", map[string]any{"task_id": taskID, "request_id": taskID + "-after-expiry",
			"knowledge_product": product.Name, "query": map[string]any{"text": generic}, "k": kMax})
		if err != nil {
			if toolErr, ok := err.(*mcp.ToolError); ok {
				a.AfterExpiryCode = toolErr.Code
			} else {
				a.AfterExpiryCode = "ERROR"
			}
		} else {
			for _, hit := range searchHits(t, result) {
				seen[hitID(hit)] = struct{}{}
			}
		}
		a.AfterExpiryNew = len(seen) - before
		arms = append(arms, a)
		t.Logf("d%d %s: requestable=%d leaked=%d bound=%d first_refusal=%d %s charged_release=%d after_expiry=%s new=%d", d, department, requestable, a.Leaked, a.LeakBound, a.FirstRefusal, a.RefusalCode, a.ChargedRelease, a.AfterExpiryCode, a.AfterExpiryNew)
		connector.Close()
	}
	out := map[string]any{"version": "taskgate-kb-ekb2b-report-v1", "campaign_class": "kb-pilot", "publication_eligible": false,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "publication_epoch": epoch, "ttl_seconds": int(ttl.Seconds()),
		"budget": map[string]any{"queries": budgetQueries, "release_cells": budgetRelease, "influence": budgetInflu, "outcome": budgetOutcome, "k_max": kMax}, "arms": arms}
	encoded, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	t.Logf("E-KB2(b) report %s sha256 %s", reportPath, hex.EncodeToString(sum[:])[:16])
	for _, a := range arms {
		if a.Leaked > a.LeakBound {
			t.Fatalf("deployment %d leaked %d chunks, above the bound %d", a.Deployment, a.Leaked, a.LeakBound)
		}
		if a.RefusalCode != apierr.CodeExposureBudgetExhausted {
			t.Fatalf("deployment %d: scan ended with %q at step %d, want the exposure budget refusal", a.Deployment, a.RefusalCode, a.FirstRefusal)
		}
		if a.AfterExpiryNew != 0 || a.AfterExpiryCode == "" {
			t.Fatalf("deployment %d: after TTL expiry code %q, new chunks %d", a.Deployment, a.AfterExpiryCode, a.AfterExpiryNew)
		}
	}
}
