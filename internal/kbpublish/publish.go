package kbpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	snapshotOwnerRole = "taskgate_snapshot_owner"
	readerRole        = "gateway_reader"
)

// PublicationRef is one compiled V4 bundle of the epoch.
type PublicationRef struct {
	Product          string `json:"product"`
	PublicationName  string `json:"publication_name"`
	SourceRelation   string `json:"source_relation"`
	OrdinalSidecar   string `json:"ordinal_sidecar"`
	SourceNamespace  string `json:"source_namespace"`
	RowCount         uint64 `json:"row_count"`
	ManifestDigest   string `json:"manifest_digest"`
	DictionaryDigest string `json:"dictionary_digest"`
	SidecarDigest    string `json:"sidecar_digest"`
}

type Counts struct {
	Documents        int `json:"documents"`
	DocumentVersions int `json:"document_versions"`
	Chunks           int `json:"chunks"`
	LeadershipChunks int `json:"leadership_chunks"`
	Values           int `json:"values"`
	Labels           int `json:"labels"`
}

// Manifest binds everything an index publication epoch commits to.
type Manifest struct {
	Version               string                    `json:"version"`
	KnowledgeProduct      string                    `json:"knowledge_product"`
	Collection            string                    `json:"collection"`
	Epoch                 string                    `json:"epoch"`
	SourceID              string                    `json:"source_id"`
	CatalogSource         string                    `json:"catalog_source"`
	SourceNamespace       string                    `json:"source_namespace"`
	SchemaDigest          string                    `json:"schema_digest"`
	CorpusSHA256          string                    `json:"corpus_sha256"`
	Segmentation          Segmentation              `json:"segmentation"`
	SegmentationDigest    string                    `json:"segmentation_digest"`
	Embedding             Embedding                 `json:"embedding"`
	EmbeddingDigest       string                    `json:"embedding_digest"`
	ExtractionRules       []ExtractionRule          `json:"extraction_rules"`
	ExtractionRulesDigest string                    `json:"extraction_rules_digest"`
	DocumentTypes         []string                  `json:"document_types"`
	Retrieval             Retrieval                 `json:"retrieval"`
	Collation             Collation                 `json:"collation"`
	Vocabulary            Vocabulary                `json:"vocabulary"`
	Counts                Counts                    `json:"counts"`
	Publications          map[string]PublicationRef `json:"publications"`
	ManifestDigest        string                    `json:"manifest_digest"`
}

// Collation is the database collation every published text field carries.
type Collation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Vocabulary lists the attribute values present in the published collection;
// the Catalog fragment turns them into scope and attribute allowed values.
type Vocabulary struct {
	Departments     []string `json:"departments"`
	Classifications []string `json:"classifications"`
	JobRoles        []string `json:"job_roles"`
	Labels          []string `json:"labels"`
}

// Digest recomputes the manifest digest over every field except the digest.
func (m Manifest) Digest() (string, error) {
	m.ManifestDigest = ""
	return canonicalDigest(m)
}

// Options connects the publisher to the business database.
type Options struct {
	// AdminDSN writes kb.* rows and creates the frozen reporting views.
	AdminDSN string
	// ReaderDSN is the non-administrative role the snapshot compiler scans
	// with (gateway_reader), exactly as cmd/snapshot-index does.
	ReaderDSN string
	// OutputDir receives the compiler inputs with their expected digests, the
	// Catalog fragment and the manifest.
	OutputDir string
	// BundleDir is the snapshot artifact root the bundles are compiled into
	// (<BundleDir>/<publication>/). Empty means <OutputDir>/bundles.
	BundleDir string
}

// Result reports what Publish produced.
type Result struct {
	Manifest     Manifest
	ManifestPath string
	InputPaths   map[string]string
}

