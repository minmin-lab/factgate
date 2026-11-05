package catalog

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// knowledgeCatalogYAML merges the knowledge-product fixture into the
// repository Catalog with the declaration digests filled in.
func knowledgeCatalogYAML(t *testing.T, mutate func(string) string) []byte {
	t.Helper()
	base, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile("testdata/kb-fragment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	segmentation, _ := CanonicalDeclarationDigest(KBSegmentation{Function: KBSegmentationSectionWindowV1, WindowChars: 800})
	embedding, _ := CanonicalDeclarationDigest(KBEmbedding{Model: KBEmbeddingHashProjectionV1, Dimension: 256})
	rules, _ := CanonicalDeclarationDigest([]KBExtractionRule{
		{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
		{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
	})
	text := strings.NewReplacer("{{SEGMENTATION}}", segmentation, "{{EMBEDDING}}", embedding, "{{RULES}}", rules).Replace(string(fragment))
	if mutate != nil {
		text = mutate(text)
	}
	merged, err := MergeYAMLFragment(base, []byte(text))
	if err != nil {
		t.Fatalf("merge fragment: %v", err)
	}
	return merged
}

func TestKnowledgeProductFragmentMergesAndParses(t *testing.T) {
	parsed, err := Parse(knowledgeCatalogYAML(t, nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	product, ok := parsed.LookupKnowledgeProduct("policy_kb")
	if !ok || product.Products.Chunks != "policy_kb_chunks" || product.IndexPublication != "policy-kb-test-v1" ||
		len(product.Attributes) != 5 || product.Retrieval.KMax != 20 || product.Eligibility == nil {
		t.Fatalf("knowledge product = %+v", product)
	}
	publication, ok := parsed.LookupKBPublication("policy-kb-test-v1")
	if !ok || publication.KnowledgeProduct != "policy_kb" {
		t.Fatalf("kb publication = %+v", publication)
	}
	if _, ok := parsed.LookupProduct("policy_kb_values"); !ok {
		t.Fatal("values product missing after merge")
	}
	if _, ok := parsed.LookupSnapshotPublication("policy-kb-test-v1-chunks"); !ok {
		t.Fatal("chunks publication missing after merge")
	}
	plain, err := Parse(mustRead(t, "../../config/catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.SHA256 == parsed.SHA256 || len(plain.KnowledgeProducts) != 0 {
		t.Fatal("repository Catalog unexpectedly declares knowledge products or digest unchanged")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMergeYAMLFragmentRefusesRewrites(t *testing.T) {
	base := []byte("catalog_version: \"1\"\nproducts:\n  - name: a\n")
	if _, err := MergeYAMLFragment(base, []byte("catalog_version: \"2\"\n")); err == nil {
		t.Fatal("scalar rewrite accepted")
	}
	merged, err := MergeYAMLFragment(base, []byte("products:\n  - name: b\nextra:\n  - 1\n"))
	if err != nil || !strings.Contains(string(merged), "name: b") || !strings.Contains(string(merged), "extra:") {
		t.Fatalf("merge = %s, %v", merged, err)
	}
}

func TestKnowledgeProductValidationRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		message string
	}{
		{"missing relation product", replaceOnce("      chunks: policy_kb_chunks\n", "      chunks: policy_kb_missing\n"), "referenced product does not exist"},
		{"namespace mismatch", replaceOnce("    fact_namespace: kb.policy.chunks\n", "    fact_namespace: kb.other.chunks\n"), "fact_namespace must be <source_namespace>.chunks"},
		{"product snapshot is not the epoch", replaceOnce("    snapshot: policy-kb-test-v1\n    snapshot_publication: policy-kb-test-v1-values\n", "    snapshot: other\n    snapshot_publication: policy-kb-test-v1-values\n"), "snapshot must equal the index publication epoch"},
		{"values product lacks typed column", replaceOnce("      - {name: value_numeric, type: numeric, description: value_numeric}\n", ""), "must publish field value_numeric"},
		{"attribute scope not mandatory", replaceOnce("      - {name: job_role, type: enum, allowed_values: [staff, manager, director, executive, hr]}\n", "      - {name: job_role, type: enum, scope: job_role}\n"), "scoped attribute must be an enum attribute named after one of the knowledge product's mandatory scopes"},
		{"mandatory scope without attribute", replaceOnce("      - {name: classification, type: enum, scope: classification}\n", ""), "every mandatory scope needs an attribute bound to it"},
		{"unknown aggregate", replaceOnce("aggregates: [count, sum]", "aggregates: [count, avg]"), "aggregates must be count, sum, min, or max"},
		{"generation admitted", replaceOnce("generation_admitted: false", "generation_admitted: true"), "generated answers are not admitted"},
		{"extraction rule for undeclared type", replaceOnce("{document_type: policy, field: effective_on", "{document_type: memo, field: effective_on"), "undeclared document type"},
		{"segmentation digest drift", replaceOnce("window_chars: 800", "window_chars: 900"), "segmentation_digest in kb_publications does not match"},
		{"missing epoch binding", replaceOnce("    index_publication: policy-kb-test-v1\n", "    index_publication: policy-kb-test-v2\n"), "referenced kb publication epoch does not exist"},
		{"epoch binds another product", replaceOnce("    knowledge_product: policy_kb\n    manifest_digest: aaaa", "    knowledge_product: other_kb\n    manifest_digest: aaaa"), "referenced knowledge product does not exist"},
		{"bad manifest digest", replaceOnce("manifest_digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manifest_digest: nope"), "manifest_digest must be lowercase SHA-256"},
		{"scope not enum", replaceOnce("    scopes: [kb_department, classification]\n    eligibility:", "    scopes: [kb_department, expense_date]\n    eligibility:"), "knowledge product scopes must be enum Catalog scopes"},
		{"eligibility envelope on non-scope", replaceOnce("        classification:\n          from_attribute: clearance\n", "        job_role:\n          from_attribute: role\n"), "scope envelope must name a mandatory scope"},
		{"unknown field", replaceOnce("    retrieval: {k_max: 20,", "    reranker: none\n    retrieval: {k_max: 20,"), "reranker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(knowledgeCatalogYAML(t, tc.mutate))
			if err == nil {
				t.Fatal("invalid knowledge product declaration accepted")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.message)
			}
			if tc.name != "unknown field" && !errors.Is(err, ErrInvalidCatalog) && !errors.Is(err, ErrMissingField) {
				t.Fatalf("error = %v, want a catalog validation error", err)
			}
		})
	}
}

func replaceOnce(old, new string) func(string) string {
	return func(text string) string {
		if !strings.Contains(text, old) {
			panic("fixture anchor not found: " + old)
		}
		return strings.Replace(text, old, new, 1)
	}
}
