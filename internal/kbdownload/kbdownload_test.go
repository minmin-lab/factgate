package kbdownload

import (
	"strings"
	"testing"
)

func TestCanonicalListSortsAndRefuses(t *testing.T) {
	list, err := CanonicalList([]Ref{{"D-000068", 1}, {"D-000002", 2}, {"D-000002", 1}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(list, ",") != "D-000002@1,D-000002@2,D-000068@1" {
		t.Fatalf("list %v", list)
	}
	for name, refs := range map[string][]Ref{
		"empty":     nil,
		"duplicate": {{"D-1", 1}, {"D-1", 1}},
		"at sign":   {{"D@1", 1}},
		"negative":  {{"D-1", -1}},
		"blank":     {{"", 1}},
	} {
		if _, err := CanonicalList(refs); err == nil {
			t.Fatalf("%s: accepted %v", name, refs)
		}
	}
	tooMany := make([]Ref, MaxDocuments+1)
	for i := range tooMany {
		tooMany[i] = Ref{DocID: "D-" + strings.Repeat("9", i+1), Version: 1}
	}
	if _, err := CanonicalList(tooMany); err == nil {
		t.Fatal("accepted more than MaxDocuments")
	}
}

func TestParseRefIsCanonical(t *testing.T) {
	ref, err := ParseRef("D-000068@1")
	if err != nil || ref != (Ref{"D-000068", 1}) {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, bad := range []string{"D-1", "D-1@", "D-1@01", "D-1@+1", "D-1@1@2", "@1", "D 1@1"} {
		if _, err := ParseRef(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestApprovedListDistinguishesAbsentFromMalformed(t *testing.T) {
	if _, present, err := ApprovedList(map[string]any{"classification": []any{"internal"}}); present || err != nil {
		t.Fatalf("a search task read as download: present=%v err=%v", present, err)
	}
	refs, present, err := ApprovedList(map[string]any{ScopeKey: []any{"D-1@1", "D-2@3"}})
	if !present || err != nil || len(refs) != 2 {
		t.Fatalf("%v %v %v", refs, present, err)
	}
	for _, bad := range []any{"D-1@1", []any{}, []any{"D-1"}, []any{1}} {
		if _, present, err := ApprovedList(map[string]any{ScopeKey: bad}); !present || err == nil {
			t.Fatalf("malformed %#v: present=%v err=%v", bad, present, err)
		}
	}
}

func TestAssembleIsDeterministicAndRefusesPartialDocuments(t *testing.T) {
	file := File{
		Ref: Ref{"D-000068", 1}, Title: "设备采购合同",
		Values: []Value{{"signed_on", "date", "2024-04-03"}, {"amount", "numeric", "1808856.89"}},
		Chunks: []Chunk{
			{Index: 1, Text: "第二段。", TextSHA256: sha256Hex("第二段。")},
			{Index: 0, Text: "第一段。", TextSHA256: sha256Hex("第一段。")},
		},
	}
	content, err := Assemble(file)
	if err != nil {
		t.Fatal(err)
	}
	want := "# 设备采购合同\n\ndoc_id: D-000068\nversion: 1\n\n## 字段\n\n- amount (numeric): 1808856.89\n- signed_on (date): 2024-04-03\n\n## 正文\n\n第一段。\n\n第二段。\n"
	if string(content) != want {
		t.Fatalf("file:\n%q\nwant:\n%q", content, want)
	}
	again, _ := Assemble(file)
	if SHA256(again) != SHA256(content) {
		t.Fatal("assembly is not deterministic")
	}
	file.Values = nil
	content, err = Assemble(file)
	if err != nil || strings.Contains(string(content), "## 字段") {
		t.Fatalf("values block without values: %q %v", content, err)
	}

	gap := file
	gap.Chunks = []Chunk{{Index: 0, Text: "a", TextSHA256: sha256Hex("a")}, {Index: 2, Text: "c", TextSHA256: sha256Hex("c")}}
	if _, err := Assemble(gap); err == nil {
		t.Fatal("assembled a document with a missing chunk")
	}
	drift := file
	drift.Chunks = []Chunk{{Index: 0, Text: "a", TextSHA256: sha256Hex("b")}}
	if _, err := Assemble(drift); err == nil {
		t.Fatal("assembled a chunk whose text does not match its digest")
	}
	multiline := file
	multiline.Title = "a\nb"
	if _, err := Assemble(multiline); err == nil {
		t.Fatal("assembled a title that spans lines")
	}
}