// Publish freezes the collection described by spec. It refuses to publish into
// a collection that already exists: an epoch is immutable, a new build is a
// new epoch.
func Publish(ctx context.Context, spec Spec, options Options) (Result, error) {
	if err := spec.Validate(); err != nil {
		return Result{}, err
	}
	if options.AdminDSN == "" || options.ReaderDSN == "" || options.OutputDir == "" {
		return Result{}, errors.New("kb-publish: admin DSN, reader DSN and output directory are required")
	}
	corpus, err := os.ReadFile(spec.Corpus.Path)
	if err != nil {
		return Result{}, fmt.Errorf("read corpus: %w", err)
	}
	prepared, err := Prepare(spec, corpus)
	if err != nil {
		return Result{}, err
	}
	if err := load(ctx, spec, prepared, options.AdminDSN); err != nil {
		return Result{}, err
	}
	return compileAndBind(ctx, spec, prepared, options)
}

// Verify rescans and recompiles every publication of an existing output
// directory and checks the compiled digests and the manifest digest against
// what was published. Any changed chunk, value, label or document fails it.
func Verify(ctx context.Context, outputDir, readerDSN string) (Manifest, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(outputDir, "kb-publication.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("read published manifest: %w", err)
	}
	var published Manifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&published); err != nil {
		return Manifest{}, fmt.Errorf("decode published manifest: %w", err)
	}
	digest, err := published.Digest()
	if err != nil {
		return Manifest{}, err
	}
	if digest != published.ManifestDigest {
		return Manifest{}, errors.New("published manifest digest does not match its content")
	}
	verifyDir, err := os.MkdirTemp("", "kb-verify-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(verifyDir)
	for _, kind := range RelationKinds {
		reference, ok := published.Publications[kind]
		if !ok {
			return Manifest{}, fmt.Errorf("published manifest lacks the %s publication", kind)
		}
		file, err := os.Open(filepath.Join(outputDir, "inputs", kind+".json"))
		if err != nil {
			return Manifest{}, fmt.Errorf("read compiler input for %s: %w", kind, err)
		}
		input, err := snapshotbundle.DecodeCompilerInput(file)
		_ = file.Close()
		if err != nil {
			return Manifest{}, fmt.Errorf("decode compiler input for %s: %w", kind, err)
		}
		if input.ExpectedDigests.ManifestDigest != reference.ManifestDigest || input.ExpectedDigests.ManifestDigest == "" {
			return Manifest{}, fmt.Errorf("%s: compiler input expects manifest %s but the publication manifest says %s",
				kind, input.ExpectedDigests.ManifestDigest, reference.ManifestDigest)
		}
		scanned, err := snapshotbundle.ScanPostgresSnapshot(ctx, input, readerDSN)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", kind, err)
		}
		written, err := snapshotbundle.CompileOwnedToDirectory(&scanned, verifyDir, snapshotbundle.DefaultPublicationLimits())
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", kind, err)
		}
		if written.Manifest.ManifestDigest != reference.ManifestDigest ||
			written.Manifest.DictionaryManifest.DictionaryDigest != reference.DictionaryDigest ||
			written.Manifest.DictionaryManifest.SidecarDigest != reference.SidecarDigest ||
			written.Manifest.RowCount != reference.RowCount {
			return Manifest{}, fmt.Errorf("%s: recompiled publication differs from the published one", kind)
		}
	}
	return published, nil
}

