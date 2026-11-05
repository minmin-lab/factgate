package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestActivateKnowledgeProductsFailsClosedWithoutManifest(t *testing.T) {
	plain, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if manifests, err := activateKnowledgeProducts(plain, ""); err != nil || manifests != nil {
		t.Fatalf("Catalog without knowledge products: %v %v", manifests, err)
	}
	base, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile(filepath.Join("..", "..", "internal", "catalog", "testdata", "kb-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	segmentation, _ := catalog.CanonicalDeclarationDigest(catalog.KBSegmentation{Function: catalog.KBSegmentationSectionWindowV1, WindowChars: 800})
	embedding, _ := catalog.CanonicalDeclarationDigest(catalog.KBEmbedding{Model: catalog.KBEmbeddingHashProjectionV1, Dimension: 256})
	rules, _ := catalog.CanonicalDeclarationDigest([]catalog.KBExtractionRule{
		{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
		{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
	})
	text := strings.NewReplacer("{{SEGMENTATION}}", segmentation, "{{EMBEDDING}}", embedding, "{{RULES}}", rules).Replace(string(fragment))
	merged, err := catalog.MergeYAMLFragment(base, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	knowledge, err := catalog.Parse(merged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := activateKnowledgeProducts(knowledge, t.TempDir()); err == nil || !strings.Contains(err.Error(), "activate knowledge products") {
		t.Fatalf("missing manifest must refuse activation: %v", err)
	}
	if _, err := activateKnowledgeProducts(knowledge, ""); err == nil {
		t.Fatal("knowledge Catalog without an artifact directory must refuse activation")
	}
}
