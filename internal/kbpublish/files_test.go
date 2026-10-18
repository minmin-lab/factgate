package kbpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

// The originals' render manifest (docs/kb-design.md §12.9) must hash to the
// spec's digest and list exactly one admitted file per document version.
func TestLoadFileManifestRequiresAnExactCover(t *testing.T) {
	documents := []kbcorpus.Document{{DocID: "D-1", Version: 1}, {DocID: "D-1", Version: 2}, {DocID: "D-2", Version: 1}}
	line := func(doc string, version, size int, media string) string {
		return `{"byte_size":` + itoa(size) + `,"doc_id":"` + doc + `","file_sha256":"` + strings.Repeat("ab", 32) +
			`","media_type":"` + media + `","version":` + itoa(version) + `}`
	}
	write := func(lines ...string) Spec {
		t.Helper()
		path := filepath.Join(t.TempDir(), "manifest.jsonl")
		data := []byte(strings.Join(lines, "\n") + "\n")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return Spec{Files: &FilesRef{Renderer: "r", Manifest: path, SHA256: hex.EncodeToString(sum[:]), ObjectPrefix: "originals/v2"}}
	}
	good := write(line("D-1", 1, 10, "application/pdf"), line("D-2", 1, 10, "application/pdf"), line("D-1", 2, 10, "application/pdf"))
	entries, err := LoadFileManifest(good, documents)
	if err != nil || len(entries) != 3 {
		t.Fatalf("exact cover refused: %v (%d)", err, len(entries))
	}
	if key := good.Files.ObjectKey("D-1", 2, "application/pdf"); key != "originals/v2/D-1-v2.pdf" {
		t.Fatalf("object key %q", key)
	}
	for name, spec := range map[string]Spec{
		"missing":    write(line("D-1", 1, 10, "application/pdf"), line("D-2", 1, 10, "application/pdf")),
		"duplicate":  write(line("D-1", 1, 10, "application/pdf"), line("D-1", 1, 10, "application/pdf"), line("D-2", 1, 10, "application/pdf"), line("D-1", 2, 10, "application/pdf")),
		"extra":      write(line("D-1", 1, 10, "application/pdf"), line("D-2", 1, 10, "application/pdf"), line("D-1", 2, 10, "application/pdf"), line("D-9", 1, 10, "application/pdf")),
		"media type": write(line("D-1", 1, 10, "text/html"), line("D-2", 1, 10, "application/pdf"), line("D-1", 2, 10, "application/pdf")),
		"empty file": write(line("D-1", 1, 0, "application/pdf"), line("D-2", 1, 10, "application/pdf"), line("D-1", 2, 10, "application/pdf")),
	} {
		if _, err := LoadFileManifest(spec, documents); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	drifted := good
	drifted.Files = &FilesRef{Renderer: "r", Manifest: good.Files.Manifest, SHA256: strings.Repeat("0", 64), ObjectPrefix: "originals/v2"}
	if _, err := LoadFileManifest(drifted, documents); err == nil {
		t.Fatal("accepted a manifest that differs from the declared digest")
	}
	if entries, err := LoadFileManifest(Spec{}, documents); err != nil || entries != nil {
		t.Fatalf("a spec without files: %v %v", entries, err)
	}
}

func TestSpecFilesDeclaresTheFourthRelation(t *testing.T) {
	spec := Spec{}
	if strings.Join(spec.Kinds(), ",") != "documents,chunks,values" {
		t.Fatalf("kinds without files: %v", spec.Kinds())
	}
	spec.Files = &FilesRef{Renderer: "r", Manifest: "m", SHA256: strings.Repeat("ab", 32), ObjectPrefix: "originals/v2"}
	if strings.Join(spec.Kinds(), ",") != "documents,chunks,values,files" {
		t.Fatalf("kinds with files: %v", spec.Kinds())
	}
	for _, prefix := range []string{"", "/abs", "a/../b", "Upper", "a//b"} {
		bad := Spec{Version: SpecVersion, Files: &FilesRef{Renderer: "r", Manifest: "m", SHA256: strings.Repeat("ab", 32), ObjectPrefix: prefix}}
		if objectPrefixPattern.MatchString(bad.Files.ObjectPrefix) {
			t.Fatalf("object prefix %q admitted", prefix)
		}
	}
}

func TestActivationRefusesAFilesDisagreement(t *testing.T) {
	// The fixture Catalog declares files and the manifest publishes them; drop
	// either side and activation must refuse, for the files relation and not
	// for any other reason.
	cat, manifest := fixtureCatalogAndManifest(t, nil)
	product := cat.KnowledgeProducts[0]
	if err := VerifyCatalogBinding(cat, product, manifest); err != nil {
		t.Fatalf("agreeing catalog and manifest refused: %v", err)
	}
	withoutFiles := manifest
	withoutFiles.Publications = map[string]PublicationRef{}
	for kind, reference := range manifest.Publications {
		if kind != FilesKind {
			withoutFiles.Publications[kind] = reference
		}
	}
	var err error
	if withoutFiles.ManifestDigest, err = withoutFiles.Digest(); err != nil {
		t.Fatal(err)
	}
	// Bind the Catalog to the manifest without files, so the digest check
	// passes and only the relation disagreement remains.
	cat.KBPublications[0].ManifestDigest = withoutFiles.ManifestDigest
	err = VerifyCatalogBinding(cat, product, withoutFiles)
	if err == nil || !strings.Contains(err.Error(), "disagree about the files relation") {
		t.Fatalf("Catalog with files, manifest without: %v", err)
	}
	cat.KBPublications[0].ManifestDigest = manifest.ManifestDigest
	product.Products.Files = ""
	err = VerifyCatalogBinding(cat, product, manifest)
	if err == nil || !strings.Contains(err.Error(), "disagree about the files relation") {
		t.Fatalf("manifest with files, knowledge product without: %v", err)
	}
}

func itoa(value int) string { return strconv.Itoa(value) }
