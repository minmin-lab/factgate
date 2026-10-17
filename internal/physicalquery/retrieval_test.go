package physicalquery

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/retrieval"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const (
	testKBChunks    = "policy_kb_chunks"
	testKBDocuments = "policy_kb_documents"
	testKBSnapshot  = "policy-kb-test"
)

func kbTestField(name, sqlType string) catalog.Field {
	field := catalog.Field{Name: name, Type: sqlType, Description: name}
	if sqlType == "text" {
		field.Collation, field.CollationVersion = "en_US.utf8", "2.36"
	}
	return field
}

func kbTestProduct(name, namespace string, entityKey []string, fields []catalog.Field) catalog.Product {
	return catalog.Product{
		Name: name, Source: "travel_demo", ReportingView: "reporting.kb_" + name,
		Snapshot: testKBSnapshot, SnapshotPublication: testKBSnapshot + "-" + name,
		EntityKey: entityKey, FactNamespace: namespace, StableRelationRole: name,
		Scopes: []string{"kb_department", "classification"}, Fields: fields,
	}
}

func kbTestPublication(name, namespace string) catalog.SnapshotPublication {
	return catalog.SnapshotPublication{
		Name: testKBSnapshot + "-" + name, Source: "travel_demo", SourceNamespace: namespace,
		Snapshot:         testKBSnapshot,
		ManifestDigest:   strings.Repeat("1", 64),
		DictionaryDigest: strings.Repeat("2", 64),
		SidecarDigest:    strings.Repeat("3", 64),
		OrdinalSidecar:   "taskgate_ordinal.kb_" + name,
	}
}

func kbTestBinding(publication catalog.SnapshotPublication) SnapshotBinding {
	return SnapshotBinding{
		PublicationName: publication.Name, DictionaryDigest: publication.DictionaryDigest,
		ManifestDigest: publication.ManifestDigest, SidecarDigest: publication.SidecarDigest,
		SourceNamespace: publication.SourceNamespace, Snapshot: publication.Snapshot,
		OrdinalSidecar: publication.OrdinalSidecar, RowCount: 48183,
	}
}

func kbTestKnowledgeProduct() catalog.KnowledgeProduct {
	return catalog.KnowledgeProduct{
		Name: "policy_kb", Collection: "policy_bge_v2", SourceNamespace: "kb.policy",
		Products:         catalog.KnowledgeProductRelations{Chunks: testKBChunks, Documents: testKBDocuments},
		IndexPublication: testKBSnapshot,
		Attributes: []catalog.KBAttribute{
			{Name: "kb_department", Type: "enum", Scope: "kb_department"},
			{Name: "classification", Type: "enum", Scope: "classification"},
			{Name: "job_role", Type: "text"},
			{Name: "valid_until", Type: "date"},
		},
		Scopes: []string{"kb_department", "classification"},
	}
}

