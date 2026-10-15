package kbpublish

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const testReaderPassword = "kb-reader-test"

type scratch struct {
	adminDSN  string
	readerDSN string
	db        *sql.DB
}

// newScratch prepares an isolated database the way db/init does for the
// relations the publisher touches: reporting schema with the datasource
// attestation singleton, the kb schema, the NOLOGIN snapshot owner, and a
// LOGIN gateway_reader without administrative attributes.
func newScratch(t *testing.T, sourceID string) scratch {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("KB_TEST_POSTGRES_DSN"))
	if base == "" {
		t.Skip("KB_TEST_POSTGRES_DSN (a pgvector-enabled PostgreSQL) is required for kb-publish tests")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := fmt.Sprintf("kb_publish_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close()
	})
	adminURL := *parsed
	adminURL.Path = "/" + name
	query := adminURL.Query()
	query.Set("default_query_exec_mode", "simple_protocol")
	adminURL.RawQuery = query.Encode()
	db, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schema, err := os.ReadFile(filepath.Join("..", "..", "db", "init", "kb-schema.psql"))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_reader') THEN CREATE ROLE gateway_reader NOLOGIN; END IF; END $$`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'taskgate_snapshot_owner') THEN CREATE ROLE taskgate_snapshot_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS; END IF; END $$`,
		`ALTER ROLE gateway_reader LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD '` + testReaderPassword + `'`,
		`CREATE SCHEMA reporting`,
		`CREATE TABLE reporting.datasource_attestation (singleton boolean PRIMARY KEY DEFAULT TRUE CHECK (singleton), datasource_id text NOT NULL)`,
		`INSERT INTO reporting.datasource_attestation (singleton, datasource_id) VALUES (TRUE, '` + sourceID + `')`,
		`GRANT USAGE ON SCHEMA reporting TO gateway_reader`,
		`GRANT SELECT ON reporting.datasource_attestation TO gateway_reader`,
		string(schema),
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", firstLine(statement), err)
		}
	}
	readerURL := *parsed
	readerURL.User = url.UserPassword("gateway_reader", testReaderPassword)
	readerURL.Path = "/" + name
	return scratch{adminDSN: adminURL.String(), readerDSN: readerURL.String(), db: db}
}

func firstLine(statement string) string {
	line, _, _ := strings.Cut(statement, "\n")
	if len(line) > 80 {
		line = line[:80]
	}
	return line
}

