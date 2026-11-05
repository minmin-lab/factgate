// Package kbqueryset builds and freezes the E-KB4 retrieval-quality query set
// (docs/kb_experiment_design.md): target documents sampled from the frozen
// corpus, stratified by department and document type, and two Chinese
// questions per document written by a model, one close to the document's
// wording and one paraphrase. The set is frozen with a manifest that binds
// the corpus digest, so recall@k is measured against a query set fixed
// before any retrieval run.
package kbqueryset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"unicode/utf8"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

const (
	Version        = "taskgate-kb-queryset-v2"
	KindVerbatim   = "verbatim"
	KindParaphrase = "paraphrase"

	minQueryRunes = 6
	maxQueryRunes = 60
	promptSection = 320 // runes of each section shown to the model
	promptMaxSecs = 3
)

// Target is one sampled document.
type Target struct {
	DocID          string `json:"doc_id"`
	Version        int    `json:"version"`
	Title          string `json:"title"`
	DocumentType   string `json:"document_type"`
	Department     string `json:"department"`
	Classification string `json:"classification"`
}

// Query is one frozen question.
type Query struct {
	QueryID        string `json:"query_id"`
	DocID          string `json:"doc_id"`
	Version        int    `json:"version"`
	Department     string `json:"department"`
	Classification string `json:"classification"`
	DocumentType   string `json:"document_type"`
	Kind           string `json:"kind"`
	Text           string `json:"text"`
}

// Sample picks targets deterministically: the latest version of each
// document, grouped by (department, document_type), and round-robin across
// the strata in a seeded order until count is reached.
func Sample(documents []kbcorpus.Document, count int, seed uint64) ([]Target, error) {
	if count <= 0 {
		return nil, errors.New("sample count must be positive")
	}
	latest := map[string]kbcorpus.Document{}
	for _, document := range documents {
		if prior, ok := latest[document.DocID]; !ok || document.Version > prior.Version {
			latest[document.DocID] = document
		}
	}
	strata := map[string][]kbcorpus.Document{}
	for _, document := range latest {
		key := document.Department + "/" + document.DocumentType
		strata[key] = append(strata[key], document)
	}
	keys := make([]string, 0, len(strata))
	for key := range strata {
		keys = append(keys, key)
		sort.Slice(strata[key], func(i, j int) bool { return strata[key][i].DocID < strata[key][j].DocID })
	}
	sort.Strings(keys)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for _, key := range keys {
		rng.Shuffle(len(strata[key]), func(i, j int) { strata[key][i], strata[key][j] = strata[key][j], strata[key][i] })
	}
	var targets []Target
	for round := 0; len(targets) < count; round++ {
		progressed := false
		for _, key := range keys {
			if round < len(strata[key]) && len(targets) < count {
				document := strata[key][round]
				targets = append(targets, Target{DocID: document.DocID, Version: document.Version, Title: document.Title,
					DocumentType: document.DocumentType, Department: document.Department, Classification: document.Classification})
				progressed = true
			}
		}
		if !progressed {
			return nil, fmt.Errorf("corpus holds only %d documents, fewer than %d targets", len(targets), count)
		}
	}
	return targets, nil
}

// promptItem is what the model sees for one target.
type promptItem struct {
	DocID        string   `json:"doc_id"`
	Version      int      `json:"version"`
	Title        string   `json:"title"`
	DocumentType string   `json:"document_type"`
	Department   string   `json:"department"`
	Excerpts     []string `json:"excerpts"`
}

// Prompt builds the batch prompt for the given targets from their documents.
func Prompt(targets []Target, documents map[string]kbcorpus.Document) (string, error) {
	items := make([]promptItem, 0, len(targets))
	for _, target := range targets {
		document, ok := documents[Key(target.DocID, target.Version)]
		if !ok {
			return "", fmt.Errorf("target %s v%d is not in the corpus", target.DocID, target.Version)
		}
		item := promptItem{DocID: target.DocID, Version: target.Version, Title: document.Title,
			DocumentType: document.DocumentType, Department: document.Department}
		for index, section := range document.Sections {
			if index >= promptMaxSecs {
				break
			}
			item.Excerpts = append(item.Excerpts, truncateRunes(section.Text, promptSection))
		}
		items = append(items, item)
	}
	encoded, err := json.MarshalIndent(items, "", " ")
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	out.WriteString(`你是一家中国企业的员工，正在用公司内部的文档检索系统找资料。下面是一批内部文档的标题与正文节选（JSON）。请为每一份文档写两个你会输入检索框的中文问题，两个问题都应当能通过这份文档得到回答：

严格要求：
1. 输出且只输出一个 JSON 数组，不要 Markdown 代码块、不要解释。数组元素与输入顺序一一对应，每个元素形如
   {"doc_id": "...", "version": 1, "verbatim": "...", "paraphrase": "..."}。
2. verbatim：贴近原文措辞的问题，直接使用节选里出现的关键词或短语（如具体的事项、制度名、对方名称、金额用途），8 到 40 个汉字。
3. paraphrase：同一个信息需求的改写，换用不同的说法、同义词或口语表达，不照抄节选中连续 6 个以上的字，8 到 40 个汉字。
4. 不要在问题里出现 doc_id、编号前缀或 "这份文档" 之类的指代；问题要像真实员工会问的那样具体。
5. 两个问题不得相同。

文档节选：
`)
	out.Write(encoded)
	out.WriteString("\n")
	return out.String(), nil
}

