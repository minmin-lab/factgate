package kboracle

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

func testBinding() retrieval.Binding {
	return retrieval.Binding{KnowledgeProduct: "policy_kb", SourceNamespace: "kb.policy", Epoch: "policy-kb-test-v1",
		CatalogSHA256: strings.Repeat("ab", 32), ChunksProduct: "policy_kb_chunks", DocumentsProduct: "policy_kb_documents",
		ChunksPublicationSHA256: strings.Repeat("cd", 32), DocumentsPublicationSHA256: strings.Repeat("ef", 32),
		CollationName: "en_US.utf8", CollationVersion: "2.36"}
}

func testProduct() catalog.KnowledgeProduct {
	return catalog.KnowledgeProduct{Name: "policy_kb", IndexPublication: "policy-kb-test-v1",
		Attributes: []catalog.KBAttribute{
			{Name: "kb_department", Type: "enum", Scope: "kb_department"},
			{Name: "classification", Type: "enum", Scope: "classification"},
			{Name: "job_role", Type: "enum", AllowedValues: []string{"staff", "manager"}},
			{Name: "valid_until", Type: "date"},
		},
		Embedding: catalog.KBEmbedding{Model: catalog.KBEmbeddingHashProjectionV1, Dimension: 4},
		Retrieval: catalog.KBRetrieval{KMax: 50}, Scopes: []string{"kb_department", "classification"}}
}

