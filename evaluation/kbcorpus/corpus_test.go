package kbcorpus

import (
	"strings"
	"testing"
)

func TestGenerationIsDeterministic(t *testing.T) {
	spec := Spec{Seed: 7, Documents: 300}
	_, first, manifestA, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, second, manifestB, err := Build(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || manifestA.DocumentsSHA256 != manifestB.DocumentsSHA256 {
		t.Fatal("two builds of the same spec differ")
	}
	_, _, other, err := Build(Spec{Seed: 8, Documents: 300})
	if err != nil {
		t.Fatal(err)
	}
	if other.DocumentsSHA256 == manifestA.DocumentsSHA256 {
		t.Fatal("different seeds produced the same corpus")
	}
}

func TestCorpusShapeAndVocabularies(t *testing.T) {
	documents, jsonl, manifest, err := Build(Spec{Seed: 20261015, Documents: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(Departments) != 12 || len(Roles) != 5 || len(Classifications) != 4 || len(DocumentTypes) != 4 {
		t.Fatalf("vocabulary sizes: departments=%d roles=%d classifications=%d types=%d", len(Departments), len(Roles), len(Classifications), len(DocumentTypes))
	}
	if manifest.Counts.Documents != 2000 || manifest.Counts.Versions <= 2000 || manifest.Counts.Versions > 2300 {
		t.Fatalf("counts = %+v", manifest.Counts)
	}
	for _, vocabulary := range []struct {
		name   string
		values []string
		counts map[string]int
	}{{"department", Departments, manifest.Counts.ByDepartment}, {"type", DocumentTypes, manifest.Counts.ByType},
		{"classification", Classifications, manifest.Counts.ByClassification}, {"role", Roles, manifest.Counts.ByRole}} {
		if len(vocabulary.counts) != len(vocabulary.values) {
			t.Fatalf("%s values present = %v, want all of %v", vocabulary.name, vocabulary.counts, vocabulary.values)
		}
	}
	if manifest.Counts.LeadershipSections == 0 {
		t.Fatal("no leadership-only sections generated")
	}
	leadershipInsideOtherDocs := 0
	versions := map[string]int{}
	for _, doc := range documents {
		versions[doc.DocID]++
		if len(doc.Sections) == 0 || doc.Fields == nil || doc.Title == "" || doc.ValidFrom == "" {
			t.Fatalf("incomplete document %+v", doc)
		}
		for _, label := range doc.Labels {
			if !contains(Labels, label) {
				t.Fatalf("unknown label %q", label)
			}
		}
		fields, rest, err := ParseFrontMatter(doc.Body)
		if err != nil {
			t.Fatalf("%s v%d: %v", doc.DocID, doc.Version, err)
		}
		if len(fields) != len(FieldsByType[doc.DocumentType]) {
			t.Fatalf("%s: front matter fields %v, want %v", doc.DocID, fields, FieldsByType[doc.DocumentType])
		}
		for name, value := range fields {
			if doc.Fields[name] != value {
				t.Fatalf("%s: field %s body=%q struct=%q", doc.DocID, name, value, doc.Fields[name])
			}
		}
		sections, err := ParseSections(rest)
		if err != nil {
			t.Fatalf("%s: %v", doc.DocID, err)
		}
		if len(sections) != len(doc.Sections) {
			t.Fatalf("%s: parsed %d sections, struct has %d", doc.DocID, len(sections), len(doc.Sections))
		}
		for index, section := range sections {
			if section != doc.Sections[index] {
				t.Fatalf("%s: section %d differs after round trip:\n%+v\n%+v", doc.DocID, index, section, doc.Sections[index])
			}
			if section.Classification == ClassificationLeadership && doc.Classification != ClassificationLeadership {
				leadershipInsideOtherDocs++
			}
		}
		if doc.Classification == ClassificationLeadership && doc.JobRole != "executive" {
			t.Fatalf("%s: leadership document readable by %s", doc.DocID, doc.JobRole)
		}
	}
	if leadershipInsideOtherDocs == 0 {
		t.Fatal("no leadership-only section inside a non-leadership document")
	}
	second := 0
	for _, count := range versions {
		if count == 2 {
			second++
		}
	}
	if second == 0 || second > 200 {
		t.Fatalf("documents with a second version = %d, want roughly 5%%", second)
	}
	decoded, err := DecodeJSONL(jsonl)
	if err != nil || len(decoded) != len(documents) {
		t.Fatalf("JSONL round trip: %d documents, err %v", len(decoded), err)
	}
	if strings.Count(string(jsonl), "\n") != len(documents) {
		t.Fatal("JSONL line count does not match document count")
	}
}

func TestFrozenManifestMatchesGenerator(t *testing.T) {
	frozen, err := FrozenManifest()
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Version != Version || frozen.Generator != Generator {
		t.Fatalf("frozen manifest header = %+v", frozen)
	}
	_, _, rebuilt, err := Build(frozen.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.DocumentsSHA256 != frozen.DocumentsSHA256 || rebuilt.DocumentsBytes != frozen.DocumentsBytes ||
		rebuilt.SpecSHA256 != frozen.SpecSHA256 || rebuilt.Counts.Versions != frozen.Counts.Versions {
		t.Fatalf("frozen corpus drifted: rebuilt %s (%d bytes), frozen %s (%d bytes)",
			rebuilt.DocumentsSHA256, rebuilt.DocumentsBytes, frozen.DocumentsSHA256, frozen.DocumentsBytes)
	}
}

func TestParsersRejectMalformedBodies(t *testing.T) {
	for _, body := range []string{"", "no front matter\n", "---\nfield value\n---\n", "---\na: 1\n", "---\na: 1\na: 2\n---\n"} {
		if _, _, err := ParseFrontMatter(body); err == nil {
			t.Fatalf("ParseFrontMatter accepted %q", body)
		}
	}
	if _, err := ParseSections("## Heading without classification\ntext\n"); err == nil {
		t.Fatal("ParseSections accepted a heading without classification")
	}
	if _, err := ParseSections(""); err == nil {
		t.Fatal("ParseSections accepted an empty body")
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
