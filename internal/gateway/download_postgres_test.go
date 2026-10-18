package gateway

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbdownload"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

// The download end-to-end test runs download_document against a real published
// knowledge product, with the same environment as the search end-to-end test
// (KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH,
// CONTROL_TEST_POSTGRES_DSN). It is the acceptance path for docs/kb-design.md
// §12: the delivered file equals an assembly made directly from the database,
// the chunk part releases four cells per chunk, a replay is free, a document
// with a section outside the task's scope is refused whole without releasing a
// chunk, and only approved documents are downloadable.
func TestDownloadEndToEndAgainstKnowledgePublication(t *testing.T) {
	businessDSN := os.Getenv("KB_E2E_BUSINESS_DSN")
	artifactDir := os.Getenv("KB_E2E_ARTIFACT_DIR")
	epoch := os.Getenv("KB_E2E_EPOCH")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" {
		t.Skip("KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH and CONTROL_TEST_POSTGRES_DSN are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harness := knowledgeE2EHarness(t, ctx, businessDSN, artifactDir, epoch)
	product := harness.catalog.KnowledgeProducts[0]
	db, err := pgx.Connect(ctx, businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	pick := func(label, condition string) kbdownload.Ref {
		t.Helper()
		var ref kbdownload.Ref
		if err := db.QueryRow(ctx, `SELECT d.doc_id, d.version FROM kb.documents d
			WHERE d.collection = $1 AND d.department = 'finance' AND d.classification = 'internal' AND d.chunk_count >= 2
			  AND EXISTS (SELECT 1 FROM kb.extracted_values v WHERE v.collection = d.collection AND v.doc_id = d.doc_id AND v.version = d.version)
			  AND `+condition+`
			ORDER BY md5($2 || d.doc_id || d.version) LIMIT 1`, product.Collection, label).Scan(&ref.DocID, &ref.Version); err != nil {
			t.Fatalf("pick %s document: %v", label, err)
		}
		return ref
	}
	whole := `NOT EXISTS (SELECT 1 FROM kb.chunks c WHERE c.collection = d.collection AND c.doc_id = d.doc_id AND c.version = d.version AND c.classification <> 'internal')`
	full := pick("kb-download-full:", whole)
	unlisted := pick("kb-download-unlisted:", whole+` AND d.doc_id <> '`+full.DocID+`'`)
	mixed := pick("kb-download-mixed:", `EXISTS (SELECT 1 FROM kb.chunks c WHERE c.collection = d.collection AND c.doc_id = d.doc_id AND c.version = d.version AND c.classification = 'leadership')`)
	t.Logf("documents: full %s, mixed %s, unlisted %s", full, mixed, unlisted)

	reference := func(ref kbdownload.Ref, values bool) []byte {
		t.Helper()
		file := kbdownload.File{Ref: ref}
		if err := db.QueryRow(ctx, `SELECT title FROM kb.documents WHERE collection = $1 AND doc_id = $2 AND version = $3`,
			product.Collection, ref.DocID, ref.Version).Scan(&file.Title); err != nil {
			t.Fatal(err)
		}
		rows, err := db.Query(ctx, `SELECT chunk_index, text_sha256, text FROM kb.chunks WHERE collection = $1 AND doc_id = $2 AND version = $3`,
			product.Collection, ref.DocID, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var chunk kbdownload.Chunk
			if err := rows.Scan(&chunk.Index, &chunk.TextSHA256, &chunk.Text); err != nil {
				t.Fatal(err)
			}
			file.Chunks = append(file.Chunks, chunk)
		}
		rows.Close()
		if values {
			file.Values = []kbdownload.Value{}
			rows, err := db.Query(ctx, `SELECT field, value_type, canonical_value FROM kb.extracted_values WHERE collection = $1 AND doc_id = $2 AND version = $3`,
				product.Collection, ref.DocID, ref.Version)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var value kbdownload.Value
				if err := rows.Scan(&value.Field, &value.ValueType, &value.CanonicalValue); err != nil {
					t.Fatal(err)
				}
				file.Values = append(file.Values, value)
			}
			rows.Close()
		}
		content, err := kbdownload.Assemble(file)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}

	chunks, documents, values := product.Products.Chunks, product.Products.Documents, product.Products.Values
	columns := map[string][]string{
		chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification"},
		documents: {"doc_id", "version", "title", "chunk_count", "kb_department", "classification"},
		values:    {"doc_id", "version", "field", "value_type", "canonical_value", "kb_department", "classification"},
	}
	limits := control.ExposureLimits{ReleaseFacts: 100000, InfluenceFacts: 1000000, OutcomeFacts: 1000}
	createDownloadTask := func(taskID string, products []string, classification []any, listed ...kbdownload.Ref) {
		list := make([]any, 0, len(listed))
		for _, ref := range listed {
			list = append(list, ref.String())
		}
		taskColumns := map[string][]string{}
		for _, name := range products {
			taskColumns[name] = columns[name]
		}
		harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil, limits, exposure.ProfileV5, products, taskColumns,
			domain.SensitivityHigh, map[string]any{"kb_department": []any{"finance"}, "classification": classification, kbdownload.ScopeKey: list})
	}
	downloadAs := func(format, taskID, requestID string, ref kbdownload.Ref) (mcp.ToolResult, error) {
		return harness.service.CallTool(ctx, harness.alice, "download_document", mustJSON(t, map[string]any{"task_id": taskID,
			"request_id": requestID, "knowledge_product": product.Name, "doc_id": ref.DocID, "version": ref.Version, "format": format}))
	}
	download := func(taskID, requestID string, ref kbdownload.Ref) (mcp.ToolResult, error) {
		return downloadAs(kbdownload.FormatAssembled, taskID, requestID, ref)
	}
	released := func(taskID string) int64 {
		t.Helper()
		ledger, err := harness.store.GetExposureLedger(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return ledger.Used.ReleaseFacts
	}

	createDownloadTask("task-kb-download", []string{chunks, documents, values}, []any{"internal", "public"}, full, mixed)
	result, err := download("task-kb-download", "dl-full-1", full)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	structured := result.Structured.(map[string]any)
	want := reference(full, true)
	if structured["content"] != string(want) || structured["file_sha256"] != kbdownload.SHA256(want) {
		t.Fatalf("delivered file differs from the direct assembly:\n%s\nwant:\n%s", structured["content"], want)
	}
	if len(result.Resources) != 1 || result.Resources[0].Text != string(want) || result.Resources[0].MimeType != kbdownload.FileMediaType {
		t.Fatalf("embedded resource %+v", result.Resources)
	}
	if !strings.Contains(string(want), "## 字段") {
		t.Fatalf("the reference file of a document with extracted values has no values block:\n%s", want)
	}
	parts := structured["queries"].([]map[string]any)
	if len(parts) != 3 {
		t.Fatalf("parts %v", parts)
	}
	var chunkCount int64
	for _, part := range parts {
		charge, _ := part["exposure"].(control.ExposureCharge)
		t.Logf("part %s: rows %v release %d influence %d outcome %d atoms %d", part["part"], part["row_count"],
			charge.ActualReleaseFacts, charge.ActualInfluenceFacts, charge.ActualOutcomeFacts, charge.ActualPredicateAtomCount)
		if part["part"] == "chunks" {
			chunkCount = int64(part["row_count"].(int))
			if charge.ActualReleaseFacts != 4*chunkCount || charge.ChargedReleaseFacts != charge.ActualReleaseFacts {
				t.Fatalf("chunk part release %+v, want 4 cells per chunk on a fresh ledger", charge)
			}
		}
	}
	if chunkCount < 2 {
		t.Fatalf("chunk part returned %d chunks", chunkCount)
	}
	afterFirst := released("task-kb-download")

	// Idempotent replay and semantic replay under a new request id are free.
	for _, requestID := range []string{"dl-full-1", "dl-full-2"} {
		again, err := download("task-kb-download", requestID, full)
		if err != nil {
			t.Fatalf("download %s: %v", requestID, err)
		}
		if again.Structured.(map[string]any)["content"] != string(want) {
			t.Fatalf("%s delivered a different file", requestID)
		}
		if got := released("task-kb-download"); got != afterFirst {
			t.Fatalf("%s charged release %d -> %d", requestID, afterFirst, got)
		}
	}

	// A document with a leadership section is refused whole: its document row
	// is charged, no chunk is released, and a retry repeats the refusal.
	_, err = download("task-kb-download", "dl-mixed-1", mixed)
	requireToolCode(t, err, apierr.CodeDocumentNotDownloadable)
	afterMixed := released("task-kb-download")
	if afterMixed-afterFirst != 2 {
		t.Fatalf("refused document released %d facts, want only the 2 document cells (title, chunk_count)", afterMixed-afterFirst)
	}
	_, err = download("task-kb-download", "dl-mixed-1", mixed)
	requireToolCode(t, err, apierr.CodeDocumentNotDownloadable)
	if got := released("task-kb-download"); got != afterMixed {
		t.Fatalf("a repeated refusal charged release %d -> %d", afterMixed, got)
	}
	_, err = download("task-kb-download", "dl-unlisted-1", unlisted)
	requireToolCode(t, err, apierr.CodeDownloadNotApproved)

	// The original file is refused without the files relation in the grant,
	// even with the values relation approved, and nothing is charged.
	_, err = downloadAs(kbdownload.FormatOriginal, "task-kb-download", "dl-original-unapproved", full)
	requireToolCode(t, err, apierr.CodeDownloadNotApproved)
	if got := released("task-kb-download"); got != afterMixed {
		t.Fatalf("a refused original download charged release %d -> %d", afterMixed, got)
	}
	originalsDir := os.Getenv("KB_E2E_ORIGINALS_DIR")
	if product.Products.Files == "" || originalsDir == "" {
		t.Logf("original-file channel not exercised: files relation %q, KB_E2E_ORIGINALS_DIR %q", product.Products.Files, originalsDir)
	} else {
		// With the files relation approved the original is the stored object
		// byte for byte, its size and digest are the published files row, and
		// the charge covers every chunk and value cell besides the document,
		// files and values cells.
		harness.service.kbOriginals = directoryOriginals(originalsDir)
		files := product.Products.Files
		filesColumns := []string{"doc_id", "version", "media_type", "byte_size", "file_sha256", "kb_department", "classification"}
		harness.createTaskWithGrantScopeAndExposureProfile(t, "task-kb-original", nil, limits, exposure.ProfileV5,
			[]string{chunks, documents, values, files}, map[string][]string{chunks: columns[chunks], documents: columns[documents], values: columns[values], files: filesColumns},
			domain.SensitivityHigh, map[string]any{"kb_department": []any{"finance"}, "classification": []any{"internal", "public"},
				kbdownload.ScopeKey: []any{full.String(), mixed.String()}})
		var objectKey, mediaType, fileSHA256 string
		var byteSize int64
		if err := db.QueryRow(ctx, `SELECT object_key, media_type, byte_size, file_sha256 FROM kb.document_files WHERE collection = $1 AND doc_id = $2 AND version = $3`,
			product.Collection, full.DocID, full.Version).Scan(&objectKey, &mediaType, &byteSize, &fileSHA256); err != nil {
			t.Fatal(err)
		}
		object, err := os.ReadFile(filepath.Join(originalsDir, filepath.FromSlash(objectKey)))
		if err != nil {
			t.Fatal(err)
		}
		result, err := downloadAs(kbdownload.FormatOriginal, "task-kb-original", "dl-original-1", full)
		if err != nil {
			t.Fatalf("original download: %v", err)
		}
		structured := result.Structured.(map[string]any)
		if structured["file_sha256"] != fileSHA256 || kbdownload.SHA256(object) != fileSHA256 || structured["media_type"] != mediaType ||
			structured["file_bytes"] != int(byteSize) || structured["content"] != nil {
			t.Fatalf("original delivery %v differs from the published files row (%s, %d bytes, %s)", structured, mediaType, byteSize, fileSHA256)
		}
		if len(result.Resources) != 1 || string(result.Resources[0].Blob) != string(object) || result.Resources[0].MimeType != mediaType {
			t.Fatal("the embedded resource is not the original object")
		}
		var originalRelease int64
		for _, part := range structured["queries"].([]map[string]any) {
			charge, _ := part["exposure"].(control.ExposureCharge)
			originalRelease += charge.ActualReleaseFacts
			if part["part"] == "files" && charge.ActualReleaseFacts != 3 {
				t.Fatalf("files part released %d, want media_type, byte_size and file_sha256", charge.ActualReleaseFacts)
			}
		}
		if originalRelease < 2+4*chunkCount+3 {
			t.Fatalf("original download released %d facts, below the document and files cells plus every chunk cell (%d)", originalRelease, 2+4*chunkCount+3)
		}
		// A section outside the scope refuses the original exactly as the assembled file.
		_, err = downloadAs(kbdownload.FormatOriginal, "task-kb-original", "dl-original-mixed", mixed)
		requireToolCode(t, err, apierr.CodeDocumentNotDownloadable)
	}

	// The same mixed document is downloadable for a task cleared for its
	// leadership section; without the values relation the file has no values.
	createDownloadTask("task-kb-download-leadership", []string{chunks, documents}, []any{"internal", "leadership", "public"}, mixed)
	result, err = download("task-kb-download-leadership", "dl-mixed-cleared", mixed)
	if err != nil {
		t.Fatalf("cleared download: %v", err)
	}
	structured = result.Structured.(map[string]any)
	if structured["content"] != string(reference(mixed, false)) || structured["values_included"] != false {
		t.Fatalf("cleared download of %s differs from the direct assembly without values:\n%s", mixed, structured["content"])
	}
}

// knowledgeE2EHarness is the search end-to-end setup: a KB-only Catalog
// attested against the live business database, a real connector, and the
// publication's HOT dictionaries registered as cmd/gateway loads them.
func knowledgeE2EHarness(t *testing.T, ctx context.Context, businessDSN, artifactDir, epoch string) *gatewayHarness {
	t.Helper()
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	demo, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := catalog.Parse([]byte(kbOnlyCatalog(t, string(demo), string(fragment), "", "")))
	if err != nil {
		t.Fatalf("parse KB-only catalog: %v", err)
	}
	built, err := catalogschema.Build(probe)
	if err != nil {
		t.Fatal(err)
	}
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 10 * time.Second,
		ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
	if err != nil {
		t.Fatalf("probe connector: %v", err)
	}
	attestation, err := probeConnector.Attestation(ctx)
	probeConnector.Close()
	if err != nil {
		t.Fatalf("probe attestation: %v", err)
	}
	parsedDSN, err := url.Parse(businessDSN)
	if err != nil || strings.Trim(parsedDSN.Path, "/") == "" {
		t.Fatalf("KB_E2E_BUSINESS_DSN must be a URL naming the database: %v", err)
	}
	database := strings.Trim(parsedDSN.Path, "/")
	harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
		return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
	}})
	if built, err = catalogschema.Build(harness.catalog); err != nil {
		t.Fatal(err)
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2,
		ExpectedSchema: built.Entries, ExpectedSchemaDigest: built.Source.SchemaDigest,
		ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: built.Source.DatasourceID, Database: built.Source.Database,
			User: built.Source.User, PostgreSQLMajorVersion: built.Source.PostgreSQLMajorVersion},
	})
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	t.Cleanup(connector.Close)
	if attestation, err = connector.Attestation(ctx); err != nil {
		t.Fatal(err)
	}
	harness.connector.attestation = attestation
	harness.service.connector = connector
	installKnowledgeRegistry(t, harness, artifactDir)
	return harness
}

// directoryOriginals is a test originals store: object keys are paths under a
// directory, as an object store's keys are paths under a bucket.
type directoryOriginals string

func (root directoryOriginals) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return nil, os.ErrNotExist
	}
	return os.Open(filepath.Join(string(root), filepath.FromSlash(key)))
}
