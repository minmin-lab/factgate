package kbcorpus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestFrozenManifestV2BindsRawCorpusWhenPresent guards the v2 freeze: the
// committed manifest must be well formed, and whenever the (uncommitted) raw
// JSONL is on this machine its bytes must hash to the committed digest. The
// prose is LLM-written and cannot be regenerated, so the digest is the freeze.
func TestFrozenManifestV2BindsRawCorpusWhenPresent(t *testing.T) {
	data, err := os.ReadFile("manifest-v2.json")
	if err != nil {
		t.Skip("manifest-v2.json not committed yet (corpus v2 generation in progress)")
	}
	var manifest ManifestV2
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != VersionV2 || manifest.Generator != GeneratorV2 || manifest.Counts.Documents == 0 || len(manifest.DocumentsSHA256) != 64 || manifest.PlanSHA256 == "" {
		t.Fatalf("manifest header = %+v", manifest)
	}
	plans, err := Generate(manifest.PlanSpec)
	if err != nil {
		t.Fatal(err)
	}
	planJSONL, _ := EncodeJSONL(plans)
	planSum := sha256.Sum256(planJSONL)
	if hex.EncodeToString(planSum[:]) != manifest.PlanSHA256 {
		t.Fatal("the metadata plan no longer reproduces the digest the v2 manifest binds")
	}
	raw, err := os.ReadFile("raw/v2/documents.jsonl")
	if err != nil {
		t.Logf("raw corpus v2 not on this machine; digest binding not checked here")
		return
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != manifest.DocumentsSHA256 || int64(len(raw)) != manifest.DocumentsBytes {
		t.Fatalf("raw/v2/documents.jsonl drifted from the frozen manifest: %x", sum)
	}
	docs, err := DecodeJSONL(raw)
	if err != nil {
		t.Fatal(err)
	}
	counts := CountDocuments(docs)
	if counts.Documents != manifest.Counts.Documents || counts.Versions != manifest.Counts.Versions {
		t.Fatalf("counts %+v differ from manifest %+v", counts, manifest.Counts)
	}
	for _, doc := range docs[:min(len(docs), 200)] {
		fields, rest, err := ParseFrontMatter(doc.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(fields) != len(doc.Fields) {
			t.Fatalf("%s: front matter drift", doc.DocID)
		}
		if _, err := ParseSections(rest); err != nil {
			t.Fatal(err)
		}
	}
}
