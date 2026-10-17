package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

// E-KB5 collection-scoped values (kb-design §9.3, author decision 2026-10-17,
// pilot class). The same aggregate, "count the counterparties of this
// department's contracts", is settled on two publications of the same corpus:
// one where counterparty is document-scoped (one value Fact per contract) and
// one where it is collection-scoped (one value Fact per distinct counterparty
// within the department and classification). The report records the answer,
// the Release / Dependency / Outcome charges, and the direct distinct count.
//
//	KB_EKB5_DOC_DSN / KB_EKB5_DOC_ARTIFACT_DIR / KB_EKB5_DOC_EPOCH      document-scoped publication
//	KB_EKB5_COLL_DSN / KB_EKB5_COLL_ARTIFACT_DIR / KB_EKB5_COLL_EPOCH   collection-scoped publication
//	CONTROL_TEST_POSTGRES_DSN, KB_EKB5_REPORT (required); KB_EKB5_DEPLOYMENTS (default 3)
func TestEKB5CollectionScopedValues(t *testing.T) {
	type pub struct{ label, dsn, artifactDir, epoch string }
	pubs := []pub{
		{"document", os.Getenv("KB_EKB5_DOC_DSN"), os.Getenv("KB_EKB5_DOC_ARTIFACT_DIR"), os.Getenv("KB_EKB5_DOC_EPOCH")},
		{"collection", os.Getenv("KB_EKB5_COLL_DSN"), os.Getenv("KB_EKB5_COLL_ARTIFACT_DIR"), os.Getenv("KB_EKB5_COLL_EPOCH")},
	}
	reportPath := os.Getenv("KB_EKB5_REPORT")
	for _, p := range pubs {
		if p.dsn == "" || p.artifactDir == "" || p.epoch == "" {
			t.Skip("KB_EKB5_DOC_* and KB_EKB5_COLL_* are required")
		}
	}
	if os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || reportPath == "" {
		t.Skip("CONTROL_TEST_POSTGRES_DSN and KB_EKB5_REPORT are required")
	}
	deployments := 3
	if v := os.Getenv("KB_EKB5_DEPLOYMENTS"); v != "" {
		deployments, _ = strconv.Atoi(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	department, classification, field := "finance", "internal", "counterparty"
	type observation struct {
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
	var observations []observation
	for _, p := range pubs {
		fragment, err := os.ReadFile(filepath.Join(p.artifactDir, "kb", p.epoch, "catalog-fragment.yaml"))
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
		probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: p.dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
		if err != nil {
			t.Fatal(err)
		}
		attestation, err := probeConnector.Attestation(ctx)
		probeConnector.Close()
		if err != nil {
			t.Fatal(err)
		}
		parsedDSN, _ := url.Parse(p.dsn)
		database := strings.Trim(parsedDSN.Path, "/")
		product := probe.KnowledgeProducts[0]
		db, err := pgx.Connect(ctx, p.dsn)
		if err != nil {
			t.Fatal(err)
		}
		var directRows, directDistinct int64
		if err := db.QueryRow(ctx, `SELECT count(*), count(DISTINCT canonical_value) FROM kb.extracted_values v JOIN kb.documents d ON d.collection = v.collection AND d.doc_id = v.doc_id AND d.version = v.version
			WHERE v.collection = $1 AND v.field = $2 AND d.department = $3 AND d.classification = $4`, product.Collection, field, department, classification).Scan(&directRows, &directDistinct); err != nil {
			t.Fatal(err)
		}
		db.Close(ctx)
		relation := product.Products.Values
		for _, rule := range product.ExtractionRules {
			if rule.Field == field && rule.Scope == catalog.KBExtractionScopeCollection {
				relation = product.Products.CollectionValues
			}
		}
		for d := 1; d <= deployments; d++ {
			harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
				return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
			}})
			builtD, err := catalogschema.Build(harness.catalog)
			if err != nil {
				t.Fatal(err)
			}
			connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: p.dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 1000, MaxConnections: 4,
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
			installKnowledgeRegistry(t, harness, p.artifactDir)
			approved := []string{product.Products.Values}
			columns := map[string][]string{product.Products.Values: {"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"}}
			if product.Products.CollectionValues != "" {
				approved = append(approved, product.Products.CollectionValues)
				columns[product.Products.CollectionValues] = []string{"field", "value_type", "canonical_value", "value_text", "value_numeric", "value_date", "kb_department", "classification"}
			}
			taskID := fmt.Sprintf("ekb5-%s-d%d", p.label, d)
			harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
				control.ExposureLimits{ReleaseFacts: 100000, InfluenceFacts: 10000000, OutcomeFacts: 1000}, exposure.ProfileV5,
				approved, columns, domain.SensitivityHigh,
				map[string]any{"kb_department": []any{department}, "classification": []any{classification, "public"}})
			args := map[string]any{"task_id": taskID, "request_id": taskID + "-count", "knowledge_product": product.Name, "field": field, "aggregate": "count",
				"filters": []map[string]any{{"attribute": "classification", "op": "=", "value": classification}}}
			result, err := callGatewayTool(harness.service, harness.alice, "aggregate", args)
			if err != nil {
				t.Fatalf("%s aggregate: %v", p.label, err)
			}
			charge := result["exposure"].(control.ExposureCharge)
			obs := observation{Publication: p.label, Deployment: d, RelationUsed: relation, Answer: fmt.Sprint(aggregateNumber(t, result)),
				DirectRows: directRows, DirectDistinct: directDistinct, ActualRelease: charge.ActualReleaseFacts, ActualInfluence: charge.ActualInfluenceFacts, ActualOutcome: charge.ActualOutcomeFacts}
			args["request_id"] = taskID + "-count-again"
			again, err := callGatewayTool(harness.service, harness.alice, "aggregate", args)
			if err != nil {
				t.Fatal(err)
			}
			againCharge := again["exposure"].(control.ExposureCharge)
			obs.ReplayCharged = againCharge.ChargedReleaseFacts + againCharge.ChargedInfluenceFacts + againCharge.ChargedOutcomeFacts
			if charge.ActualReleaseFacts > 0 {
				obs.WitnessPerAnswer = float64(charge.ActualInfluenceFacts) / float64(charge.ActualReleaseFacts)
			}
			observations = append(observations, obs)
			t.Logf("%s d%d relation=%s answer=%s direct_rows=%d direct_distinct=%d R/D/O=%d/%d/%d replay_charged=%d", p.label, d, relation, obs.Answer, directRows, directDistinct,
				charge.ActualReleaseFacts, charge.ActualInfluenceFacts, charge.ActualOutcomeFacts, obs.ReplayCharged)
			connector.Close()
		}
	}
	out := map[string]any{"version": "taskgate-kb-ekb5-report-v1", "campaign_class": "kb-pilot", "publication_eligible": false,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "department": department, "classification": classification, "field": field,
		"publications": map[string]any{"document": pubs[0].epoch, "collection": pubs[1].epoch}, "deployments": deployments, "observations": observations}
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
	t.Logf("E-KB5 report %s sha256 %s", reportPath, hex.EncodeToString(sum[:])[:16])
	// Pre-registered shape: the collection-scoped arm's dependency is the number
	// of distinct values (times the cells per row), the document-scoped arm's is
	// the number of value rows; both replay to zero.
	for _, obs := range observations {
		if obs.ReplayCharged != 0 {
			t.Fatalf("%s d%d: replay charged %d", obs.Publication, obs.Deployment, obs.ReplayCharged)
		}
	}
}
