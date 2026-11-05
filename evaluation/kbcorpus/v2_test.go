package kbcorpus

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlanItemsAndPromptCarryThePlan(t *testing.T) {
	plans, err := Generate(Spec{Seed: 21, Documents: 60})
	if err != nil {
		t.Fatal(err)
	}
	items := PlanItems(plans)
	if len(items) != len(plans) {
		t.Fatal("items and plans differ")
	}
	revisions := 0
	for index, item := range items {
		if item.DepartmentZH == "" || item.DocumentTypeZH == "" || item.ClassificationZH == "" {
			t.Fatalf("item %d lacks Chinese names: %+v", index, item)
		}
		if plans[index].Version > 1 {
			revisions++
			if len(item.Sections) != 1 || item.RevisionOf == "" {
				t.Fatalf("revision item %d = %+v", index, item)
			}
		} else if len(item.Sections) != len(plans[index].Sections) {
			t.Fatalf("item %d sections", index)
		}
	}
	if revisions == 0 {
		t.Fatal("plan has no revisions to exercise")
	}
	prompt, err := BatchPrompt(items[:3])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, items[0].DocID) || !strings.Contains(prompt, "只输出一个 JSON 数组") {
		t.Fatalf("prompt = %s", prompt[:200])
	}
}

func TestApplyGeneratedValidatesAgainstPlan(t *testing.T) {
	plans, _ := Generate(Spec{Seed: 21, Documents: 60})
	var first, second Document
	for index, plan := range plans {
		if plan.Version == 2 {
			first, second = plans[index-1], plan
			break
		}
	}
	long := strings.Repeat("本节内容按计划撰写，", 15) // 150 runes
	generated := GeneratedDocument{DocID: first.DocID, Version: 1, Title: "销售部渠道合作合同"}
	for index := range first.Sections {
		generated.Sections = append(generated.Sections, GeneratedSection{Index: index, Text: long})
	}
	applied, err := ApplyGenerated(first, generated, nil)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Title != generated.Title || len(applied.Sections) != len(first.Sections) || applied.Sections[0].Classification != first.Sections[0].Classification ||
		applied.Fields["contract_no"] != first.Fields["contract_no"] && first.DocumentType == "contract" {
		t.Fatalf("applied = %+v", applied)
	}
	fields, _, err := ParseFrontMatter(applied.Body)
	if err != nil || len(fields) != len(first.Fields) {
		t.Fatalf("front matter lost: %v %v", fields, err)
	}
	// Revision copies earlier sections and replaces the last.
	revision := GeneratedDocument{DocID: second.DocID, Version: 2, Title: generated.Title + "（修订）",
		Sections: []GeneratedSection{{Index: len(second.Sections) - 1, Text: strings.Repeat("修订后的最后一节内容，", 14)}}}
	appliedRevision, err := ApplyGenerated(second, revision, &applied)
	if err != nil {
		t.Fatal(err)
	}
	if appliedRevision.Sections[0].Text != applied.Sections[0].Text || appliedRevision.Sections[len(appliedRevision.Sections)-1].Text == applied.Sections[len(applied.Sections)-1].Text {
		t.Fatal("revision did not copy earlier sections or did not change the last")
	}
	bad := []struct {
		name string
		gen  GeneratedDocument
		prev *Document
		plan Document
	}{
		{"wrong id", GeneratedDocument{DocID: "D-999999", Version: 1, Title: generated.Title, Sections: generated.Sections}, nil, first},
		{"missing section", GeneratedDocument{DocID: first.DocID, Version: 1, Title: generated.Title, Sections: generated.Sections[:1]}, nil, first},
		{"short section", GeneratedDocument{DocID: first.DocID, Version: 1, Title: generated.Title, Sections: append([]GeneratedSection{{Index: 0, Text: "太短"}}, generated.Sections[1:]...)}, nil, first},
		{"structural marker", GeneratedDocument{DocID: first.DocID, Version: 1, Title: generated.Title, Sections: append([]GeneratedSection{{Index: 0, Text: long + " ## 新节"}}, generated.Sections[1:]...)}, nil, first},
		{"revision without previous", revision, nil, second},
		{"revision unchanged", GeneratedDocument{DocID: second.DocID, Version: 2, Title: generated.Title, Sections: []GeneratedSection{{Index: len(second.Sections) - 1, Text: long}}}, &applied, second},
	}
	for _, tc := range bad {
		if _, err := ApplyGenerated(tc.plan, tc.gen, tc.prev); err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
	}
	reply := "以下是结果：\n```json\n" + mustJSON(t, []GeneratedDocument{generated}) + "\n```"
	parsed, err := ParseBatchResponse(reply)
	if err != nil || len(parsed) != 1 || parsed[0].DocID != first.DocID {
		t.Fatalf("parse fenced reply: %v %v", parsed, err)
	}
	if _, err := ParseBatchResponse("no json here"); err == nil {
		t.Fatal("non-JSON reply accepted")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