func kbTestInputs() RetrievalPreparationInputsV1 {
	chunkFields := []catalog.Field{kbTestField("doc_id", "text"), kbTestField("version", "integer"),
		kbTestField("chunk_index", "integer"), kbTestField("text", "text"), kbTestField("text_sha256", "text"),
		kbTestField("kb_department", "text"), kbTestField("classification", "text"), kbTestField("job_role", "text"),
		kbTestField("labels", "text"), kbTestField("valid_until", "date")}
	documentFields := []catalog.Field{kbTestField("doc_id", "text"), kbTestField("version", "integer"),
		kbTestField("title", "text"), kbTestField("document_type", "text"), kbTestField("kb_department", "text"),
		kbTestField("classification", "text"), kbTestField("job_role", "text"), kbTestField("labels", "text"),
		kbTestField("valid_from", "date"), kbTestField("valid_until", "date"), kbTestField("content_sha256", "text"),
		kbTestField("chunk_count", "integer")}
	chunks := kbTestProduct(testKBChunks, "kb.policy.chunks", []string{"doc_id", "version", "chunk_index"}, chunkFields)
	documents := kbTestProduct(testKBDocuments, "kb.policy.documents", []string{"doc_id", "version"}, documentFields)
	chunksPub := kbTestPublication(testKBChunks, "kb.policy.chunks")
	documentsPub := kbTestPublication(testKBDocuments, "kb.policy.documents")
	view := CatalogView{
		Digest: strings.Repeat("e", 64), Version: "catalog-kb-test",
		Products:             map[string]catalog.Product{testKBChunks: chunks, testKBDocuments: documents},
		SnapshotPublications: []catalog.SnapshotPublication{chunksPub, documentsPub},
		Scopes: []catalog.Scope{
			{Name: "classification", Type: "enum", AllowedValues: []string{"confidential", "internal", "leadership", "public"}},
			{Name: "kb_department", Type: "enum", AllowedValues: []string{"finance", "hr", "legal"}},
		},
	}
	grant := Grant{
		ApprovedProducts: []string{testKBChunks, testKBDocuments},
		ApprovedColumns: map[string][]string{
			testKBChunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
			testKBDocuments: {"doc_id", "version", "title", "kb_department", "classification"},
		},
		MandatoryScope:  json.RawMessage(`{"classification":["internal","public"],"kb_department":["finance"]}`),
		ExposureProfile: exposure.ProfileV5,
		PredicateLimits: queryplan.PredicateLimits{MaxRawLiteralsPerQuery: 64, MaxUniqueAtomsPerQuery: 64,
			MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 65536},
	}
	normalForm := retrieval.NormalForm{
		Version: "taskgate-retrieval-nf-v1", Tool: retrieval.ToolSearch, KnowledgeProduct: "policy_kb", Epoch: testKBSnapshot,
		Atoms: []retrieval.Atom{
			{Attribute: "classification", Type: "enum", Operator: "=", CanonicalLiteral: "s:internal"},
			{Attribute: "job_role", Type: "text", Operator: "in", CanonicalLiteral: "s:analyst"},
			{Attribute: "job_role", Type: "text", Operator: "in", CanonicalLiteral: "s:manager"},
			{Attribute: "kb_department", Type: "enum", Operator: "=", CanonicalLiteral: "s:finance", Injected: true},
		},
		Query: &retrieval.QueryCommitment{Kind: "text", SHA256: strings.Repeat("4", 64), Dimension: 3},
		K:     5,
	}
	return RetrievalPreparationInputsV1{
		Grant: grant, Catalog: view,
		SnapshotBindings: map[string]SnapshotBinding{chunksPub.Name: kbTestBinding(chunksPub), documentsPub.Name: kbTestBinding(documentsPub)},
		KnowledgeProduct: kbTestKnowledgeProduct(),
		NormalForm:       normalForm,
		QueryVector:      []float32{0.1, -0.25, 0.5},
		VectorSource: RetrievalVectorSource{
			LogicalName: "policy_kb_chunk_vectors", Schema: "kb", Relation: "chunks",
			Collection: "policy_bge_v2", CollectionColumn: "collection", EmbeddingColumn: "embedding",
			EntityColumns:    []string{"doc_id", "version", "chunk_index"},
			AttributeColumns: map[string]string{"kb_department": "department"},
		},
	}
}

