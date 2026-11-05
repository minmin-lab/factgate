package kbcorpus

import (
	"bytes"
	"strings"
	"testing"
)

const v3TestSeed = 20261015

func TestPlanV3GradesRolesAndVersions(t *testing.T) {
	spec := Spec{Seed: v3TestSeed, Documents: 200}
	plans, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := PlanV3(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != len(plans) {
		t.Fatalf("PlanV3 returned %d versions, the v1 plan has %d", len(documents), len(plans))
	}
	again, err := PlanV3(spec)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := EncodeJSONL(documents)
	second, _ := EncodeJSONL(again)
	if !bytes.Equal(first, second) {
		t.Fatal("two PlanV3 runs of the same spec differ")
	}

	byGrade := map[string]int{}
	type docState struct {
		classification string
		jobRole        string
		sections       string
	}
	seen := map[string]docState{}
	// The secret:confidential split is per document: count each v1
	// confidential doc_id once, not once per version.
	splitSecret, splitConfidential := map[string]struct{}{}, map[string]struct{}{}
	for index, doc := range documents {
		plan := plans[index]
		if doc.DocID != plan.DocID || doc.Version != plan.Version || doc.Title != plan.Title || doc.ValidFrom != plan.ValidFrom ||
			len(doc.Sections) != len(plan.Sections) || len(doc.Fields) != len(plan.Fields) {
			t.Fatalf("%s v%d: v3 changed a planned attribute other than grade, role, headings or body", doc.DocID, doc.Version)
		}
		top := -1
		var signature strings.Builder
		for sectionIndex, section := range doc.Sections {
			v1 := plan.Sections[sectionIndex]
			if section.Text != v1.Text {
				t.Fatalf("%s v%d section %d text changed", doc.DocID, doc.Version, sectionIndex)
			}
			switch v1.Classification {
			case ClassificationPublic, ClassificationInternal:
				if section.Classification != v1.Classification {
					t.Fatalf("%s: %s section became %s", doc.DocID, v1.Classification, section.Classification)
				}
			case ClassificationLeadership:
				if section.Classification != ClassificationTopSecret {
					t.Fatalf("%s: leadership section became %s", doc.DocID, section.Classification)
				}
			case ClassificationConfidential:
				switch section.Classification {
				case ClassificationSecret:
					splitSecret[doc.DocID] = struct{}{}
				case ClassificationConfidential:
					splitConfidential[doc.DocID] = struct{}{}
				default:
					t.Fatalf("%s: confidential section became %s", doc.DocID, section.Classification)
				}
			default:
				t.Fatalf("%s: unexpected v1 grade %q", doc.DocID, v1.Classification)
			}
			rank := ClassificationRankV3(section.Classification)
			if rank < 0 {
				t.Fatalf("%s: section grade %q is not a v3 grade", doc.DocID, section.Classification)
			}
			top = max(top, rank)
			signature.WriteString(section.Heading + "[" + section.Classification + "];")
		}
		if doc.Classification != ClassificationsV3[top] {
			t.Fatalf("%s v%d: document grade %s, highest section grade %s", doc.DocID, doc.Version, doc.Classification, ClassificationsV3[top])
		}
		if !contains(rolesByClassificationV3[doc.Classification], doc.JobRole) {
			t.Fatalf("%s: job_role %s not drawn from the %s table", doc.DocID, doc.JobRole, doc.Classification)
		}
		state := docState{doc.Classification, doc.JobRole, signature.String()}
		if previous, ok := seen[doc.DocID]; ok {
			if previous != state {
				t.Fatalf("%s v%d: grade, role or section grades differ from the earlier version: %+v vs %+v", doc.DocID, doc.Version, state, previous)
			}
		} else {
			seen[doc.DocID] = state
			byGrade[doc.Classification]++
		}
	}
	revisions := len(documents) - len(seen)
	if revisions == 0 {
		t.Fatal("the plan has no second version to compare")
	}
	for _, grade := range ClassificationsV3 {
		if byGrade[grade] == 0 {
			t.Fatalf("grade %s absent from the document grades %v", grade, byGrade)
		}
	}
	for grade := range byGrade {
		if ClassificationRankV3(grade) < 0 {
			t.Fatalf("document grade %q is not a v3 grade", grade)
		}
	}
	for docID := range splitSecret {
		if _, both := splitConfidential[docID]; both {
			t.Fatalf("%s: v1 confidential sections split into both secret and confidential", docID)
		}
	}
	split := len(splitSecret) + len(splitConfidential)
	share := float64(len(splitSecret)) / float64(split)
	t.Logf("documents=%d versions=%d grades=%v; v1 confidential docs=%d secret=%d confidential=%d (secret share %.3f)",
		len(seen), len(documents), byGrade, split, len(splitSecret), len(splitConfidential), share)
	if share < 0.45 || share > 0.75 {
		t.Fatalf("secret share of v1 confidential documents = %.3f, want 0.60 +- 0.15", share)
	}
}

// TestPlanV3DrawMatchesSpecifiedHash pins the draw to values computed outside
// Go: printf '%s' "20261015|D-00000N" | sha256sum, first 16 hex digits mod 100.
func TestPlanV3DrawMatchesSpecifiedHash(t *testing.T) {
	for docID, want := range map[string]uint64{"D-000001": 61, "D-000002": 87, "D-000003": 93, "D-000004": 66, "D-000005": 22, "D-000006": 44} {
		if got := v3Draw(v3TestSeed, docID, "") % 100; got != want {
			t.Fatalf("%s: draw mod 100 = %d, want %d", docID, got, want)
		}
		grade, err := v3SectionClassification(v3TestSeed, docID, ClassificationConfidential)
		if err != nil {
			t.Fatal(err)
		}
		if wantGrade := map[bool]string{true: ClassificationSecret, false: ClassificationConfidential}[want < 60]; grade != wantGrade {
			t.Fatalf("%s: confidential maps to %s, want %s", docID, grade, wantGrade)
		}
	}
	if _, err := v3SectionClassification(v3TestSeed, "D-000001", "restricted"); err == nil {
		t.Fatal("unknown v1 grade accepted")
	}
}

func TestPlanV3BodiesRoundTrip(t *testing.T) {
	documents, err := PlanV3(Spec{Seed: v3TestSeed, Documents: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range documents {
		fields, rest, err := ParseFrontMatter(doc.Body)
		if err != nil {
			t.Fatalf("%s v%d: %v", doc.DocID, doc.Version, err)
		}
		if len(fields) != len(doc.Fields) {
			t.Fatalf("%s v%d: front matter has %d fields, struct %d", doc.DocID, doc.Version, len(fields), len(doc.Fields))
		}
		for name, value := range fields {
			if doc.Fields[name] != value {
				t.Fatalf("%s v%d: field %s body=%q struct=%q", doc.DocID, doc.Version, name, value, doc.Fields[name])
			}
		}
		sections, err := ParseSections(rest)
		if err != nil {
			t.Fatalf("%s v%d: %v", doc.DocID, doc.Version, err)
		}
		if len(sections) != len(doc.Sections) {
			t.Fatalf("%s v%d: parsed %d sections, struct has %d", doc.DocID, doc.Version, len(sections), len(doc.Sections))
		}
		for index, section := range sections {
			if section != doc.Sections[index] {
				t.Fatalf("%s v%d section %d differs after round trip:\n%+v\n%+v", doc.DocID, doc.Version, index, section, doc.Sections[index])
			}
			if !strings.Contains(doc.Body, SectionPrefix+section.Heading+" ["+section.Classification+"]\n") {
				t.Fatalf("%s v%d: body lacks heading line for section %d", doc.DocID, doc.Version, index)
			}
		}
	}
}

func TestSectionHeadingsZHCoverV1(t *testing.T) {
	headings := map[string]struct{}{"Leadership assessment": {}}
	for _, heading := range sectionHeadings {
		headings[heading] = struct{}{}
	}
	// Also every heading the v1 generator actually emits, in case a heading
	// literal is added outside sectionHeadings.
	plans, err := Generate(Spec{Seed: v3TestSeed, Documents: 2000})
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		for _, section := range plan.Sections {
			headings[section.Heading] = struct{}{}
		}
	}
	t.Logf("%d v1 headings checked", len(headings))
	chinese := map[string]string{}
	for heading := range headings {
		zh, ok := sectionHeadingsZH[heading]
		if !ok {
			t.Fatalf("v1 heading %q has no Chinese heading", heading)
		}
		if zh == "" || strings.ContainsAny(zh, "[]\n") || strings.Contains(zh, SectionPrefix) {
			t.Fatalf("Chinese heading %q for %q would break ParseSections", zh, heading)
		}
		if other, duplicate := chinese[zh]; duplicate {
			t.Fatalf("%q and %q share the Chinese heading %q", other, heading, zh)
		}
		chinese[zh] = heading
	}
	if sectionHeadingsZH["Purpose"] != "目的" || sectionHeadingsZH["Scope"] != "范围" || sectionHeadingsZH["Leadership assessment"] != "管理层评估" {
		t.Fatalf("specified headings changed: %v", sectionHeadingsZH)
	}
}

func TestChineseNamesAndPromptV3(t *testing.T) {
	// PlanItems falls back to the raw value, so this test, not v2's empty
	// check, guards that every vocabulary value has a Chinese name.
	for _, table := range []struct {
		name   string
		values []string
		names  map[string]string
	}{{"department", Departments, departmentNamesZH}, {"document type", DocumentTypes, documentTypeNamesZH},
		{"v1 grade", Classifications, classificationNamesZH}, {"v3 grade", ClassificationsV3, classificationNamesZH}} {
		for _, value := range table.values {
			if table.names[value] == "" {
				t.Fatalf("%s %q has no Chinese name", table.name, value)
			}
		}
	}
	if nameZH(classificationNamesZH, "restricted") != "restricted" {
		t.Fatal("an unnamed value does not fall back to itself")
	}
	documents, err := PlanV3(Spec{Seed: v3TestSeed, Documents: 200})
	if err != nil {
		t.Fatal(err)
	}
	items := PlanItems(documents)
	names := map[string]bool{}
	for _, item := range items {
		names[item.ClassificationZH] = true
		for _, section := range item.Sections {
			if section.ClassificationZH != classificationNamesZH[section.Classification] {
				t.Fatalf("%s: section grade %s named %q", item.DocID, section.Classification, section.ClassificationZH)
			}
		}
	}
	for _, want := range []string{"公开", "内部", "秘密", "机密", "绝密"} {
		if !names[want] {
			t.Fatalf("v3 items lack grade name %s: %v", want, names)
		}
	}
	promptV3, err := BatchPromptV3(items[:10])
	if err != nil {
		t.Fatal(err)
	}
	promptV2, err := BatchPrompt(items[:10])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"top_secret（绝密）", "8. 正文不得写出与本节密级不符的密级名", "\n7. ", items[0].DocID} {
		if !strings.Contains(promptV3, want) {
			t.Fatalf("v3 prompt lacks %q", want)
		}
	}
	if strings.Contains(promptV3, "leadership（领导层专属）") || !strings.Contains(promptV2, "5. 密级为 leadership（领导层专属）") ||
		strings.Contains(promptV2, "8. 正文不得") {
		t.Fatal("v2 and v3 prompts carry the wrong grade rules")
	}
}

func TestApplyGeneratedV3RejectsForeignGradeNames(t *testing.T) {
	spec := Spec{Seed: v3TestSeed, Documents: 200}
	v3, err := PlanV3(spec)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Generate(spec)
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("本节按计划撰写的正文内容，", 12) // 144 runes
	// find returns the first v1 plan whose sections all carry grade, plus its
	// second version when there is one.
	find := func(plans []Document, grade string, wantRevision bool) (Document, *Document) {
		for index, plan := range plans {
			if plan.Version != 1 {
				continue
			}
			all := true
			for _, section := range plan.Sections {
				all = all && section.Classification == grade
			}
			hasRevision := index+1 < len(plans) && plans[index+1].DocID == plan.DocID
			if all && (!wantRevision || hasRevision) {
				if hasRevision {
					return plan, &plans[index+1]
				}
				return plan, nil
			}
		}
		t.Fatalf("no v1 plan with every section %s (revision %v)", grade, wantRevision)
		return Document{}, nil
	}
	// write gives every section the filler and section 0 the filler plus word.
	write := func(plan Document, word string) GeneratedDocument {
		generated := GeneratedDocument{DocID: plan.DocID, Version: plan.Version, Title: "密级校验测试文档"}
		for index := range plan.Sections {
			text := filler
			if index == 0 {
				text = filler + word + "。"
			}
			generated.Sections = append(generated.Sections, GeneratedSection{Index: index, Text: text})
		}
		return generated
	}

	secretPlan, secretRevision := find(v3, ClassificationSecret, true)
	if _, err := ApplyGeneratedV3(secretPlan, write(secretPlan, "机密"), nil); err == nil || !strings.Contains(err.Error(), "names the grade 机密") {
		t.Fatalf("v3 secret section containing 机密 accepted (err %v)", err)
	}
	if _, err := ApplyGenerated(secretPlan, write(secretPlan, "机密"), nil); err != nil {
		t.Fatalf("ApplyGenerated (v2 path) must not check grade names: %v", err)
	}
	applied, err := ApplyGeneratedV3(secretPlan, write(secretPlan, "秘密"), nil)
	if err != nil {
		t.Fatalf("v3 secret section containing its own grade 秘密 rejected: %v", err)
	}
	// A revision's regenerated last section is checked too.
	revision := GeneratedDocument{DocID: secretRevision.DocID, Version: 2, Title: "密级校验测试文档（修订）",
		Sections: []GeneratedSection{{Index: len(secretRevision.Sections) - 1, Text: "修订：" + filler + "绝密。"}}}
	if _, err := ApplyGeneratedV3(*secretRevision, revision, &applied); err == nil {
		t.Fatal("v3 revision whose secret section contains 绝密 accepted")
	}
	revision.Sections[0].Text = "修订：" + filler + "秘密。"
	if _, err := ApplyGeneratedV3(*secretRevision, revision, &applied); err != nil {
		t.Fatalf("v3 revision naming its own grade rejected: %v", err)
	}

	publicPlan, _ := find(v3, ClassificationPublic, false)
	for _, word := range []string{"秘密", "机密", "绝密"} {
		if _, err := ApplyGeneratedV3(publicPlan, write(publicPlan, word), nil); err == nil {
			t.Fatalf("v3 public section containing %s accepted", word)
		}
	}
	if _, err := ApplyGeneratedV3(publicPlan, write(publicPlan, "公开发布的内部通知"), nil); err != nil {
		t.Fatalf("公开/内部 must not be checked: %v", err)
	}
	topPlan, _ := find(v3, ClassificationTopSecret, false)
	if _, err := ApplyGeneratedV3(topPlan, write(topPlan, "绝密"), nil); err != nil {
		t.Fatalf("v3 top_secret section containing 绝密 rejected: %v", err)
	}
	if _, err := ApplyGeneratedV3(topPlan, write(topPlan, "秘密"), nil); err == nil {
		t.Fatal("v3 top_secret section containing 秘密 accepted")
	}

	// v2 plan: grade names are not checked, whatever the section grade.
	confidentialV2, _ := find(v2, ClassificationConfidential, false)
	for _, word := range []string{"机密", "绝密", "秘密"} {
		if _, err := ApplyGenerated(confidentialV2, write(confidentialV2, word), nil); err != nil {
			t.Fatalf("v2 confidential section containing %s rejected: %v", word, err)
		}
	}
}