// Generated is the model's output for one target.
type Generated struct {
	DocID      string `json:"doc_id"`
	Version    int    `json:"version"`
	Verbatim   string `json:"verbatim"`
	Paraphrase string `json:"paraphrase"`
}

// ParseResponse extracts the JSON array from a model reply.
func ParseResponse(reply string) ([]Generated, error) {
	start := strings.Index(reply, "[")
	end := strings.LastIndex(reply, "]")
	if start < 0 || end <= start {
		return nil, errors.New("reply contains no JSON array")
	}
	var generated []Generated
	decoder := json.NewDecoder(strings.NewReader(reply[start : end+1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return nil, fmt.Errorf("decode generated queries: %w", err)
	}
	return generated, nil
}

// Apply validates one generated pair against its target and returns the two
// frozen queries.
func Apply(target Target, generated Generated) ([]Query, error) {
	if generated.DocID != target.DocID || generated.Version != target.Version {
		return nil, fmt.Errorf("generated pair names %s v%d, target is %s v%d", generated.DocID, generated.Version, target.DocID, target.Version)
	}
	var queries []Query
	for _, pair := range []struct{ kind, text string }{{KindVerbatim, generated.Verbatim}, {KindParaphrase, generated.Paraphrase}} {
		text := strings.TrimSpace(pair.text)
		runes := utf8.RuneCountInString(text)
		if runes < minQueryRunes || runes > maxQueryRunes {
			return nil, fmt.Errorf("%s %s v%d: %q has %d runes, want %d..%d", pair.kind, target.DocID, target.Version, text, runes, minQueryRunes, maxQueryRunes)
		}
		if strings.ContainsAny(text, "\n\r\t") || strings.Contains(text, target.DocID) || strings.Contains(text, "这份文档") {
			return nil, fmt.Errorf("%s %s v%d: %q is not a plain question", pair.kind, target.DocID, target.Version, text)
		}
		queries = append(queries, Query{QueryID: fmt.Sprintf("%s-v%d-%s", target.DocID, target.Version, pair.kind), DocID: target.DocID, Version: target.Version,
			Department: target.Department, Classification: target.Classification, DocumentType: target.DocumentType, Kind: pair.kind, Text: text})
	}
	if queries[0].Text == queries[1].Text {
		return nil, fmt.Errorf("%s v%d: verbatim and paraphrase are identical", target.DocID, target.Version)
	}
	return queries, nil
}

// Manifest binds the frozen query set.
type Manifest struct {
	Version       string         `json:"version"`
	Generator     string         `json:"generator"`
	Model         string         `json:"model"`
	CorpusSHA256  string         `json:"corpus_sha256"`
	Seed          uint64         `json:"seed"`
	Targets       int            `json:"targets"`
	Queries       int            `json:"queries"`
	ByKind        map[string]int `json:"by_kind"`
	ByType        map[string]int `json:"by_document_type"`
	ByDepartment  map[string]int `json:"by_department"`
	TotalCostUSD  float64        `json:"total_cost_usd"`
	QueriesBytes  int            `json:"queries_bytes"`
	QueriesSHA256 string         `json:"queries_sha256"`
	TargetsSHA256 string         `json:"targets_sha256"`
}

// Encode writes the queries as JSONL in a canonical order and returns the
// bytes with their digest.
func Encode(queries []Query) ([]byte, string, error) {
	sorted := append([]Query(nil), queries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].QueryID < sorted[j].QueryID })
	var out bytes.Buffer
	for _, query := range sorted {
		encoded, err := json.Marshal(query)
		if err != nil {
			return nil, "", err
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	sum := sha256.Sum256(out.Bytes())
	return out.Bytes(), hex.EncodeToString(sum[:]), nil
}

// Summarize fills the manifest counts from the queries.
func Summarize(manifest Manifest, queries []Query) Manifest {
	manifest.Queries = len(queries)
	manifest.ByKind, manifest.ByType, manifest.ByDepartment = map[string]int{}, map[string]int{}, map[string]int{}
	for _, query := range queries {
		manifest.ByKind[query.Kind]++
		manifest.ByType[query.DocumentType]++
		manifest.ByDepartment[query.Department]++
	}
	return manifest
}

// Key names one document version.
func Key(docID string, version int) string { return fmt.Sprintf("%s v%d", docID, version) }

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit]) + "…"
}
