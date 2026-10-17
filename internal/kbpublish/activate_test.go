package kbpublish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// fixtureCatalogAndManifest builds the knowledge-product Catalog from the
// internal/catalog fixture and a manifest that is exactly the epoch it binds.
func fixtureCatalogAndManifest(t *testing.T, mutateCatalog func(string) string) (*catalog.Catalog, Manifest) {
	t.Helper()
	spec := Spec{KnowledgeProduct: "policy_kb", Collection: "policy_v1", SourceID: "taskgate-demo-travel", CatalogSource: "travel_demo",
		SourceNamespace: "kb.policy", Epoch: "policy-kb-test-v1",
		Segmentation: Segmentation{Function: SegmentationSectionWindowV1, WindowChars: 800},
		Embedding:    Embedding{Model: EmbeddingHashProjectionV1, Dimension: 256},
		ExtractionRules: []ExtractionRule{
			{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
			{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
		}}
	segmentation, embedding, rules, err := DeclarationDigests(spec)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Version: ManifestVersion, KnowledgeProduct: "policy_kb", Collection: "policy_v1", Epoch: "policy-kb-test-v1",
		SourceID: "taskgate-demo-travel", CatalogSource: "travel_demo", SourceNamespace: "kb.policy", SchemaDigest: strings.Repeat("ab", 32),
		CorpusSHA256: strings.Repeat("cd", 32), Segmentation: spec.Segmentation, SegmentationDigest: segmentation,
		Embedding: spec.Embedding, EmbeddingDigest: embedding, ExtractionRules: spec.ExtractionRules, ExtractionRulesDigest: rules,
		DocumentTypes: []string{"contract", "approval", "policy", "report"}, Retrieval: Retrieval{KMax: 20, Aggregates: []string{"count", "sum"}},
		Collation: Collation{Name: "en_US.utf8", Version: "2.36"}, Publications: map[string]PublicationRef{}}
	digits := map[string][3]string{"documents": {"1", "2", "3"}, "chunks": {"4", "5", "6"}, "values": {"7", "8", "9"}}
	for _, kind := range RelationKinds {
		names := spec.Relation(kind)
		manifest.Publications[kind] = PublicationRef{Product: names.Product, PublicationName: names.PublicationName,
			SourceRelation: names.SourceRelation, OrdinalSidecar: names.OrdinalSidecar, SourceNamespace: names.SourceNamespace, RowCount: 1,
			SidecarDigest: strings.Repeat(digits[kind][0], 64), DictionaryDigest: strings.Repeat(digits[kind][1], 64), ManifestDigest: strings.Repeat(digits[kind][2], 64)}
	}
	if manifest.ManifestDigest, err = manifest.Digest(); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := os.ReadFile(filepath.Join("..", "catalog", "testdata", "kb-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer("{{SEGMENTATION}}", segmentation, "{{EMBEDDING}}", embedding, "{{RULES}}", rules,
		"manifest_digest: "+strings.Repeat("a", 64), "manifest_digest: "+manifest.ManifestDigest).Replace(string(fragment))
	if mutateCatalog != nil {
		text = mutateCatalog(text)
	}
	merged, err := catalog.MergeYAMLFragment(base, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := catalog.Parse(merged)
	if err != nil {
		t.Fatalf("parse knowledge catalog: %v", err)
	}
	return parsed, manifest
}

func TestVerifyCatalogBindingAcceptsBoundEpochAndRejectsDrift(t *testing.T) {
	cat, manifest := fixtureCatalogAndManifest(t, nil)
	product, _ := cat.LookupKnowledgeProduct("policy_kb")
	if err := VerifyCatalogBinding(cat, product, manifest); err != nil {
		t.Fatalf("bound epoch rejected: %v", err)
	}
	drifts := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{"manifest digest", func(m *Manifest) { m.ManifestDigest = strings.Repeat("f", 64) }, "manifest digest"},
		{"epoch", func(m *Manifest) { m.Epoch = "policy-kb-test-v2" }, "identity"},
		{"collection", func(m *Manifest) { m.Collection = "other" }, "identity"},
		{"segmentation digest", func(m *Manifest) { m.SegmentationDigest = strings.Repeat("e", 64) }, "segmentation, embedding or extraction"},
		{"chunk bundle digest", func(m *Manifest) {
			ref := m.Publications["chunks"]
			ref.ManifestDigest = strings.Repeat("0", 64)
			m.Publications["chunks"] = ref
		}, "chunks publication digests differ"},
		{"values relation renamed", func(m *Manifest) {
			ref := m.Publications["values"]
			ref.SourceRelation = "reporting.somewhere_else"
			m.Publications["values"] = ref
		}, "values publication identity differs"},
		{"missing publication", func(m *Manifest) { delete(m.Publications, "documents") }, "lacks the documents publication"},
	}
	for _, drift := range drifts {
		t.Run(drift.name, func(t *testing.T) {
			_, candidate := fixtureCatalogAndManifest(t, nil)
			drift.mutate(&candidate)
			if drift.name != "manifest digest" {
				// Keep the self-digest consistent so only the Catalog binding fails.
				candidate.ManifestDigest, _ = candidate.Digest()
				fragmentDigest := candidate.ManifestDigest
				cat, _ := fixtureCatalogAndManifest(t, func(text string) string {
					return strings.Replace(text, "manifest_digest: "+manifest.ManifestDigest, "manifest_digest: "+fragmentDigest, 1)
				})
				product, _ := cat.LookupKnowledgeProduct("policy_kb")
				err := VerifyCatalogBinding(cat, product, candidate)
				if err == nil || !strings.Contains(err.Error(), drift.want) {
					t.Fatalf("drift %s: err = %v, want %q", drift.name, err, drift.want)
				}
				return
			}
			err := VerifyCatalogBinding(cat, product, candidate)
			if err == nil || !strings.Contains(err.Error(), drift.want) {
				t.Fatalf("drift %s: err = %v, want %q", drift.name, err, drift.want)
			}
		})
	}
}

func TestActivateFromDirectoryFailsClosed(t *testing.T) {
	cat, manifest := fixtureCatalogAndManifest(t, nil)
	root := t.TempDir()
	kbDir := KBDir(root)
	if _, err := ActivateFromDirectory(cat, kbDir); err == nil || !strings.Contains(err.Error(), "stat kb manifest directory") {
		t.Fatalf("missing kb manifest directory: %v", err)
	}
	if _, err := ActivateFromDirectory(cat, ""); err == nil {
		t.Fatal("empty kb manifest directory accepted")
	}
	if err := os.MkdirAll(filepath.Join(kbDir, manifest.Epoch), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, kbDir); err == nil || !strings.Contains(err.Error(), "read kb publication manifest") {
		t.Fatalf("missing manifest: %v", err)
	}
	path := EpochManifestPath(kbDir, manifest.Epoch)
	if path != ManifestPath(root, manifest.Epoch) {
		t.Fatalf("kb-publish layout %s and gateway layout %s disagree", ManifestPath(root, manifest.Epoch), path)
	}
	encoded, _ := encodeManifest(manifest)
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	manifests, err := ActivateFromDirectory(cat, kbDir)
	if err != nil || len(manifests) != 1 || manifests[0].ManifestDigest != manifest.ManifestDigest {
		t.Fatalf("activation = %v, %v", manifests, err)
	}
	// Files the publisher leaves beside the manifest are not activated and do
	// not block activation; entries the Catalog does not declare do.
	if err := os.WriteFile(filepath.Join(kbDir, manifest.Epoch, "catalog-fragment.yaml"), []byte("products: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, kbDir); err != nil {
		t.Fatalf("publisher side file refused: %v", err)
	}
	stray := filepath.Join(kbDir, "policy-kb-undeclared-epoch")
	if err := os.Mkdir(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, kbDir); err == nil || !strings.Contains(err.Error(), "undeclared epoch") {
		t.Fatalf("undeclared epoch accepted: %v", err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, kbDir); err == nil || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("stray file accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(kbDir, "README")); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked-kb")
	if err := os.Symlink(kbDir, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, linked); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked kb manifest directory accepted: %v", err)
	}
	// A byte edited in the stored manifest breaks its self-digest.
	tampered := strings.Replace(string(encoded), `"window_chars": 800`, `"window_chars": 801`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateFromDirectory(cat, kbDir); err == nil || !strings.Contains(err.Error(), "digest does not match its content") {
		t.Fatalf("tampered manifest: %v", err)
	}
	plain, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if manifests, err := ActivateFromDirectory(plain, ""); err != nil || manifests != nil {
		t.Fatalf("catalog without knowledge products must activate trivially: %v %v", manifests, err)
	}
}