func testCorpus(t *testing.T, dir string, documents int) (string, string) {
	t.Helper()
	_, jsonl, manifest, err := kbcorpus.Build(kbcorpus.Spec{Seed: 11, Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "documents.jsonl")
	if err := os.WriteFile(path, jsonl, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, manifest.DocumentsSHA256
}

func testSpec(corpusPath, corpusSHA string) Spec {
	return Spec{Version: SpecVersion, KnowledgeProduct: "policy_kb", Description: "test knowledge product", Sensitivity: "high",
		DocumentTypes: []string{"contract", "approval", "policy", "report"}, Retrieval: Retrieval{KMax: 20, Aggregates: []string{"count", "sum"}},
		Collection: "policy_v1", SourceID: "taskgate-kb-test",
		CatalogSource: "travel_demo", SourceNamespace: "kb.policy", Epoch: "policy-kb-test-v1",
		SchemaDigest: strings.Repeat("ab", 32), Corpus: CorpusRef{Path: corpusPath, SHA256: corpusSHA},
		Segmentation: Segmentation{Function: SegmentationSectionWindowV1, WindowChars: 400},
		Embedding:    Embedding{Model: EmbeddingHashProjectionV1, Dimension: 64},
		ExtractionRules: []ExtractionRule{
			{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
			{DocumentType: "contract", Field: "counterparty", Type: "text", Source: "frontmatter", Scope: "document"},
			{DocumentType: "approval", Field: "approved_amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
			{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
		}}
}

func TestPublishIsDeterministicAcrossDatabasesAndVerifies(t *testing.T) {
	dir := t.TempDir()
	corpusPath, corpusSHA := testCorpus(t, dir, 40)
	spec := testSpec(corpusPath, corpusSHA)
	ctx := context.Background()

	first := newScratch(t, spec.SourceID)
	resultA, err := Publish(ctx, spec, Options{AdminDSN: first.adminDSN, ReaderDSN: first.readerDSN, OutputDir: filepath.Join(dir, "a")})
	if err != nil {
		t.Fatalf("publish A: %v", err)
	}
	second := newScratch(t, spec.SourceID)
	resultB, err := Publish(ctx, spec, Options{AdminDSN: second.adminDSN, ReaderDSN: second.readerDSN, OutputDir: filepath.Join(dir, "b")})
	if err != nil {
		t.Fatalf("publish B: %v", err)
	}
	if resultA.Manifest.ManifestDigest != resultB.Manifest.ManifestDigest {
		t.Fatalf("same input published to two databases gave different manifests:\n%+v\n%+v", resultA.Manifest, resultB.Manifest)
	}
	for _, kind := range RelationKinds {
		a, b := resultA.Manifest.Publications[kind], resultB.Manifest.Publications[kind]
		if a != b || a.ManifestDigest == "" || a.RowCount == 0 {
			t.Fatalf("%s publication differs or is empty: %+v vs %+v", kind, a, b)
		}
	}
	counts := resultA.Manifest.Counts
	if counts.Documents != 40 || counts.DocumentVersions < 40 || counts.Chunks < counts.DocumentVersions || counts.Values == 0 || counts.LeadershipChunks == 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if resultA.Manifest.Publications["chunks"].RowCount != uint64(counts.Chunks) || resultA.Manifest.Publications["documents"].RowCount != uint64(counts.DocumentVersions) {
		t.Fatalf("publication row counts do not match prepared counts: %+v", resultA.Manifest)
	}

	// The frozen views are readable by the gateway role, carry no embedding, and
	// the chunk vectors have the declared dimension.
	reader, err := sql.Open("pgx", first.readerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var rows int
	if err := reader.QueryRowContext(ctx, "SELECT count(*) FROM "+spec.Relation("chunks").SourceRelation).Scan(&rows); err != nil || rows != counts.Chunks {
		t.Fatalf("reader count on chunks view = %d (%v), want %d", rows, err, counts.Chunks)
	}
	var hasEmbedding bool
	if err := first.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'reporting' AND table_name = $1 AND column_name = 'embedding')`,
		strings.TrimPrefix(spec.Relation("chunks").SourceRelation, "reporting.")).Scan(&hasEmbedding); err != nil || hasEmbedding {
		t.Fatalf("chunks view exposes embedding (%v)", err)
	}
	var dimension int
	if err := first.db.QueryRowContext(ctx, `SELECT vector_dims(embedding) FROM kb.chunks LIMIT 1`).Scan(&dimension); err != nil || dimension != 64 {
		t.Fatalf("embedding dimension = %d (%v)", dimension, err)
	}
	var owner string
	if err := first.db.QueryRowContext(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, spec.Relation("documents").SourceRelation).Scan(&owner); err != nil || owner != snapshotOwnerRole {
		t.Fatalf("documents view owner = %s (%v)", owner, err)
	}

	verified, err := Verify(ctx, filepath.Join(dir, "a"), first.readerDSN)
	if err != nil {
		t.Fatalf("verify untouched publication: %v", err)
	}
	if verified.ManifestDigest != resultA.Manifest.ManifestDigest {
		t.Fatal("verify returned a different manifest")
	}
	// The generated fragment merges into the repository Catalog, validates, and
	// binds exactly this manifest: the end-to-end proof for K2.4.
	fragment, err := os.ReadFile(filepath.Join(dir, "a", "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := catalog.MergeYAMLFragment(base, fragment)
	if err != nil {
		t.Fatalf("merge fragment: %v", err)
	}
	knowledgeCatalog, err := catalog.Parse(merged)
	if err != nil {
		t.Fatalf("fragment does not validate against the repository Catalog: %v\n%s", err, fragment)
	}
	knowledgeProduct, ok := knowledgeCatalog.LookupKnowledgeProduct(spec.KnowledgeProduct)
	if !ok {
		t.Fatal("knowledge product missing after merge")
	}
	if err := VerifyCatalogBinding(knowledgeCatalog, knowledgeProduct, resultA.Manifest); err != nil {
		t.Fatalf("published manifest does not bind to the fragment it produced: %v", err)
	}
	artifactRoot := filepath.Join(dir, "artifacts")
	third := newScratch(t, spec.SourceID)
	resultC, err := Publish(ctx, spec, Options{AdminDSN: third.adminDSN, ReaderDSN: third.readerDSN,
		OutputDir: filepath.Dir(ManifestPath(artifactRoot, spec.Epoch)), BundleDir: artifactRoot})
	if err != nil {
		t.Fatalf("publish into artifact layout: %v", err)
	}
	if resultC.Manifest.ManifestDigest != resultA.Manifest.ManifestDigest {
		t.Fatal("artifact-layout publish changed the manifest digest")
	}
	for _, kind := range RelationKinds {
		name := spec.Relation(kind).PublicationName
		if _, err := os.Stat(filepath.Join(artifactRoot, name, name+".bundle.json")); err != nil {
			t.Fatalf("bundle %s not at the loader layout: %v", name, err)
		}
	}
	if manifests, err := ActivateFromDirectory(knowledgeCatalog, artifactRoot); err != nil || len(manifests) != 1 {
		t.Fatalf("activation from artifact layout: %v %v", manifests, err)
	}

	// Republishing the same collection is refused: an epoch is immutable.
	_, err = Publish(ctx, spec, Options{AdminDSN: first.adminDSN, ReaderDSN: first.readerDSN, OutputDir: filepath.Join(dir, "again")})
	if err == nil || !strings.Contains(err.Error(), "already published") {
		t.Fatalf("republish error = %v", err)
	}

	// Tampering with one chunk after publication is detected by verify even
	// though the materialized view is refreshed to match.
	if _, err := first.db.ExecContext(ctx, `UPDATE kb.chunks SET text = text || ' tampered' WHERE chunk_index = 0 AND doc_id = (SELECT min(doc_id) FROM kb.chunks)`); err != nil {
		t.Fatal(err)
	}
	// REFRESH runs as the view owner, which production never grants base-table
	// access; the adversary here is a superuser who can grant it.
	if _, err := first.db.ExecContext(ctx, `GRANT SELECT ON ALL TABLES IN SCHEMA kb TO taskgate_snapshot_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, "REFRESH MATERIALIZED VIEW "+spec.Relation("chunks").SourceRelation); err != nil {
		t.Fatal(err)
	}
	_, err = Verify(ctx, filepath.Join(dir, "a"), first.readerDSN)
	if err == nil || !strings.Contains(err.Error(), "chunks") {
		t.Fatalf("verify after tamper = %v, want a chunks digest failure", err)
	}
	// The documents publication is still intact, so a verify that stops at the
	// first failure must have failed on chunks specifically, not earlier.
	if !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "differs") {
		t.Fatalf("tamper error does not name a digest mismatch: %v", err)
	}
}

func TestPublishRejectsCorpusDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	corpusPath, _ := testCorpus(t, dir, 5)
	spec := testSpec(corpusPath, strings.Repeat("00", 32))
	corpus, _ := os.ReadFile(corpusPath)
	if _, err := Prepare(spec, corpus); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Prepare with wrong digest = %v", err)
	}
}