func TestPrepareRetrievalSealsAndReproduces(t *testing.T) {
	inputs := kbTestInputs()
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRetrievalWith(inputs, compiler)
	if err != nil {
		t.Fatalf("prepare retrieval: %v", err)
	}
	if err := prepared.RequireRetrievalInputs(inputs, compiler); err != nil {
		t.Fatalf("require inputs: %v", err)
	}
	again, err := PrepareRetrievalWith(inputs, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if again.Binding().SHA256 != prepared.Binding().SHA256 {
		t.Fatal("two preparations of the same retrieval inputs sealed different bindings")
	}
	visible, companion, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{visible, companion} {
		for _, want := range []string{`"tg_retrieval_hits"`, `"embedding" <=> E'[0.1,-0.25,0.5]'`, "LIMIT 5",
			`"collection" = E'policy_bge_v2'`, `"classification" = E'internal'`, `"job_role" IN (E'analyst', E'manager')`} {
			if !strings.Contains(statement, want) {
				t.Fatalf("statement lacks %s:\n%s", want, statement)
			}
		}
		if strings.Contains(statement, "finance") {
			t.Fatalf("injected scope atom rendered into the statement; the policy renderer owns it:\n%s", statement)
		}
	}
	for _, id := range retrievalColumnIDs(testKBChunks) {
		if !strings.Contains(visible, queryplan.RelationalOutputAlias(id)) {
			t.Fatalf("visible statement lacks the compiled alias of %s", id)
		}
	}
	for _, key := range []string{"tg_policy_kb_chunks_doc_id", "tg_policy_kb_chunks_version", "tg_policy_kb_chunks_chunk_index"} {
		if !strings.Contains(companion, key) {
			t.Fatalf("companion statement lacks %s", key)
		}
	}
	normalForm, err := inputs.NormalForm.Digest()
	if err != nil {
		t.Fatal(err)
	}
	binding := prepared.Binding()
	if binding.NormalFormSHA256 != normalForm {
		t.Fatal("binding does not carry the retrieval normal form digest")
	}
	if binding.EstimatedBaseFacts != 5 {
		t.Fatalf("estimated base facts %d, want k=5", binding.EstimatedBaseFacts)
	}
	footprint, err := prepared.PredicateFootprint()
	if err != nil {
		t.Fatal(err)
	}
	if footprint == nil || footprint.UniqueAtomCount != 3 {
		t.Fatalf("predicate footprint %+v, want 3 caller atoms", footprint)
	}
	// The policy renderer is the gate the statements must pass at execution.
	engine := sqlpolicy.New(sqlpolicy.Config{})
	for _, statement := range []string{visible, companion} {
		decision, err := engine.Authorize(sqlpolicy.Request{SQL: statement, Grant: prepared.PolicyGrant(), RowLimit: 5})
		if err != nil {
			t.Fatalf("policy rejected the prepared statement: %v\n%s", err, statement)
		}
		rendered := decision.SQL
		for _, want := range []string{`"kb"."chunks"`, `"department" = E'finance'`, `<=> '[0.1,-0.25,0.5]'`} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("rendered statement lacks %s:\n%s", want, rendered)
			}
		}
	}
}

