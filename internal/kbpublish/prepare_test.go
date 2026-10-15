package kbpublish

import (
	"math"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

func sampleDocument(t *testing.T) kbcorpus.Document {
	t.Helper()
	documents, err := kbcorpus.Generate(kbcorpus.Spec{Seed: 3, Documents: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range documents {
		if doc.DocumentType == "contract" {
			return doc
		}
	}
	t.Fatal("no contract in sample")
	return kbcorpus.Document{}
}

func TestSegmentWindowsRespectLimitAndInheritSectionClassification(t *testing.T) {
	doc := kbcorpus.Document{DocID: "D-1", Version: 1, DocumentType: "policy", Department: "sales", Classification: "internal",
		JobRole: "staff", Labels: []string{"pricing"}, ValidUntil: "2027-01-01", Fields: map[string]string{"policy_no": "P-1", "effective_on": "2026-01-01"},
		Sections: []kbcorpus.Section{
			{Heading: "Purpose", Classification: "internal", Text: strings.Repeat("Sentence one is short. ", 20)},
			{Heading: "Leadership assessment", Classification: "leadership", Text: "Only leaders read this. It is brief."},
		}}
	doc.Body = kbcorpus.RenderBody(doc)
	chunks, err := Segment(doc, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 5 {
		t.Fatalf("expected the long section to be split into several windows, got %d chunks", len(chunks))
	}
	for index, chunk := range chunks {
		if chunk.ChunkIndex != index || len([]rune(chunk.Text)) > 100 || chunk.Text == "" {
			t.Fatalf("chunk %d violates index/window: %+v", index, chunk)
		}
		if chunk.Department != "sales" || chunk.JobRole != "staff" || chunk.ValidUntil != "2027-01-01" || len(chunk.Labels) != 1 {
			t.Fatalf("chunk %d lost document attributes: %+v", index, chunk)
		}
		if len(chunk.TextSHA256) != 64 {
			t.Fatalf("chunk %d has no digest", index)
		}
	}
	last := chunks[len(chunks)-1]
	if last.Classification != "leadership" || chunks[0].Classification != "internal" {
		t.Fatalf("section classification not inherited: first=%s last=%s", chunks[0].Classification, last.Classification)
	}
	again, _ := Segment(doc, 100)
	for index := range chunks {
		if chunks[index].Text != again[index].Text || chunks[index].TextSHA256 != again[index].TextSHA256 {
			t.Fatal("segmentation is not deterministic")
		}
	}
	// A single oversized sentence becomes its own chunk rather than being cut.
	long := kbcorpus.Document{DocID: "D-2", Version: 1, DocumentType: "policy", Department: "sales", Classification: "internal", JobRole: "staff",
		Fields:   map[string]string{"policy_no": "P-2", "effective_on": "2026-01-01"},
		Sections: []kbcorpus.Section{{Heading: "Purpose", Classification: "internal", Text: strings.Repeat("word ", 60) + "end."}}}
	long.Body = kbcorpus.RenderBody(long)
	chunks, err = Segment(long, 64)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("oversized sentence: chunks=%d err=%v", len(chunks), err)
	}
}

func TestEmbedIsDeterministicAndNormalized(t *testing.T) {
	a := Embed("Q3 pricing policy for the sales team", 64)
	b := Embed("Q3 pricing policy for the sales team", 64)
	c := Embed("completely different incident postmortem", 64)
	if len(a) != 64 || VectorLiteral(a) != VectorLiteral(b) {
		t.Fatal("embedding is not deterministic")
	}
	if VectorLiteral(a) == VectorLiteral(c) {
		t.Fatal("different texts produced identical embeddings")
	}
	var norm float64
	for _, component := range a {
		norm += float64(component) * float64(component)
	}
	if math.Abs(norm-1) > 1e-4 {
		t.Fatalf("embedding norm = %f, want 1", norm)
	}
	if literal := VectorLiteral(Embed("", 4)); literal != "[0.000000,0.000000,0.000000,0.000000]" {
		t.Fatalf("empty text literal = %s", literal)
	}
}

func TestExtractCanonicalizesDeclaredFields(t *testing.T) {
	doc := sampleDocument(t)
	rules := []ExtractionRule{
		{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
		{DocumentType: "contract", Field: "signed_on", Type: "date", Source: "frontmatter", Scope: "document"},
		{DocumentType: "contract", Field: "counterparty", Type: "text", Source: "frontmatter", Scope: "document"},
		{DocumentType: "approval", Field: "approved_amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
	}
	values, err := Extract(doc, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 {
		t.Fatalf("values = %+v, want the three contract rules only", values)
	}
	byField := map[string]Value{}
	for _, value := range values {
		byField[value.Field] = value
	}
	// Canonical values use the exposure encoding (typed prefix); Raw keeps the
	// front-matter text for the typed column.
	if !strings.HasPrefix(byField["amount"].CanonicalValue, "n:") || byField["signed_on"].CanonicalValue != "d:"+doc.Fields["signed_on"] ||
		byField["counterparty"].CanonicalValue != "s:"+doc.Fields["counterparty"] || byField["amount"].ValueType != "numeric" ||
		byField["amount"].Raw != doc.Fields["amount"] {
		t.Fatalf("canonical values = %+v", byField)
	}
	_, err = Extract(doc, []ExtractionRule{{DocumentType: "contract", Field: "missing", Type: "text", Source: "frontmatter", Scope: "document"}})
	if err == nil || !strings.Contains(err.Error(), "lacks field") {
		t.Fatalf("missing field error = %v", err)
	}
	_, err = Extract(doc, []ExtractionRule{{DocumentType: "contract", Field: "counterparty", Type: "numeric", Source: "frontmatter", Scope: "document"}})
	if err == nil {
		t.Fatal("non-numeric text accepted as numeric")
	}
}

func validSpec() Spec {
	return Spec{Version: SpecVersion, KnowledgeProduct: "policy_kb", Description: "test knowledge product", Sensitivity: "high",
		DocumentTypes: []string{"contract", "approval", "policy", "report"}, Retrieval: Retrieval{KMax: 20, Aggregates: []string{"count", "sum"}},
		Collection: "policy_v1", SourceID: "taskgate-demo-travel",
		CatalogSource: "travel_demo", SourceNamespace: "kb.policy", Epoch: "policy-kb-2026-10-v1",
		SchemaDigest: strings.Repeat("ab", 32), Corpus: CorpusRef{Path: "x.jsonl", SHA256: strings.Repeat("cd", 32)},
		Segmentation:    Segmentation{Function: SegmentationSectionWindowV1, WindowChars: 800},
		Embedding:       Embedding{Model: EmbeddingHashProjectionV1, Dimension: 256},
		ExtractionRules: []ExtractionRule{{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"}}}
}

func TestSpecValidationAndNames(t *testing.T) {
	spec := validSpec()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	names := spec.Relation("chunks")
	if names.Product != "policy_kb_chunks" || names.PublicationName != "policy-kb-2026-10-v1-chunks" ||
		names.SourceRelation != "reporting.kb_policy_kb_policy_kb_2026_10_v1_chunks" ||
		names.OrdinalSidecar != "taskgate_ordinal.kb_policy_kb_policy_kb_2026_10_v1_chunks" || names.SourceNamespace != "kb.policy.chunks" {
		t.Fatalf("relation names = %+v", names)
	}
	mutations := []func(*Spec){
		func(s *Spec) { s.Version = "v0" },
		func(s *Spec) { s.KnowledgeProduct = "Policy" },
		func(s *Spec) { s.SourceNamespace = "kb..policy" },
		func(s *Spec) { s.Epoch = "Epoch With Spaces" },
		func(s *Spec) { s.SchemaDigest = "nope" },
		func(s *Spec) { s.Corpus.SHA256 = "" },
		func(s *Spec) { s.Segmentation.WindowChars = 10 },
		func(s *Spec) { s.Embedding.Dimension = 1 },
		func(s *Spec) { s.ExtractionRules[0].Type = "float" },
		func(s *Spec) { s.ExtractionRules[0].Scope = "collection" },
		func(s *Spec) { s.ExtractionRules = append(s.ExtractionRules, s.ExtractionRules[0]) },
	}
	for index, mutate := range mutations {
		candidate := validSpec()
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("mutation %d accepted", index)
		}
	}
	if _, err := ParseSpec([]byte(`{"version":"taskgate-kb-publish-spec-v1","unknown":1}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}
