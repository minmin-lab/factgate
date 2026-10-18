package retrieval

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

func product() catalog.KnowledgeProduct {
	return catalog.KnowledgeProduct{Name: "policy_kb", IndexPublication: "policy-kb-test-v1",
		Attributes: []catalog.KBAttribute{
			{Name: "kb_department", Type: "enum", Scope: "kb_department"},
			{Name: "classification", Type: "enum", Scope: "classification"},
			{Name: "job_role", Type: "enum", AllowedValues: []string{"staff", "manager", "director", "executive", "hr"}},
			{Name: "labels", Type: "text_set"},
			{Name: "valid_until", Type: "date"},
		},
		Embedding: catalog.KBEmbedding{Model: catalog.KBEmbeddingHashProjectionV1, Dimension: 4},
		ExtractionRules: []catalog.KBExtractionRule{
			{DocumentType: "contract", Field: "amount", Type: "numeric", Source: "frontmatter", Scope: "document"},
			{DocumentType: "contract", Field: "counterparty", Type: "text", Source: "frontmatter", Scope: "document"},
			{DocumentType: "policy", Field: "effective_on", Type: "date", Source: "frontmatter", Scope: "document"},
		},
		Retrieval: catalog.KBRetrieval{KMax: 20, Exact: true, Aggregates: []string{"count", "sum", "max"}},
		Scopes:    []string{"kb_department", "classification"}}
}

func scope() map[string][]string {
	return map[string][]string{"kb_department": {"sales"}, "classification": {"public", "internal"}}
}

func decode(t *testing.T, tool, body string) Request {
	t.Helper()
	request, err := DecodeRequest(tool, []byte(body))
	if err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return request
}

func digestOf(t *testing.T, tool, body string) (NormalForm, string) {
	t.Helper()
	form, err := Normalize(product(), scope(), decode(t, tool, body))
	if err != nil {
		t.Fatalf("normalize %s: %v", body, err)
	}
	digest, err := form.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return form, digest
}