func TestPrepareRetrievalBindsVectorAndNormalForm(t *testing.T) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	base := kbTestInputs()
	prepared, err := PrepareRetrievalWith(base, compiler)
	if err != nil {
		t.Fatal(err)
	}
	moved := kbTestInputs()
	moved.QueryVector = []float32{0.1, -0.25, 0.51}
	other, err := PrepareRetrievalWith(moved, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if other.Binding().VisibleTargetSHA256 == prepared.Binding().VisibleTargetSHA256 {
		t.Fatal("a different query vector sealed the same visible target")
	}
	if err := prepared.RequireRetrievalInputs(moved, compiler); err == nil {
		t.Fatal("prepared retrieval accepted inputs with another query vector")
	}
	rek := kbTestInputs()
	rek.NormalForm.K = 6
	bigger, err := PrepareRetrievalWith(rek, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if bigger.Binding().NormalFormSHA256 == prepared.Binding().NormalFormSHA256 {
		t.Fatal("a different k sealed the same normal form")
	}
}

func TestPrepareRetrievalRefusesUnpreparedShapes(t *testing.T) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	fetchWithAtoms := kbTestInputs()
	fetchWithAtoms.NormalForm.Tool = retrieval.ToolFetch
	fetchWithAtoms.NormalForm.Query = nil
	fetchWithAtoms.NormalForm.K = 0
	fetchWithAtoms.NormalForm.Chunks = []retrieval.ChunkRef{{DocID: "D-1", Version: 1, ChunkIndex: 0}}
	fetchWithAtoms.QueryVector = nil
	if _, err := PrepareRetrievalWith(fetchWithAtoms, compiler); err == nil {
		t.Fatal("fetch with caller atoms prepared")
	}
	has := kbTestInputs()
	has.NormalForm.Atoms = append(has.NormalForm.Atoms, retrieval.Atom{Attribute: "labels", Type: "text_set", Operator: "has", CanonicalLiteral: "s:urgent"})
	if _, err := PrepareRetrievalWith(has, compiler); err == nil {
		t.Fatal("text_set has-atom prepared without a plan rendering")
	}
	wrongProfile := kbTestInputs()
	wrongProfile.Grant.ExposureProfile = exposure.ProfileV4
	if _, err := PrepareRetrievalWith(wrongProfile, compiler); err == nil {
		t.Fatal("retrieval prepared under a profile without a predicate footprint")
	}
	untagged := kbTestInputs()
	untagged.NormalForm.Atoms[0].CanonicalLiteral = "internal"
	if _, err := PrepareRetrievalWith(untagged, compiler); err == nil {
		t.Fatal("atom literal without its canonical type tag accepted")
	}
	noVector := kbTestInputs()
	noVector.QueryVector = nil
	if _, err := PrepareRetrievalWith(noVector, compiler); err == nil {
		t.Fatal("search prepared without a query vector")
	}
	unscoped := kbTestInputs()
	unscoped.Grant.MandatoryScope = json.RawMessage(`{"classification":["internal"]}`)
	if _, err := PrepareRetrievalWith(unscoped, compiler); err == nil {
		t.Fatal("retrieval prepared for a task that carries no value for a mandatory scope")
	}
}

func TestPrepareRetrievalFetchNamesItsChunks(t *testing.T) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	inputs := kbTestInputs()
	inputs.NormalForm.Tool = retrieval.ToolFetch
	inputs.NormalForm.Query = nil
	inputs.NormalForm.K = 0
	inputs.NormalForm.Chunks = []retrieval.ChunkRef{{DocID: "D-9", Version: 2, ChunkIndex: 3}, {DocID: "D-1", Version: 1, ChunkIndex: 0}}
	inputs.NormalForm.Atoms = []retrieval.Atom{{Attribute: "kb_department", Type: "enum", Operator: "=", CanonicalLiteral: "s:finance", Injected: true}}
	inputs.QueryVector = nil
	prepared, err := PrepareRetrievalWith(inputs, compiler)
	if err != nil {
		t.Fatalf("prepare fetch: %v", err)
	}
	visible, companion, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{visible, companion} {
		for _, want := range []string{`("doc_id" = E'D-1' AND "version" = 1 AND "chunk_index" = 0) OR ("doc_id" = E'D-9' AND "version" = 2 AND "chunk_index" = 3)`, "LIMIT 2", "0 AS \"tg_distance\""} {
			if !strings.Contains(statement, want) {
				t.Fatalf("fetch statement lacks %s:\n%s", want, statement)
			}
		}
		if strings.Contains(statement, "<=>") {
			t.Fatalf("fetch statement ranks by a vector:\n%s", statement)
		}
	}
	if prepared.Binding().EstimatedBaseFacts != 2 {
		t.Fatalf("estimated base facts %d, want 2", prepared.Binding().EstimatedBaseFacts)
	}
	footprint, err := prepared.PredicateFootprint()
	if err != nil {
		t.Fatal(err)
	}
	if footprint == nil || footprint.UniqueAtomCount != 0 {
		t.Fatalf("fetch footprint %+v, want no caller atoms", footprint)
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	for _, statement := range []string{visible, companion} {
		if _, err := engine.Authorize(sqlpolicy.Request{SQL: statement, Grant: prepared.PolicyGrant(), RowLimit: 2}); err != nil {
			t.Fatalf("policy rejected the fetch statement: %v\n%s", err, statement)
		}
	}
	if err := prepared.RequireRetrievalInputs(inputs, compiler); err != nil {
		t.Fatal(err)
	}
}
