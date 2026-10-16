package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
