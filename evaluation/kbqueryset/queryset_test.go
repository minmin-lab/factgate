package kbqueryset

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

func corpusFixture() []kbcorpus.Document {
	var docs []kbcorpus.Document
	for d := 0; d < 12; d++ {
		for _, typ := range []string{"contract", "policy"} {
			id := "D-" + string(rune('A'+d)) + typ[:1]
			docs = append(docs, kbcorpus.Document{DocID: id, Version: 1, Title: "t" + id, DocumentType: typ, Department: []string{"finance", "hr", "legal"}[d%3],
				Classification: "internal", Sections: []kbcorpus.Section{{Text: strings.Repeat("报销制度条款", 20)}}})
			docs = append(docs, kbcorpus.Document{DocID: id, Version: 2, Title: "t" + id, DocumentType: typ, Department: []string{"finance", "hr", "legal"}[d%3],
				Classification: "internal", Sections: []kbcorpus.Section{{Text: "修订版"}}})
		}
	}
	return docs
}

func TestSampleIsDeterministicStratifiedAndLatestVersion(t *testing.T) {
	docs := corpusFixture()
	first, err := Sample(docs, 12, 7)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Sample(docs, 12, 7)
	if len(first) != 12 || len(second) != 12 {
		t.Fatalf("sampled %d and %d targets", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("sampling is not deterministic")
		}
		if first[i].Version != 2 {
			t.Fatalf("target %+v is not the latest version", first[i])
		}
	}
	strata := map[string]int{}
	for _, target := range first {
		strata[target.Department+"/"+target.DocumentType]++
	}
	if len(strata) != 6 {
		t.Fatalf("12 targets cover %d of 6 strata: %v", len(strata), strata)
	}
	for key, n := range strata {
		if n != 2 {
			t.Fatalf("stratum %s has %d targets, want 2", key, n)
		}
	}
	if _, err := Sample(docs, 1000, 7); err == nil {
		t.Fatal("oversampling accepted")
	}
}

func TestPromptParseApply(t *testing.T) {
	docs := corpusFixture()
	targets, _ := Sample(docs, 2, 1)
	byKey := map[string]kbcorpus.Document{}
	for _, d := range docs {
		byKey[Key(d.DocID, d.Version)] = d
	}
	prompt, err := Prompt(targets, byKey)
	if err != nil || !strings.Contains(prompt, targets[0].DocID) || !strings.Contains(prompt, "paraphrase") {
		t.Fatalf("prompt %v: %s", err, prompt[:120])
	}
	reply := "```json\n[{\"doc_id\":\"" + targets[0].DocID + "\",\"version\":2,\"verbatim\":\"差旅报销的审批流程是什么\",\"paraphrase\":\"出差费用怎么报、谁来批\"}]\n```"
	generated, err := ParseResponse(reply)
	if err != nil || len(generated) != 1 {
		t.Fatalf("parse: %v", err)
	}
	queries, err := Apply(targets[0], generated[0])
	if err != nil || len(queries) != 2 || queries[0].Kind != KindVerbatim || queries[1].Kind != KindParaphrase {
		t.Fatalf("apply: %v %+v", err, queries)
	}
	bad := generated[0]
	bad.Paraphrase = bad.Verbatim
	if _, err := Apply(targets[0], bad); err == nil {
		t.Fatal("identical pair accepted")
	}
	short := generated[0]
	short.Verbatim = "太短"
	if _, err := Apply(targets[0], short); err == nil {
		t.Fatal("short query accepted")
	}
	leak := generated[0]
	leak.Paraphrase = "请给我 " + targets[0].DocID + " 的内容"
	if _, err := Apply(targets[0], leak); err == nil {
		t.Fatal("doc id leak accepted")
	}
	if _, err := ParseResponse("[{\"doc_id\":\"x\",\"version\":1,\"verbatim\":\"a\",\"paraphrase\":\"b\",\"extra\":1}]"); err == nil {
		t.Fatal("unknown field accepted")
	}
	encoded, digest, err := Encode(queries)
	if err != nil || len(digest) != 64 || !strings.HasPrefix(string(encoded), "{") {
		t.Fatalf("encode: %v", err)
	}
	again, digest2, _ := Encode([]Query{queries[1], queries[0]})
	if string(again) != string(encoded) || digest2 != digest {
		t.Fatal("encoding is not canonical")
	}
}
