package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

// The knowledge end-to-end test runs the search tool against a real published
// knowledge product (kb-design §10a, plan K3.3): a business database holding
// kb.* plus the published reporting relations and their ordinal sidecars, the
// artifact root kb-publish wrote, and the control store. It is the acceptance
// path for retrieval settlement: hit set, three-dimensional charge, idempotent
// and semantic replay, novelty across overlapping hit sets, and the closed
// refusal when the exposure budget cannot cover a search.
//
//	KB_E2E_BUSINESS_DSN   reader DSN of the business database (gateway_reader)
//	KB_E2E_ARTIFACT_DIR   kb-publish -artifact-dir root
//	KB_E2E_EPOCH          the publication epoch under <artifact>/kb/<epoch>/
//	CONTROL_TEST_POSTGRES_DSN
//	KB_EMBEDDING_ENDPOINT (optional) enables the text-query case
func TestSearchEndToEndAgainstKnowledgePublication(t *testing.T) {
	businessDSN := os.Getenv("KB_E2E_BUSINESS_DSN")
	artifactDir := os.Getenv("KB_E2E_ARTIFACT_DIR")
	epoch := os.Getenv("KB_E2E_EPOCH")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" {
		t.Skip("KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH and CONTROL_TEST_POSTGRES_DSN are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	demo, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Phase 1: a KB-only catalog with the demo source, to learn the live
	// attestation of the published relations.
	probeText := kbOnlyCatalog(t, string(demo), string(fragment), "", "")
	probe, err := catalog.Parse([]byte(probeText))
	if err != nil {
		t.Fatalf("parse KB-only catalog: %v", err)
	}
	built, err := catalogschema.Build(probe)
	if err != nil {
		t.Fatal(err)
	}
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2,
		ExpectedSchema: built.Entries,
	})
	if err != nil {
		t.Fatalf("probe connector: %v", err)
	}
	attestation, err := probeConnector.Attestation(ctx)
	probeConnector.Close()
	if err != nil {
		t.Fatalf("probe attestation: %v", err)
	}
	// Phase 2: the harness on the attested catalog. The database name comes
	// from the DSN; the probe attestation carries only the schema digest.
	parsedDSN, err := url.Parse(businessDSN)
	if err != nil || strings.Trim(parsedDSN.Path, "/") == "" {
		t.Fatalf("KB_E2E_BUSINESS_DSN must be a URL naming the database: %v", err)
	}
	database := strings.Trim(parsedDSN.Path, "/")
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
		return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
	}})
	built, err = catalogschema.Build(harness.catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("phase 1 attestation %+v; phase 2 expects source %+v entries=%d", attestation, built.Source, len(built.Entries))
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2,
		ExpectedSchema: built.Entries, ExpectedSchemaDigest: built.Source.SchemaDigest,
		ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: built.Source.DatasourceID, Database: built.Source.Database,
			User: built.Source.User, PostgreSQLMajorVersion: built.Source.PostgreSQLMajorVersion},
	})
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	defer connector.Close()
	if attestation, err = connector.Attestation(ctx); err != nil {
		t.Fatal(err)
	}
	harness.connector.attestation = attestation
	harness.service.connector = connector
	installKnowledgeRegistry(t, harness, artifactDir)
	if endpoint := strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")); endpoint != "" {
		embedder, err := kbpublish.NewHTTPEmbedder(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		harness.service.kbEmbedder = embedder
	}
	product := harness.catalog.KnowledgeProducts[0]
	chunks, documents := product.Products.Chunks, product.Products.Documents
	columns := map[string][]string{
		chunks:    {"doc_id", "version", "chunk_index", "text", "kb_department", "classification", "job_role", "valid_until"},
		documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
	}
	scope := map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal", "public"}}
	harness.createTaskWithGrantScopeAndExposureProfile(t, "task-kb-e2e", nil,
		control.ExposureLimits{ReleaseFacts: 400, InfluenceFacts: 4000, OutcomeFacts: 64}, exposure.ProfileV5,
		[]string{chunks, documents}, columns, domain.SensitivityHigh, scope)

	db, err := pgx.Connect(ctx, businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	// The query vector is a stored chunk vector from inside the task's scope,
	// so the hit set is non-empty and independent of any embedding service.
	var seedDoc, seedVector string
	var seedVersion, seedChunk int
	if err := db.QueryRow(ctx, `SELECT doc_id, version, chunk_index, embedding::text FROM kb.chunks
		WHERE collection = $1 AND department = 'finance' AND classification = 'internal'
		ORDER BY md5('kb-e2e:' || text_sha256) LIMIT 1`, product.Collection).Scan(&seedDoc, &seedVersion, &seedChunk, &seedVector); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	vector := parseVectorLiteral(t, seedVector)
	search := func(requestID string, k int, overrides map[string]any) (map[string]any, error) {
		args := map[string]any{"task_id": "task-kb-e2e", "request_id": requestID, "knowledge_product": product.Name,
			"filters": []map[string]any{{"attribute": "classification", "op": "=", "value": "internal"}},
			"query":   map[string]any{"vector": vector}, "k": k}
		for key, value := range overrides {
			args[key] = value
		}
		return callGatewayTool(harness.service, harness.alice, "search", args)
	}

	first, err := search("kb-e2e-1", 5, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	hits := searchHits(t, first)
	if len(hits) != 5 {
		t.Fatalf("search returned %d rows, want 5", len(hits))
	}
	if hits[0] != [3]any{seedDoc, int64(seedVersion), int64(seedChunk)} && !sameHit(hits[0], seedDoc, seedVersion, seedChunk) {
		t.Fatalf("the seed chunk is not the first hit: %#v", hits[0])
	}
	expected := exactHits(t, ctx, db, product.Collection, seedVector, 5)
	for i := range hits {
		if !sameHit(hits[i], expected[i].doc, expected[i].version, expected[i].chunk) {
			t.Fatalf("hit %d differs from exact retrieval: gateway %#v, direct %+v", i, hits[i], expected[i])
		}
	}
	charge := first["exposure"].(control.ExposureCharge)
	documentsHit := distinctDocuments(hits)
	t.Logf("search k=5: release actual=%d charged=%d influence actual=%d charged=%d outcome actual=%d charged=%d atoms=%d composite=%s",
		charge.ActualReleaseFacts, charge.ChargedReleaseFacts, charge.ActualInfluenceFacts, charge.ChargedInfluenceFacts,
		charge.ActualOutcomeFacts, charge.ChargedOutcomeFacts, charge.ActualPredicateAtomCount, charge.CompositeOutcomeSHA256)
	// §9.9: the four visible cells of every hit are Release Facts.
	if charge.ActualReleaseFacts != 4*int64(len(hits)) || charge.ChargedReleaseFacts != charge.ActualReleaseFacts {
		t.Fatalf("release charge %+v, want 4 cells per hit on a fresh ledger", charge)
	}
	// Dependency covers Release plus the chunk rows and the document rows.
	if charge.ActualInfluenceFacts < charge.ActualReleaseFacts+int64(len(hits))+int64(documentsHit) {
		t.Fatalf("influence %d below release+chunk rows+document rows (%d+%d+%d)", charge.ActualInfluenceFacts,
			charge.ActualReleaseFacts, len(hits), documentsHit)
	}
	// Outcome: one caller atom (classification = internal) plus the composite.
	if charge.ActualPredicateAtomCount != 1 || charge.ActualOutcomeFacts != 2 || charge.CompositeOutcomeSHA256 == "" {
		t.Fatalf("outcome charge %+v, want 1 atom + 1 composite", charge)
	}

	// Idempotent replay: same request id, same body.
	replayed, err := search("kb-e2e-1", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replayed["idempotent_replay"] != true || len(searchHits(t, replayed)) != 5 {
		t.Fatalf("idempotent replay did not return the stored result: %#v", replayed["idempotent_replay"])
	}
	// Semantic replay: a new request with the same normal form settles 0/0/0.
	again, err := search("kb-e2e-2", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	againCharge := again["exposure"].(control.ExposureCharge)
	if againCharge.ChargedReleaseFacts != 0 || againCharge.ChargedInfluenceFacts != 0 || againCharge.ChargedOutcomeFacts != 0 {
		t.Fatalf("same normal form charged again: %+v", againCharge)
	}
	if !equalHits(searchHits(t, again), hits) {
		t.Fatal("semantic replay returned a different hit set")
	}
	// Novelty: k=10 shares the top-5 prefix under the total order, so only the
	// five new chunks' cells are charged.
	wider, err := search("kb-e2e-3", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	widerHits := searchHits(t, wider)
	if len(widerHits) != 10 || !equalHits(widerHits[:5], hits) {
		t.Fatalf("k=10 does not extend the k=5 hit set: %#v", widerHits)
	}
	widerCharge := wider["exposure"].(control.ExposureCharge)
	if widerCharge.ChargedReleaseFacts != 4*5 {
		t.Fatalf("k=10 charged %d release facts, want 20 for five new chunks", widerCharge.ChargedReleaseFacts)
	}
	if widerCharge.ChargedOutcomeFacts != 1 {
		t.Fatalf("k=10 charged %d outcome facts, want 1 (new composite, same atom)", widerCharge.ChargedOutcomeFacts)
	}

	// Over the limit: a task whose release budget cannot hold one search is
	// refused closed, before anything is charged.
	harness.createTaskWithGrantScopeAndExposureProfile(t, "task-kb-tight", nil,
		control.ExposureLimits{ReleaseFacts: 8, InfluenceFacts: 4000, OutcomeFacts: 64}, exposure.ProfileV5,
		[]string{chunks, documents}, columns, domain.SensitivityHigh, scope)
	_, err = search("kb-e2e-tight", 5, map[string]any{"task_id": "task-kb-tight"})
	requireToolCode(t, err, apierr.CodeExposureBudgetExhausted)

	// Text query through the trusted embedding service, when configured: the
	// gateway's hit set must equal direct exact retrieval with the same vector.
	if harness.service.kbEmbedder != nil {
		text := "差旅报销的审批流程"
		vectors, err := harness.service.kbEmbedder.Embed([]string{text})
		if err != nil || len(vectors) != 1 {
			t.Fatalf("embedding service: %v", err)
		}
		textResult, err := search("kb-e2e-text", 5, map[string]any{"query": map[string]any{"text": text}})
		if err != nil {
			t.Fatalf("text search: %v", err)
		}
		textHits := searchHits(t, textResult)
		direct := exactHits(t, ctx, db, product.Collection, vectorLiteralOf(vectors[0]), 5)
		for i := range direct {
			if i >= len(textHits) || !sameHit(textHits[i], direct[i].doc, direct[i].version, direct[i].chunk) {
				t.Fatalf("text search hit %d differs from direct exact retrieval: %#v vs %+v", i, textHits, direct)
			}
		}
		t.Logf("text search k=5 matches direct exact retrieval member by member; first hit %+v", direct[0])
	}
}

// kbOnlyCatalog turns the demo Catalog into a knowledge-only Catalog: the
// demo source (re-pointed at the attested database and digest when given),
// the demo approval routes that are not scoped to demo products, no demo
// publications or products, then the kb-publish fragment.
func kbOnlyCatalog(t *testing.T, demo, fragment, database, schemaDigest string) string {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal([]byte(demo), &document); err != nil {
		t.Fatalf("decode demo catalog: %v", err)
	}
	document["snapshot_publications"] = []any{}
	document["products"] = []any{}
	routes, _ := document["approval_routes"].([]any)
	kept := make([]any, 0, len(routes))
	for _, route := range routes {
		if entry, ok := route.(map[string]any); ok {
			if _, scoped := entry["products"]; scoped {
				continue
			}
		}
		kept = append(kept, route)
	}
	document["approval_routes"] = kept
	if sources, ok := document["sources"].([]any); ok {
		for _, source := range sources {
			entry, ok := source.(map[string]any)
			if !ok {
				continue
			}
			if database != "" {
				entry["database"] = database
			}
			if schemaDigest != "" {
				entry["schema_digest"] = schemaDigest
			}
		}
	}
	base, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := catalog.MergeYAMLFragment(base, []byte(fragment))
	if err != nil {
		t.Fatalf("merge knowledge fragment: %v", err)
	}
	return string(merged)
}

// installKnowledgeRegistry registers every Catalog publication from the
// artifact root the way cmd/gateway's loader does: the bundle manifest names
// the HOT file, the HOT dictionary is parsed against the Catalog's manifest
// digest, registered, and persisted to the control store.
func installKnowledgeRegistry(t *testing.T, harness *gatewayHarness, artifactDir string) {
	t.Helper()
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, publication := range harness.catalog.SnapshotPublications {
		directory := filepath.Join(artifactDir, publication.Name)
		manifestFile, err := os.Open(filepath.Join(directory, publication.Name+".bundle.json"))
		if err != nil {
			t.Fatalf("open bundle manifest %s: %v", publication.Name, err)
		}
		bundle, err := snapshotbundle.DecodeBundleManifest(manifestFile)
		manifestFile.Close()
		if err != nil {
			t.Fatalf("decode bundle manifest %s: %v", publication.Name, err)
		}
		hotBytes, err := os.ReadFile(filepath.Join(directory, bundle.Hot.Name))
		if err != nil {
			t.Fatal(err)
		}
		hot, err := ordinal.ParseHotDictionary(hotBytes, publication.ManifestDigest)
		if err != nil {
			t.Fatalf("parse HOT %s: %v", publication.Name, err)
		}
		if hot.DictionaryDigest() != publication.DictionaryDigest || hot.Manifest().SidecarDigest != publication.SidecarDigest {
			t.Fatalf("publication %s does not match the Catalog", publication.Name)
		}
		if err := registry.RegisterPublication(ordinal.PublicationKey{CatalogDigest: harness.catalog.SHA256, PublicationName: publication.Name},
			publication.ManifestDigest, hot); err != nil {
			t.Fatal(err)
		}
		if err := harness.store.PutOrdinalSnapshotPublication(context.Background(), publication.ManifestDigest, hot, nil); err != nil {
			t.Fatal(err)
		}
	}
	harness.service.snapshotRegistry = registry
}

type directHit struct {
	doc            string
	version, chunk int
}

func exactHits(t *testing.T, ctx context.Context, db *pgx.Conn, collection, vector string, k int) []directHit {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT doc_id, version, chunk_index FROM kb.chunks
		WHERE collection = $1 AND department = 'finance' AND classification = 'internal'
		ORDER BY embedding <=> $2::vector, doc_id, version, chunk_index LIMIT $3`, collection, vector, k)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hits []directHit
	for rows.Next() {
		var hit directHit
		if err := rows.Scan(&hit.doc, &hit.version, &hit.chunk); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, hit)
	}
	return hits
}

// searchHits decodes the response rows into (doc_id, version, chunk_index).
func searchHits(t *testing.T, result map[string]any) [][3]any {
	t.Helper()
	encoded, err := json.Marshal(result["rows"])
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatalf("rows have shape %T: %v", result["rows"], err)
	}
	hits := make([][3]any, 0, len(rows))
	for _, row := range rows {
		if len(row) != 4 {
			t.Fatalf("search row has %d columns, want 4: %#v", len(row), row)
		}
		hits = append(hits, [3]any{row[0], row[1], row[2]})
	}
	return hits
}

func sameHit(hit [3]any, doc string, version, chunk int) bool {
	return fmt.Sprint(hit[0]) == doc && numberEquals(hit[1], version) && numberEquals(hit[2], chunk)
}

func numberEquals(value any, want int) bool {
	switch typed := value.(type) {
	case float64:
		return typed == float64(want)
	case int64:
		return typed == int64(want)
	case int:
		return typed == want
	case json.Number:
		f, err := typed.Float64()
		return err == nil && f == float64(want)
	case string:
		return typed == fmt.Sprint(want)
	default:
		return false
	}
}

func equalHits(left, right [][3]any) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if fmt.Sprint(left[i]) != fmt.Sprint(right[i]) {
			return false
		}
	}
	return true
}

func distinctDocuments(hits [][3]any) int {
	seen := map[string]struct{}{}
	for _, hit := range hits {
		seen[fmt.Sprint(hit[0], "#", hit[1])] = struct{}{}
	}
	return len(seen)
}

func parseVectorLiteral(t *testing.T, literal string) []float64 {
	t.Helper()
	trimmed := strings.Trim(strings.TrimSpace(literal), "[]")
	parts := strings.Split(trimmed, ",")
	vector := make([]float64, 0, len(parts))
	for _, part := range parts {
		var value float64
		if _, err := fmt.Sscanf(strings.TrimSpace(part), "%g", &value); err != nil {
			t.Fatalf("vector literal component %q: %v", part, err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			t.Fatalf("vector literal component %q is not finite", part)
		}
		vector = append(vector, value)
	}
	return vector
}

func vectorLiteralOf(vector []float32) string {
	parts := make([]string, 0, len(vector))
	for _, value := range vector {
		parts = append(parts, fmt.Sprintf("%g", value))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
