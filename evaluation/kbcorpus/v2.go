package kbcorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Corpus v2 keeps v1's deterministic metadata plan (identities, types,
// departments, classifications, roles, labels, dates, extractable fields,
// section plan) and replaces the templated prose with LLM-written Chinese
// text. The plan is reproducible; the prose is not, so the frozen artifact is
// the JSONL itself, digest-bound and mirrored.

const (
	VersionV2   = "taskgate-kb-corpus-v2"
	GeneratorV2 = "evaluation/cmd/kb-corpus-v2"
)

var departmentNamesZH = map[string]string{
	"sales": "销售部", "engineering": "工程部", "finance": "财务部", "hr": "人力资源部", "legal": "法务部", "marketing": "市场部",
	"operations": "运营部", "procurement": "采购部", "product": "产品部", "research": "研究院", "support": "客户支持部", "it": "信息技术部",
}

var documentTypeNamesZH = map[string]string{"contract": "合同", "approval": "审批单", "policy": "制度", "report": "报告"}

var classificationNamesZH = map[string]string{"public": "公开", "internal": "内部", "confidential": "机密", "leadership": "领导层专属"}

// GenerationItem is one document version the model must write. For a second
// version only the last section is requested; the earlier sections are copied
// from the first version in the same batch.
type GenerationItem struct {
	DocID            string            `json:"doc_id"`
	Version          int               `json:"version"`
	DocumentType     string            `json:"document_type"`
	DocumentTypeZH   string            `json:"document_type_zh"`
	Department       string            `json:"department"`
	DepartmentZH     string            `json:"department_zh"`
	Classification   string            `json:"classification"`
	ClassificationZH string            `json:"classification_zh"`
	Labels           []string          `json:"labels"`
	ValidFrom        string            `json:"valid_from"`
	ValidUntil       string            `json:"valid_until,omitempty"`
	Fields           map[string]string `json:"fields"`
	Sections         []SectionPlan     `json:"sections"`
	RevisionOf       string            `json:"revision_of,omitempty"`
}

type SectionPlan struct {
	Index            int    `json:"index"`
	Heading          string `json:"heading"`
	Classification   string `json:"classification"`
	ClassificationZH string `json:"classification_zh"`
}

// PlanItems turns v1 plan documents into generation items.
func PlanItems(plans []Document) []GenerationItem {
	items := make([]GenerationItem, 0, len(plans))
	for _, plan := range plans {
		item := GenerationItem{DocID: plan.DocID, Version: plan.Version, DocumentType: plan.DocumentType,
			DocumentTypeZH: documentTypeNamesZH[plan.DocumentType], Department: plan.Department, DepartmentZH: departmentNamesZH[plan.Department],
			Classification: plan.Classification, ClassificationZH: classificationNamesZH[plan.Classification],
			Labels: append([]string{}, plan.Labels...), ValidFrom: plan.ValidFrom, ValidUntil: plan.ValidUntil, Fields: plan.Fields}
		if plan.Version > 1 {
			item.RevisionOf = fmt.Sprintf("%s v%d", plan.DocID, plan.Version-1)
			last := plan.Sections[len(plan.Sections)-1]
			item.Sections = []SectionPlan{{Index: len(plan.Sections) - 1, Heading: last.Heading, Classification: last.Classification, ClassificationZH: classificationNamesZH[last.Classification]}}
		} else {
			for index, section := range plan.Sections {
				item.Sections = append(item.Sections, SectionPlan{Index: index, Heading: section.Heading, Classification: section.Classification, ClassificationZH: classificationNamesZH[section.Classification]})
			}
		}
		items = append(items, item)
	}
	return items
}

// BatchPrompt renders the instruction for one batch.
func BatchPrompt(items []GenerationItem) (string, error) {
	plan, err := json.MarshalIndent(items, "", " ")
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	out.WriteString(`你是一家中国企业的文档撰写者。下面是一批企业内部文档的元数据计划（JSON）。请为每一份文档写出真实感的中文正文。

严格要求：
1. 输出且只输出一个 JSON 数组，不要 Markdown 代码块、不要解释。数组元素与输入顺序一一对应，每个元素形如
   {"doc_id": "...", "version": 1, "title": "...", "sections": [{"index": 0, "text": "..."}, ...]}。
2. sections 的 index 必须与计划中的 index 完全一致、数量一致；不要新增或删除节；不要输出 heading 和 classification（计划已给定）。
3. 每节 text 为 150 到 400 个汉字的连贯段落，不含换行、不含 "## "。文风按文档类型：合同像合同条款，审批单像审批说明，制度像制度条文，报告像工作报告。
4. 正文必须自然地提到计划 fields 里的值（编号、金额、对方、日期等按原样出现，金额可带"元"）。
5. 密级为 leadership（领导层专属）的节要写成只有管理层才能看到的评估或决策内容；其他节不得泄露该节内容。
6. revision_of 非空的条目是修订版：只需写出计划中给出的那一节（最后一节），内容应是对原节的修订，其余节会从原版复制。
7. title 为 8 到 30 个汉字的标题，体现部门与主题；不同文档的标题与正文不要雷同。

元数据计划：
`)
	out.Write(plan)
	out.WriteString("\n")
	return out.String(), nil
}

// GeneratedDocument is the model's output for one item.
type GeneratedDocument struct {
	DocID    string             `json:"doc_id"`
	Version  int                `json:"version"`
	Title    string             `json:"title"`
	Sections []GeneratedSection `json:"sections"`
}