func load(ctx context.Context, spec Spec, prepared Prepared, adminDSN string) error {
	config, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return errors.New("parse admin DSN")
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.ConnectTimeout = 10 * time.Second
	config.RuntimeParams["application_name"] = "taskgate-kb-publish"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kb.collections WHERE collection = $1)`, spec.Collection).Scan(&exists); err != nil {
		return fmt.Errorf("inspect kb.collections: %w", err)
	}
	if exists {
		return fmt.Errorf("collection %q is already published; an epoch is immutable, publish a new epoch into a new collection", spec.Collection)
	}
	var sourceID string
	if err := tx.QueryRow(ctx, `SELECT datasource_id FROM reporting.datasource_attestation WHERE singleton`).Scan(&sourceID); err != nil {
		return fmt.Errorf("read datasource attestation: %w", err)
	}
	if sourceID != spec.SourceID {
		return fmt.Errorf("database attests datasource %q, spec expects %q", sourceID, spec.SourceID)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kb.collections (collection, description) VALUES ($1, $2)`,
		spec.Collection, "knowledge product "+spec.KnowledgeProduct+" epoch "+spec.Epoch); err != nil {
		return fmt.Errorf("insert collection: %w", err)
	}
	batch := &pgx.Batch{}
	for _, label := range prepared.Labels {
		batch.Queue(`INSERT INTO kb.labels (collection, label) VALUES ($1, $2)`, spec.Collection, label)
	}
	for _, doc := range prepared.Documents {
		var validUntil any
		if doc.ValidUntil != "" {
			validUntil = doc.ValidUntil
		}
		batch.Queue(`INSERT INTO kb.documents (collection, doc_id, version, title, document_type, department, classification, job_role, labels, valid_from, valid_until, content_sha256, chunk_count, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			spec.Collection, doc.DocID, doc.Version, doc.Title, doc.DocumentType, doc.Department, doc.Classification, doc.JobRole,
			append([]string{}, doc.Labels...), doc.ValidFrom, validUntil, sha256Hex([]byte(doc.Body)), chunkCount(prepared.Chunks, doc), doc.Body)
	}
	for _, chunk := range prepared.Chunks {
		var validUntil any
		if chunk.ValidUntil != "" {
			validUntil = chunk.ValidUntil
		}
		batch.Queue(`INSERT INTO kb.chunks (collection, doc_id, version, chunk_index, text, text_sha256, embedding, department, classification, job_role, labels, valid_until)
			VALUES ($1, $2, $3, $4, $5, $6, $7::vector, $8, $9, $10, $11, $12)`,
			spec.Collection, chunk.DocID, chunk.Version, chunk.ChunkIndex, chunk.Text, chunk.TextSHA256, VectorLiteral(chunk.Embedding),
			chunk.Department, chunk.Classification, chunk.JobRole, append([]string{}, chunk.Labels...), validUntil)
	}
	for _, value := range prepared.Values {
		var valueText, valueNumeric, valueDate any
		switch value.ValueType {
		case "text":
			valueText = value.Raw
		case "date":
			valueDate = value.Raw
		default:
			valueNumeric = value.Raw
		}
		batch.Queue(`INSERT INTO kb.extracted_values (collection, doc_id, version, document_type, field, value_type, canonical_value, value_text, value_numeric, value_date, department, classification)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::date, $11, $12)`,
			spec.Collection, value.DocID, value.Version, value.DocumentType, value.Field, value.ValueType, value.CanonicalValue,
			valueText, valueNumeric, valueDate, value.Department, value.Classification)
	}
	results := tx.SendBatch(ctx, batch)
	for index := 0; index < batch.Len(); index++ {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("load kb rows (statement %d): %w", index, err)
		}
	}
	if err := results.Close(); err != nil {
		return err
	}
	for _, kind := range RelationKinds {
		names := spec.Relation(kind)
		schema, relation, _ := strings.Cut(names.SourceRelation, ".")
		relationSQL := pgx.Identifier{schema, relation}.Sanitize()
		if _, err := tx.Exec(ctx, "CREATE MATERIALIZED VIEW "+relationSQL+" AS "+relationSelect(kind, spec.Collection)); err != nil {
			return fmt.Errorf("create %s: %w", names.SourceRelation, err)
		}
		for _, statement := range []string{
			"ALTER MATERIALIZED VIEW " + relationSQL + " OWNER TO " + snapshotOwnerRole,
			"GRANT SELECT ON " + relationSQL + " TO " + readerRole,
			"COMMENT ON MATERIALIZED VIEW " + relationSQL + " IS 'Immutable TaskGate knowledge-product publication " + names.PublicationName + "; a new build is a new epoch.'",
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("%s: %w", statement, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func chunkCount(chunks []Chunk, doc kbcorpus.Document) int {
	count := 0
	for _, chunk := range chunks {
		if chunk.DocID == doc.DocID && chunk.Version == doc.Version {
			count++
		}
	}
	return count
}

func relationSelect(kind, collection string) string {
	literal := "'" + strings.ReplaceAll(collection, "'", "''") + "'"
	switch kind {
	case "documents":
		return `SELECT doc_id, version, title, document_type, department AS kb_department, classification, job_role,
			array_to_string(labels, ',') AS labels, valid_from, valid_until, content_sha256, chunk_count
			FROM kb.documents WHERE collection = ` + literal + ` ORDER BY doc_id, version`
	case "chunks":
		return `SELECT doc_id, version, chunk_index, text, text_sha256, department AS kb_department, classification, job_role,
			array_to_string(labels, ',') AS labels, valid_until
			FROM kb.chunks WHERE collection = ` + literal + ` ORDER BY doc_id, version, chunk_index`
	default:
		return `SELECT doc_id, version, document_type, field, value_type, canonical_value, value_text, value_numeric, value_date, department AS kb_department, classification
			FROM kb.extracted_values WHERE collection = ` + literal + ` ORDER BY doc_id, version, field`
	}
}

// relationFields lists each published relation's fields in view order with
// their canonical SQL types; text fields receive the database collation.
var relationFields = map[string][][2]string{
	"documents": {{"doc_id", "text"}, {"version", "integer"}, {"title", "text"}, {"document_type", "text"}, {"kb_department", "text"},
		{"classification", "text"}, {"job_role", "text"}, {"labels", "text"}, {"valid_from", "date"}, {"valid_until", "date"},
		{"content_sha256", "text"}, {"chunk_count", "integer"}},
	"chunks": {{"doc_id", "text"}, {"version", "integer"}, {"chunk_index", "integer"}, {"text", "text"}, {"text_sha256", "text"},
		{"kb_department", "text"}, {"classification", "text"}, {"job_role", "text"}, {"labels", "text"}, {"valid_until", "date"}},
	"values": {{"doc_id", "text"}, {"version", "integer"}, {"document_type", "text"}, {"field", "text"}, {"value_type", "text"},
		{"canonical_value", "text"}, {"value_text", "text"}, {"value_numeric", "numeric"}, {"value_date", "date"},
		{"kb_department", "text"}, {"classification", "text"}},
}

var relationKeys = map[string][]string{
	"documents": {"doc_id", "version"},
	"chunks":    {"doc_id", "version", "chunk_index"},
	"values":    {"doc_id", "version", "field"},
}

func databaseCollation(ctx context.Context, readerDSN string) (string, string, error) {
	config, err := pgx.ParseConfig(readerDSN)
	if err != nil {
		return "", "", errors.New("parse reader DSN")
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return "", "", fmt.Errorf("connect reader: %w", err)
	}
	defer conn.Close(context.Background())
	var name, version string
	if err := conn.QueryRow(ctx, `SELECT datcollate, COALESCE(datcollversion, '') FROM pg_catalog.pg_database WHERE datname = current_database()`).Scan(&name, &version); err != nil {
		return "", "", fmt.Errorf("read database collation: %w", err)
	}
	if name == "" || version == "" {
		return "", "", errors.New("database collation name or version is empty; the compiler requires both for text fields")
	}
	return name, version, nil
}

func compilerInput(spec Spec, kind, collation, collationVersion string) snapshotbundle.CompilerInput {
	names := spec.Relation(kind)
	var fields []snapshotbundle.SnapshotField
	for _, field := range relationFields[kind] {
		physical := snapshotbundle.SnapshotField{Name: field[0], SQLType: field[1]}
		if field[1] == "text" {
			physical.Collation, physical.CollationVersion = collation, collationVersion
		}
		canonical := physical
		canonical.CanonicalFieldID = names.Product + "." + field[0]
		fields = append(fields, physical, canonical)
	}
	return snapshotbundle.CompilerInput{
		Version: "taskgate-snapshot-index-input-v1", PublicationName: names.PublicationName,
		CatalogSource: spec.CatalogSource, SourceRelation: names.SourceRelation, OrdinalSidecar: names.OrdinalSidecar,
		EntityKeyFields: append([]string(nil), relationKeys[kind]...),
		Snapshot: snapshotbundle.SnapshotInput{
			SourceID: spec.SourceID, SourceNamespace: names.SourceNamespace, Snapshot: spec.Epoch,
			SchemaDigest: spec.SchemaDigest, Fields: fields,
		},
	}
}

func compileAndBind(ctx context.Context, spec Spec, prepared Prepared, options Options) (Result, error) {
	collation, collationVersion, err := databaseCollation(ctx, options.ReaderDSN)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Join(options.OutputDir, "inputs"), 0o755); err != nil {
		return Result{}, err
	}
	manifest := Manifest{
		Version: ManifestVersion, KnowledgeProduct: spec.KnowledgeProduct, Collection: spec.Collection, Epoch: spec.Epoch,
		SourceID: spec.SourceID, CatalogSource: spec.CatalogSource, SourceNamespace: spec.SourceNamespace,
		SchemaDigest: spec.SchemaDigest, CorpusSHA256: spec.Corpus.SHA256,
		Segmentation: spec.Segmentation, Embedding: spec.Embedding, ExtractionRules: append([]ExtractionRule{}, spec.ExtractionRules...),
		Publications: map[string]PublicationRef{},
	}
	manifest.SegmentationDigest, manifest.EmbeddingDigest, manifest.ExtractionRulesDigest, err = DeclarationDigests(spec)
	if err != nil {
		return Result{}, err
	}
	manifest.DocumentTypes = append([]string(nil), spec.DocumentTypes...)
	manifest.Retrieval = Retrieval{KMax: spec.Retrieval.KMax, Aggregates: append([]string{}, spec.Retrieval.Aggregates...)}
	manifest.Collation = Collation{Name: collation, Version: collationVersion}
	manifest.Vocabulary = vocabulary(prepared)
	seen := map[string]struct{}{}
	for _, doc := range prepared.Documents {
		seen[doc.DocID] = struct{}{}
	}
	manifest.Counts = Counts{Documents: len(seen), DocumentVersions: len(prepared.Documents), Chunks: len(prepared.Chunks),
		Values: len(prepared.Values), Labels: len(prepared.Labels)}
	for _, chunk := range prepared.Chunks {
		if chunk.Classification == "leadership" {
			manifest.Counts.LeadershipChunks++
		}
	}
	inputPaths := map[string]string{}
	for _, kind := range RelationKinds {
		if kind == "values" && len(prepared.Values) == 0 {
			return Result{}, errors.New("no extracted values: declare at least one extraction rule that matches the corpus")
		}
		input := compilerInput(spec, kind, collation, collationVersion)
		scanned, err := snapshotbundle.ScanPostgresSnapshot(ctx, input, options.ReaderDSN)
		if err != nil {
			return Result{}, fmt.Errorf("scan %s: %w", kind, err)
		}
		// One artifact root for the epoch: the installer and the gateway loader
		// expect <root>/<publication_name>/<publication_name>.bundle.json.
		bundleDir := options.BundleDir
		if bundleDir == "" {
			bundleDir = filepath.Join(options.OutputDir, "bundles")
		}
		written, err := snapshotbundle.CompileOwnedToDirectory(&scanned, bundleDir, snapshotbundle.DefaultPublicationLimits())
		if err != nil {
			return Result{}, fmt.Errorf("compile %s: %w", kind, err)
		}
		dictionary := written.Manifest.DictionaryManifest
		input.ExpectedDigests = snapshotbundle.ExpectedDigests{
			SidecarDigest: dictionary.SidecarDigest, DictionaryDigest: dictionary.DictionaryDigest,
			ManifestDigest: written.Manifest.ManifestDigest, ColdPayloadDigest: dictionary.ColdPayloadDigest,
			HotIndexDigest: dictionary.HotIndexDigest,
		}
		names := spec.Relation(kind)
		manifest.Publications[kind] = PublicationRef{
			Product: names.Product, PublicationName: names.PublicationName, SourceRelation: names.SourceRelation,
			OrdinalSidecar: names.OrdinalSidecar, SourceNamespace: names.SourceNamespace, RowCount: written.Manifest.RowCount,
			ManifestDigest: written.Manifest.ManifestDigest, DictionaryDigest: dictionary.DictionaryDigest, SidecarDigest: dictionary.SidecarDigest,
		}
		encoded, err := json.MarshalIndent(input, "", "  ")
		if err != nil {
			return Result{}, err
		}
		path := filepath.Join(options.OutputDir, "inputs", kind+".json")
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			return Result{}, err
		}
		inputPaths[kind] = path
	}
	if manifest.ManifestDigest, err = manifest.Digest(); err != nil {
		return Result{}, err
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return Result{}, err
	}
	manifestPath := filepath.Join(options.OutputDir, "kb-publication.json")
	if err := os.WriteFile(manifestPath, encoded, 0o644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(options.OutputDir, "catalog-fragment.yaml"), []byte(CatalogFragment(spec, manifest)), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Manifest: manifest, ManifestPath: manifestPath, InputPaths: inputPaths}, nil
}

func encodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// DeclarationDigests computes the segmentation, embedding and extraction-rule
// digests with the Catalog's own canonical function over the Catalog's own
// declaration types, so kb_publications validation and this manifest agree.
func DeclarationDigests(spec Spec) (string, string, string, error) {
	segmentation, err := catalog.CanonicalDeclarationDigest(catalog.KBSegmentation{Function: spec.Segmentation.Function, WindowChars: spec.Segmentation.WindowChars})
	if err != nil {
		return "", "", "", err
	}
	embedding, err := catalog.CanonicalDeclarationDigest(catalog.KBEmbedding{Model: spec.Embedding.Model, Dimension: spec.Embedding.Dimension})
	if err != nil {
		return "", "", "", err
	}
	rules := make([]catalog.KBExtractionRule, 0, len(spec.ExtractionRules))
	for _, rule := range spec.ExtractionRules {
		rules = append(rules, catalog.KBExtractionRule{DocumentType: rule.DocumentType, Field: rule.Field, Type: rule.Type, Source: rule.Source, Scope: rule.Scope})
	}
	extraction, err := catalog.CanonicalDeclarationDigest(rules)
	if err != nil {
		return "", "", "", err
	}
	return segmentation, embedding, extraction, nil
}

func vocabulary(prepared Prepared) Vocabulary {
	departments, classifications, roles := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for _, doc := range prepared.Documents {
		departments[doc.Department] = struct{}{}
		classifications[doc.Classification] = struct{}{}
		roles[doc.JobRole] = struct{}{}
	}
	for _, chunk := range prepared.Chunks {
		classifications[chunk.Classification] = struct{}{}
	}
	return Vocabulary{Departments: sortedKeys(departments), Classifications: sortedKeys(classifications),
		JobRoles: sortedKeys(roles), Labels: append([]string{}, prepared.Labels...)}
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func yamlList(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// CatalogFragment renders the Catalog declarations an operator merges into the
// candidate Catalog with catalog.MergeYAMLFragment: two enum scopes, three
// snapshot publications, three products, the knowledge_products block and the
// kb_publications binding. The result validates under internal/catalog.
func CatalogFragment(spec Spec, manifest Manifest) string {
	var out strings.Builder
	kinds := append([]string(nil), RelationKinds...)
	sort.Strings(kinds)
	out.WriteString("# Generated by cmd/kb-publish for " + spec.KnowledgeProduct + " epoch " + spec.Epoch + "\n")
	fmt.Fprintf(&out, "scopes:\n  - name: kb_department\n    type: enum\n    description: knowledge product department scope\n    allowed_values: %s\n", yamlList(manifest.Vocabulary.Departments))
	fmt.Fprintf(&out, "  - name: classification\n    type: enum\n    description: knowledge product classification scope\n    allowed_values: %s\n", yamlList(manifest.Vocabulary.Classifications))
	out.WriteString("snapshot_publications:\n")
	for _, kind := range kinds {
		reference := manifest.Publications[kind]
		fmt.Fprintf(&out, "  - name: %s\n    source: %s\n    source_namespace: %s\n    snapshot: %s\n    ordinal_sidecar: %s\n    sidecar_digest: %s\n    dictionary_digest: %s\n    manifest_digest: %s\n",
			reference.PublicationName, spec.CatalogSource, reference.SourceNamespace, spec.Epoch, reference.OrdinalSidecar,
			reference.SidecarDigest, reference.DictionaryDigest, reference.ManifestDigest)
	}
	out.WriteString("products:\n")
	for _, kind := range kinds {
		reference := manifest.Publications[kind]
		fmt.Fprintf(&out, "  - name: %s\n    source: %s\n    reporting_view: %s\n    description: knowledge product %s %s\n    sensitivity: %s\n    snapshot: %s\n    snapshot_publication: %s\n    entity_key: [%s]\n    fact_namespace: %s\n    stable_relation_role: %s\n    scopes: [kb_department, classification]\n",
			reference.Product, spec.CatalogSource, reference.SourceRelation, spec.KnowledgeProduct, kind, spec.Sensitivity, spec.Epoch,
			reference.PublicationName, strings.Join(relationKeys[kind], ", "), reference.SourceNamespace, reference.Product)
		if kind == "values" && len(spec.Retrieval.Aggregates) > 0 {
			fmt.Fprintf(&out, "    allowed_aggregates: [%s]\n", strings.Join(spec.Retrieval.Aggregates, ", "))
		}
		out.WriteString("    fields:\n")
		for _, field := range relationFields[kind] {
			if field[1] == "text" {
				fmt.Fprintf(&out, "      - {name: %s, type: text, collation: %s, collation_version: \"%s\", description: %s}\n", field[0], manifest.Collation.Name, manifest.Collation.Version, field[0])
			} else {
				fmt.Fprintf(&out, "      - {name: %s, type: %s, description: %s}\n", field[0], field[1], field[0])
			}
		}
	}
	fmt.Fprintf(&out, "knowledge_products:\n  - name: %s\n    description: %s\n    sensitivity: %s\n    collection: %s\n    source_namespace: %s\n    products:\n", spec.KnowledgeProduct, spec.Description, spec.Sensitivity, spec.Collection, spec.SourceNamespace)
	for _, kind := range kinds {
		fmt.Fprintf(&out, "      %s: %s\n", kind, manifest.Publications[kind].Product)
	}
	fmt.Fprintf(&out, "    index_publication: %s\n    document_types: %s\n    attributes:\n", spec.Epoch, yamlList(spec.DocumentTypes))
	out.WriteString("      - {name: kb_department, type: enum, scope: kb_department}\n      - {name: classification, type: enum, scope: classification}\n")
	fmt.Fprintf(&out, "      - {name: job_role, type: enum, allowed_values: %s}\n      - {name: labels, type: text_set}\n      - {name: valid_until, type: date}\n", yamlList(manifest.Vocabulary.JobRoles))
	fmt.Fprintf(&out, "    segmentation: {function: %s, window_chars: %d}\n    embedding: {model: %s, dimension: %d}\n", spec.Segmentation.Function, spec.Segmentation.WindowChars, spec.Embedding.Model, spec.Embedding.Dimension)
	if len(spec.ExtractionRules) > 0 {
		out.WriteString("    extraction_rules:\n")
		for _, rule := range spec.ExtractionRules {
			fmt.Fprintf(&out, "      - {document_type: %s, field: %s, type: %s, source: %s, scope: %s}\n", rule.DocumentType, rule.Field, rule.Type, rule.Source, rule.Scope)
		}
	}
	fmt.Fprintf(&out, "    retrieval: {k_max: %d, exact: true, aggregates: [%s], generation_admitted: false}\n    scopes: [kb_department, classification]\n", spec.Retrieval.KMax, strings.Join(spec.Retrieval.Aggregates, ", "))
	out.WriteString("    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n        classification:\n          from_attribute: clearance\n")
	fmt.Fprintf(&out, "kb_publications:\n  - epoch: %s\n    knowledge_product: %s\n    manifest_digest: %s\n    segmentation_digest: %s\n    embedding_digest: %s\n    extraction_rules_digest: %s\n",
		spec.Epoch, spec.KnowledgeProduct, manifest.ManifestDigest, manifest.SegmentationDigest, manifest.EmbeddingDigest, manifest.ExtractionRulesDigest)
	return out.String()
}