func normalize(t *testing.T, tool, body string) retrieval.NormalForm {
	t.Helper()
	request, err := retrieval.DecodeRequest(tool, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	form, err := retrieval.Normalize(testProduct(), map[string][]string{"kb_department": {"sales"}, "classification": {"public", "internal"}}, request)
	if err != nil {
		t.Fatal(err)
	}
	return form
}

func effectHashes(t *testing.T, effect retrieval.Effect) ([]string, []string, []string) {
	t.Helper()
	release, err := retrieval.Hashes(effect.Release)
	if err != nil {
		t.Fatal(err)
	}
	dependency, err := retrieval.Hashes(effect.Dependency)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := retrieval.Hashes(effect.Outcome())
	if err != nil {
		t.Fatal(err)
	}
	return release, dependency, outcome
}

func TestHandComputedCounts(t *testing.T) {
	form := normalize(t, retrieval.ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","value":"staff"},{"attribute":"valid_until","op":">=","value":"2026-01-01"}],"query":{"text":"pricing"},"k":4}`)
	hits := []retrieval.Hit{{DocID: "D-1", Version: 1, ChunkIndex: 0, Text: "alpha", Attributes: testAttributes()}}
	effect, err := retrieval.BuildEffect(testBinding(), form, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Release) != 4 || len(effect.Dependency) != dependencyCount(1, 1, 2) || len(effect.Atoms) != 2 || len(effect.Outcome()) != 3 {
		t.Fatalf("one chunk: R=%d D=%d atoms=%d O=%d", len(effect.Release), len(effect.Dependency), len(effect.Atoms), len(effect.Outcome()))
	}
	// Two chunks of one document share the document row Fact.
	hits = append(hits, retrieval.Hit{DocID: "D-1", Version: 1, ChunkIndex: 1, Text: "beta", Attributes: testAttributes()})
	effect, err = retrieval.BuildEffect(testBinding(), form, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Release) != 8 || len(effect.Dependency) != dependencyCount(2, 1, 2) {
		t.Fatalf("two chunks one document: R=%d D=%d", len(effect.Release), len(effect.Dependency))
	}
	expected, err := Compute(testBinding(), form, []Chunk{{"D-1", 1, 0, "alpha", testAttributes()}, {"D-1", 1, 1, "beta", testAttributes()}})
	if err != nil {
		t.Fatal(err)
	}
	release, dependency, outcome := splitHashes(t, effect)
	if differences := Compare(expected, release, dependency, outcome); len(differences) != 0 {
		t.Fatalf("oracle disagrees: %+v", differences)
	}
	// Empty result: no base facts, atoms and one composite remain.
	effect, err = retrieval.BuildEffect(testBinding(), form, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(effect.Release) != 0 || len(effect.Dependency) != 0 || len(effect.Outcome()) != 3 {
		t.Fatalf("empty result: R=%d D=%d O=%d", len(effect.Release), len(effect.Dependency), len(effect.Outcome()))
	}
	expected, _ = Compute(testBinding(), form, nil)
	release, dependency, outcome = splitHashes(t, effect)
	if differences := Compare(expected, release, dependency, outcome); len(differences) != 0 {
		t.Fatalf("oracle disagrees on empty result: %+v", differences)
	}
}

func splitHashes(t *testing.T, effect retrieval.Effect) ([]string, []string, []string) {
	return effectHashes(t, effect)
}

func TestRandomHitSetsMatchOracleMemberByMember(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	for trial := 0; trial < 200; trial++ {
		filters := []string{}
		if rng.IntN(2) == 0 {
			filters = append(filters, `{"attribute":"job_role","op":"in","values":["staff","manager"]}`)
		}
		if rng.IntN(2) == 0 {
			filters = append(filters, fmt.Sprintf(`{"attribute":"valid_until","op":"<","value":"2027-%02d-01"}`, 1+rng.IntN(12)))
		}
		body := fmt.Sprintf(`{"filters":[%s],"query":{"text":"query %d"},"k":%d}`, strings.Join(filters, ","), rng.IntN(5), 1+rng.IntN(20))
		form := normalize(t, retrieval.ToolSearch, body)
		count := rng.IntN(12)
		var hits []retrieval.Hit
		var chunks []Chunk
		seen := map[string]struct{}{}
		for len(hits) < count {
			hit := retrieval.Hit{DocID: fmt.Sprintf("D-%d", rng.IntN(6)), Version: 1 + rng.IntN(2), ChunkIndex: rng.IntN(4), Text: fmt.Sprintf("text %d", rng.IntN(1000)), Attributes: map[string]string{"job_role": []string{"staff", "manager"}[rng.IntN(2)], "valid_until": fmt.Sprintf("2026-%02d-01", 1+rng.IntN(12))}}
			id := fmt.Sprintf("%s/%d/%d", hit.DocID, hit.Version, hit.ChunkIndex)
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			hits = append(hits, hit)
			chunks = append(chunks, Chunk{hit.DocID, hit.Version, hit.ChunkIndex, hit.Text, hit.Attributes})
		}
		effect, err := retrieval.BuildEffect(testBinding(), form, hits)
		if err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		expected, err := Compute(testBinding(), form, chunks)
		if err != nil {
			t.Fatalf("trial %d oracle: %v", trial, err)
		}
		release, dependency, outcome := splitHashes(t, effect)
		if differences := Compare(expected, release, dependency, outcome); len(differences) != 0 {
			t.Fatalf("trial %d (%s, %d hits): %+v", trial, body, count, differences)
		}
	}
}

func TestReplayAndDistinctRequestsOnComposite(t *testing.T) {
	form := normalize(t, retrieval.ToolSearch, `{"query":{"text":"pricing"},"k":4}`)
	hits := []retrieval.Hit{{DocID: "D-1", Version: 1, ChunkIndex: 0, Text: "alpha", Attributes: testAttributes()}, {DocID: "D-2", Version: 1, ChunkIndex: 0, Text: "gamma", Attributes: testAttributes()}}
	first, _ := retrieval.BuildEffect(testBinding(), form, hits)
	second, _ := retrieval.BuildEffect(testBinding(), form, []retrieval.Hit{hits[1], hits[0]})
	firstHash, _ := first.Composite.Hash()
	secondHash, _ := second.Composite.Hash()
	if firstHash != secondHash {
		t.Fatal("identical request and hit set (reordered) produced different composites")
	}
	other := normalize(t, retrieval.ToolSearch, `{"query":{"text":"pricing"},"k":5}`)
	third, _ := retrieval.BuildEffect(testBinding(), other, hits)
	thirdHash, _ := third.Composite.Hash()
	if thirdHash == firstHash {
		t.Fatal("different k produced the same composite")
	}
	approximate, _ := retrieval.BuildEffect(testBinding(), form, hits[:1])
	approximateHash, _ := approximate.Composite.Hash()
	if approximateHash == firstHash {
		t.Fatal("different hit set under the same request produced the same composite")
	}
	// A different Catalog binding changes every atom and the composite.
	rebound := testBinding()
	rebound.CatalogSHA256 = strings.Repeat("99", 32)
	withAtoms := normalize(t, retrieval.ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","value":"staff"}],"query":{"text":"pricing"},"k":4}`)
	a, _ := retrieval.BuildEffect(testBinding(), withAtoms, hits)
	b, _ := retrieval.BuildEffect(rebound, withAtoms, hits)
	aAtoms, _ := retrieval.Hashes(a.Atoms)
	bAtoms, _ := retrieval.Hashes(b.Atoms)
	if aAtoms[0] == bAtoms[0] {
		t.Fatal("atom identity ignores the Catalog digest")
	}
	fetch := normalize(t, retrieval.ToolFetch, `{"chunks":[{"doc_id":"D-1","version":1,"chunk_index":0}]}`)
	fetched, err := retrieval.BuildEffect(testBinding(), fetch, hits[:1])
	if err != nil || len(fetched.Atoms) != 0 || len(fetched.Outcome()) != 1 || len(fetched.Dependency) != dependencyCount(1, 1, 0) {
		t.Fatalf("fetch effect = atoms %d outcome %d dependency %d (%v)", len(fetched.Atoms), len(fetched.Outcome()), len(fetched.Dependency), err)
	}
	if _, err := retrieval.BuildEffect(testBinding(), form, []retrieval.Hit{hits[0], hits[0]}); err == nil {
		t.Fatal("duplicate hit accepted")
	}
}

// testAttributes gives a hit values for every attribute the tests filter on.
func testAttributes() map[string]string {
	return map[string]string{"job_role": "staff", "valid_until": "2026-06-01"}
}

// dependencyCount is the general algebra's F_D size for chunks hits in docs
// distinct documents under filtered caller-filtered attributes: four visible
// cells and one row per chunk, two key cells and one row per document, one
// chunk cell per filtered attribute (kb-design §5, measured 2026-10-16).
func dependencyCount(chunks, docs, filtered int) int {
	return chunks*(4+1+filtered) + docs*(2+1)
}