type GeneratedSection struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// ParseBatchResponse extracts the JSON array from a model reply, tolerating a
// code fence or surrounding prose.
func ParseBatchResponse(reply string) ([]GeneratedDocument, error) {
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start < 0 || end <= start {
		return nil, errors.New("reply contains no JSON array")
	}
	var generated []GeneratedDocument
	decoder := json.NewDecoder(strings.NewReader(reply[start : end+1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return nil, fmt.Errorf("decode generated documents: %w", err)
	}
	return generated, nil
}

const (
	minSectionRunes = 100
	maxSectionRunes = 600
)

// ApplyGenerated validates one generated document against its plan and
// returns the final document version. previous is the already-applied first
// version when plan.Version > 1.
func ApplyGenerated(plan Document, generated GeneratedDocument, previous *Document) (Document, error) {
	if generated.DocID != plan.DocID || generated.Version != plan.Version {
		return Document{}, fmt.Errorf("generated %s v%d does not match plan %s v%d", generated.DocID, generated.Version, plan.DocID, plan.Version)
	}
	title := strings.TrimSpace(generated.Title)
	if runes := utf8.RuneCountInString(title); runes < 4 || runes > 60 || strings.ContainsAny(title, "\n\r\t") {
		return Document{}, fmt.Errorf("%s v%d: title %q is not 4..60 runes on one line", plan.DocID, plan.Version, title)
	}
	doc := plan
	doc.Title = title
	doc.Labels = append([]string{}, plan.Labels...)
	doc.Sections = make([]Section, len(plan.Sections))
	if plan.Version > 1 {
		if previous == nil || previous.DocID != plan.DocID || previous.Version != plan.Version-1 || len(previous.Sections) != len(plan.Sections) {
			return Document{}, fmt.Errorf("%s v%d: revision needs the applied version %d", plan.DocID, plan.Version, plan.Version-1)
		}
		copy(doc.Sections, previous.Sections)
		if len(generated.Sections) != 1 || generated.Sections[0].Index != len(plan.Sections)-1 {
			return Document{}, fmt.Errorf("%s v%d: revision must carry exactly the last section", plan.DocID, plan.Version)
		}
	} else {
		if len(generated.Sections) != len(plan.Sections) {
			return Document{}, fmt.Errorf("%s v%d: %d sections generated, plan has %d", plan.DocID, plan.Version, len(generated.Sections), len(plan.Sections))
		}
		for index := range plan.Sections {
			doc.Sections[index] = Section{Heading: plan.Sections[index].Heading, Classification: plan.Sections[index].Classification}
		}
	}
	seen := map[int]struct{}{}
	for _, section := range generated.Sections {
		if section.Index < 0 || section.Index >= len(plan.Sections) {
			return Document{}, fmt.Errorf("%s v%d: section index %d out of range", plan.DocID, plan.Version, section.Index)
		}
		if _, duplicate := seen[section.Index]; duplicate {
			return Document{}, fmt.Errorf("%s v%d: section %d generated twice", plan.DocID, plan.Version, section.Index)
		}
		seen[section.Index] = struct{}{}
		text := strings.Join(strings.Fields(section.Text), " ")
		if runes := utf8.RuneCountInString(text); runes < minSectionRunes || runes > maxSectionRunes {
			return Document{}, fmt.Errorf("%s v%d: section %d has %d runes, want %d..%d", plan.DocID, plan.Version, section.Index, runes, minSectionRunes, maxSectionRunes)
		}
		if strings.Contains(text, SectionPrefix) || strings.Contains(text, FrontMatterDelimiter) {
			return Document{}, fmt.Errorf("%s v%d: section %d contains structural markers", plan.DocID, plan.Version, section.Index)
		}
		doc.Sections[section.Index] = Section{Heading: plan.Sections[section.Index].Heading, Classification: plan.Sections[section.Index].Classification, Text: text}
	}
	if plan.Version > 1 && doc.Sections[len(doc.Sections)-1].Text == previous.Sections[len(previous.Sections)-1].Text {
		return Document{}, fmt.Errorf("%s v%d: revision did not change the last section", plan.DocID, plan.Version)
	}
	doc.Body = RenderBody(doc)
	// Round trip through the parsers the publisher uses.
	if _, rest, err := ParseFrontMatter(doc.Body); err != nil {
		return Document{}, err
	} else if sections, err := ParseSections(rest); err != nil || len(sections) != len(doc.Sections) {
		return Document{}, fmt.Errorf("%s v%d: rendered body does not round-trip (%v)", plan.DocID, plan.Version, err)
	}
	return doc, nil
}

// ManifestV2 binds a generated corpus. The prose is not reproducible, so the
// manifest records how it was produced and what it cost.
type ManifestV2 struct {
	Version         string  `json:"version"`
	Generator       string  `json:"generator"`
	PlanSpec        Spec    `json:"plan_spec"`
	PlanSHA256      string  `json:"plan_sha256"`
	Model           string  `json:"model"`
	BatchSize       int     `json:"batch_size"`
	Batches         int     `json:"batches"`
	Retries         int     `json:"retries"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	Counts          Counts  `json:"counts"`
	DocumentsBytes  int64   `json:"documents_bytes"`
	DocumentsSHA256 string  `json:"documents_sha256"`
}