func TestEquivalentSearchesShareOneNormalForm(t *testing.T) {
	base := `{"filters":[{"attribute":"job_role","op":"in","values":["manager","staff"]},{"attribute":"valid_until","op":">=","value":"2026-10-01"}],"query":{"text":"Q3  Pricing Policy"},"k":8}`
	form, digest := digestOf(t, ToolSearch, base)
	variants := []string{
		`{"filters":[{"attribute":"valid_until","op":">=","value":"2026-10-01"},{"attribute":"job_role","op":"in","values":["staff","manager","staff"]}],"query":{"text":"  q3 pricing   policy "},"k":8}`,
		`{"tool":"search","filters":[{"attribute":"job_role","op":"in","values":["manager","staff"]},{"attribute":"valid_until","op":">=","value":"2026-10-01"}],"query":{"text":"Q3 PRICING POLICY"},"k":8}`,
	}
	for _, variant := range variants {
		_, other := digestOf(t, ToolSearch, variant)
		if other != digest {
			t.Fatalf("equivalent request produced a different h_N:\n%s\n%s", base, variant)
		}
	}
	// Single-value in equals =.
	_, single := digestOf(t, ToolSearch, `{"filters":[{"attribute":"job_role","op":"in","values":["hr"]}],"query":{"text":"x"},"k":1}`)
	_, equals := digestOf(t, ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","value":"hr"}],"query":{"text":"x"},"k":1}`)
	if single != equals {
		t.Fatal("single-value in and = differ")
	}
	// Multi-value in is NOT two = atoms (disjunction vs conjunction).
	_, pair := digestOf(t, ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","value":"hr"},{"attribute":"job_role","op":"=","value":"staff"}],"query":{"text":"x"},"k":1}`)
	_, in := digestOf(t, ToolSearch, `{"filters":[{"attribute":"job_role","op":"in","values":["hr","staff"]}],"query":{"text":"x"},"k":1}`)
	if pair == in {
		t.Fatal("in [a,b] collapsed onto two = atoms")
	}
	// Different k, different text, different injected scope all change h_N.
	for _, different := range []string{
		`{"filters":[{"attribute":"job_role","op":"in","values":["manager","staff"]},{"attribute":"valid_until","op":">=","value":"2026-10-01"}],"query":{"text":"Q3 pricing policy"},"k":9}`,
		`{"filters":[{"attribute":"job_role","op":"in","values":["manager","staff"]},{"attribute":"valid_until","op":">=","value":"2026-10-01"}],"query":{"text":"Q4 pricing policy"},"k":8}`,
	} {
		if _, other := digestOf(t, ToolSearch, different); other == digest {
			t.Fatalf("non-equivalent request shares h_N: %s", different)
		}
	}
	widened, err := Normalize(product(), map[string][]string{"kb_department": {"sales", "hr"}, "classification": {"public", "internal"}}, decode(t, ToolSearch, base))
	if err != nil {
		t.Fatal(err)
	}
	if other, _ := widened.Digest(); other == digest {
		t.Fatal("injected mandatory scope is not part of h_N")
	}
	// Injected atoms exist, are sorted after caller atoms of the same attribute, and are not caller atoms.
	injected := 0
	for _, atom := range form.Atoms {
		if atom.Injected {
			injected++
		}
	}
	if injected != 3 || len(form.CallerAtoms()) != 3 {
		t.Fatalf("atoms = %+v", form.Atoms)
	}
	if form.Query.Kind != "text" || form.K != 8 || form.Epoch != "policy-kb-test-v1" || form.Version != NormalFormVersion {
		t.Fatalf("form header = %+v", form)
	}
}

func TestVectorQueriesCommitAfterQuantization(t *testing.T) {
	_, a := digestOf(t, ToolSearch, `{"query":{"vector":[0.5,-0.25,1.0,0.0]},"k":3}`)
	_, b := digestOf(t, ToolSearch, `{"query":{"vector":[0.500001,-0.250001,1.5,0.0000001]},"k":3}`)
	if a != b {
		t.Fatal("vectors within quantization step (and clipping) must commit identically")
	}
	_, c := digestOf(t, ToolSearch, `{"query":{"vector":[0.5,-0.25,0.9,0.0]},"k":3}`)
	if a == c {
		t.Fatal("different vectors committed identically")
	}
	for _, body := range []string{
		`{"query":{"vector":[0.5,-0.25,1.0]},"k":3}`,
		`{"query":{"vector":[0.5,-0.25,1.0,0.0],"text":"both"},"k":3}`,
		`{"query":{},"k":3}`,
	} {
		_, err := Normalize(product(), scope(), decode(t, ToolSearch, body))
		requireCode(t, err, apierr.CodeRetrievalQueryUnsupported, body)
	}
}

func requireCode(t *testing.T, err error, code, context string) {
	t.Helper()
	var rejection *Error
	if !errors.As(err, &rejection) {
		t.Fatalf("%s: err = %v, want *Error", context, err)
	}
	if rejection.Code != code {
		t.Fatalf("%s: code = %s, want %s (%v)", context, rejection.Code, code, err)
	}
}

func TestClosedLanguageRejections(t *testing.T) {
	cases := []struct {
		tool string
		body string
		code string
	}{
		{ToolSearch, `{"filters":[{"attribute":"author","op":"=","value":"x"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"job_role","op":"like","value":"man%"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","value":"ceo"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"valid_until","op":"=","value":"01/10/2026"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"labels","op":"=","value":"budget"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"job_role","op":"in","values":[]}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"filters":[{"attribute":"job_role","op":"=","values":["a","b"]}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalFilterUnsupported},
		{ToolSearch, `{"query":{"text":"q"},"k":21}`, apierr.CodeRetrievalKExceeded},
		{ToolSearch, `{"query":{"text":"q"},"k":0}`, apierr.CodeRetrievalKExceeded},
		{ToolSearch, `{"query":{"text":"q"}}`, apierr.CodeRetrievalKExceeded},
		{ToolSearch, `{"filters":[{"attribute":"kb_department","op":"=","value":"hr"}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalScopeExceeded},
		{ToolSearch, `{"filters":[{"attribute":"classification","op":"in","values":["leadership","confidential"]}],"query":{"text":"q"},"k":1}`, apierr.CodeRetrievalScopeExceeded},
		{ToolSearch, `{"query":{"text":"q"},"k":1,"chunks":[{"doc_id":"D","version":1,"chunk_index":0}]}`, apierr.CodeRetrievalQueryUnsupported},
		{ToolFetch, `{"chunks":[]}`, apierr.CodeRetrievalQueryUnsupported},
		{ToolFetch, `{"chunks":[{"doc_id":"","version":1,"chunk_index":0}]}`, apierr.CodeRetrievalQueryUnsupported},
		{ToolFetch, `{"chunks":[{"doc_id":"D","version":1,"chunk_index":0}],"k":3}`, apierr.CodeRetrievalQueryUnsupported},
		{ToolAggregate, `{"field":"amount","aggregate":"avg"}`, apierr.CodeAggregateUnsupported},
		{ToolAggregate, `{"field":"title","aggregate":"count"}`, apierr.CodeAggregateUnsupported},
		{ToolAggregate, `{"field":"counterparty","aggregate":"sum"}`, apierr.CodeAggregateUnsupported},
		{ToolAggregate, `{"field":"counterparty","aggregate":"max"}`, apierr.CodeAggregateUnsupported},
		{ToolAggregate, `{"field":"amount","aggregate":"sum","k":4}`, apierr.CodeRetrievalQueryUnsupported},
	}
	for _, tc := range cases {
		request, err := DecodeRequest(tc.tool, []byte(tc.body))
		if err == nil {
			_, err = Normalize(product(), scope(), request)
		}
		requireCode(t, err, tc.code, tc.tool+" "+tc.body)
	}
	// Unknown members of the request are outside the language.
	for _, body := range []string{`{"query":{"text":"q"},"k":1,"rerank":"cross-encoder"}`, `{"query":{"text":"q","more_like_this":"D-1"},"k":1}`, `{"filters":[{"attribute":"job_role","op":"=","value":"hr","boost":2}],"query":{"text":"q"},"k":1}`} {
		_, err := DecodeRequest(ToolSearch, []byte(body))
		requireCode(t, err, apierr.CodeRetrievalQueryUnsupported, body)
	}
	if _, err := DecodeRequest(ToolSearch, []byte(`{"tool":"fetch","chunks":[{"doc_id":"D","version":1,"chunk_index":0}]}`)); err == nil {
		t.Fatal("tool mismatch accepted")
	}
	// A task whose grant lacks a mandatory scope cannot retrieve at all.
	_, err := Normalize(product(), map[string][]string{"kb_department": {"sales"}}, decode(t, ToolSearch, `{"query":{"text":"q"},"k":1}`))
	requireCode(t, err, apierr.CodeRetrievalScopeExceeded, "missing classification scope")
}

func TestFetchAndAggregateNormalForms(t *testing.T) {
	form, digest := digestOf(t, ToolFetch, `{"chunks":[{"doc_id":"D-2","version":1,"chunk_index":3},{"doc_id":"D-1","version":2,"chunk_index":0},{"doc_id":"D-2","version":1,"chunk_index":3}]}`)
	if len(form.Chunks) != 2 || form.Chunks[0].DocID != "D-1" || len(form.Atoms) != 0 || form.Query != nil {
		t.Fatalf("fetch form = %+v", form)
	}
	if _, other := digestOf(t, ToolFetch, `{"chunks":[{"doc_id":"D-1","version":2,"chunk_index":0},{"doc_id":"D-2","version":1,"chunk_index":3}]}`); other != digest {
		t.Fatal("reordered fetch differs")
	}
	aggregate, aggregateDigest := digestOf(t, ToolAggregate, `{"filters":[{"attribute":"job_role","op":"=","value":"hr"}],"field":"amount","aggregate":"sum"}`)
	if aggregate.Aggregate == nil || aggregate.Aggregate.ValueType != "numeric" || aggregate.Aggregate.Function != "sum" || len(aggregate.CallerAtoms()) != 1 {
		t.Fatalf("aggregate form = %+v", aggregate)
	}
	if _, count := digestOf(t, ToolAggregate, `{"filters":[{"attribute":"job_role","op":"=","value":"hr"}],"field":"amount","aggregate":"count"}`); count == aggregateDigest {
		t.Fatal("count and sum share h_N")
	}
	encoded, _ := json.Marshal(form)
	if !strings.Contains(string(encoded), `"version":"taskgate-retrieval-nf-v1"`) || strings.Contains(string(encoded), `"injected"`) {
		t.Fatalf("fetch JSON = %s", encoded)
	}
	if NormalizeText("  Ｑ3  Pricing\tPolicy\n") != "ｑ3 pricing policy" {
		t.Fatalf("NormalizeText = %q", NormalizeText("  Ｑ3  Pricing\tPolicy\n"))
	}
}

func keywordProduct() catalog.KnowledgeProduct {
	kp := product()
	kp.Retrieval.Keyword = true
	return kp
}

func keywordDigest(t *testing.T, body string) (NormalForm, string) {
	t.Helper()
	form, err := Normalize(keywordProduct(), scope(), decode(t, ToolSearch, body))
	if err != nil {
		t.Fatalf("normalize %s: %v", body, err)
	}
	digest, err := form.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return form, digest
}

func TestKeywordQueriesCommitToTheirCanonicalList(t *testing.T) {
	form, a := keywordDigest(t, `{"query":{"keywords":["违约金","上限"]},"k":5}`)
	if form.Query == nil || form.Query.Kind != QueryKindKeyword || form.Query.Dimension != 0 || form.Query.Quantization != "" {
		t.Fatalf("keyword commitment = %+v", form.Query)
	}
	// Order, duplicates, surrounding whitespace and NFC form do not change
	// which chunks match, so they do not change the commitment.
	for _, body := range []string{
		`{"query":{"keywords":["上限","违约金"]},"k":5}`,
		`{"query":{"keywords":["  违约金 ","上限","违约金"]},"k":5}`,
	} {
		if _, b := keywordDigest(t, body); b != a {
			t.Fatalf("%s committed differently from its canonical list", body)
		}
	}
	_, composed := keywordDigest(t, `{"query":{"keywords":["café"]},"k":5}`)
	_, decomposed := keywordDigest(t, `{"query":{"keywords":["café"]},"k":5}`)
	if composed != decomposed {
		t.Fatal("NFC-equivalent keywords committed differently")
	}
	// Matching is case-sensitive and inner whitespace is significant, so these
	// change the hit set and must change the commitment.
	_, upper := keywordDigest(t, `{"query":{"keywords":["ABC"]},"k":5}`)
	_, lower := keywordDigest(t, `{"query":{"keywords":["abc"]},"k":5}`)
	_, spaced := keywordDigest(t, `{"query":{"keywords":["a bc"]},"k":5}`)
	if upper == lower || lower == spaced {
		t.Fatal("keywords with different hit sets committed identically")
	}
	// A keyword list never shares a commitment with the same string as text.
	_, text := digestOf(t, ToolSearch, `{"query":{"text":"违约金"},"k":5}`)
	_, single := keywordDigest(t, `{"query":{"keywords":["违约金"]},"k":5}`)
	if text == single {
		t.Fatal("a keyword query and a text query committed identically")
	}
}

func TestKeywordQueriesOutsideTheLanguageAreRefused(t *testing.T) {
	long := strings.Repeat("长", MaxKeywordRunes+1)
	for _, body := range []string{
		`{"query":{"keywords":[]},"k":5}`,
		`{"query":{"keywords":["   "]},"k":5}`,
		`{"query":{"keywords":["a","b","c","d","e","f","g","h","i"]},"k":5}`,
		`{"query":{"keywords":["` + long + `"]},"k":5}`,
		`{"query":{"keywords":["tab\tinside"]},"k":5}`,
		`{"query":{"keywords":["违约金"],"text":"违约金"},"k":5}`,
		`{"query":{"keywords":["违约金"],"vector":[0.1,0.2,0.3,0.4]},"k":5}`,
	} {
		_, err := Normalize(keywordProduct(), scope(), decode(t, ToolSearch, body))
		requireCode(t, err, apierr.CodeRetrievalQueryUnsupported, body)
	}
	// A knowledge product that does not declare keyword search refuses it.
	_, err := Normalize(product(), scope(), decode(t, ToolSearch, `{"query":{"keywords":["违约金"]},"k":5}`))
	requireCode(t, err, apierr.CodeRetrievalQueryUnsupported, "undeclared keyword search")
	// k is bounded as for any search.
	_, err = Normalize(keywordProduct(), scope(), decode(t, ToolSearch, `{"query":{"keywords":["违约金"]},"k":21}`))
	requireCode(t, err, apierr.CodeRetrievalKExceeded, "k above k_max")
}
