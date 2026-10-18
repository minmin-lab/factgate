package kbformal

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
)

// The frozen keyword set of E-KB6..E-KB8 (docs/kb_experiment_design.md,
// 关键词检索实验, 共同设定). Its rules are fixed before any keyword experiment
// runs; the generator is deterministic, so the set is reproducible from the
// corpora and the E-KB4 query-set targets.
const (
	KeywordsetVersion     = "taskgate-kb-keywordset-v1"
	KeywordClassAnchor    = "anchor"
	KeywordClassFrequent  = "frequent"
	KeywordClassAbsent    = "absent"
	keywordAnchorRunes    = 4
	keywordFrequentCount  = 100
	keywordAbsentCount    = 50
	keywordAbsentRunes    = 4
	keywordAbsentSeedHigh = 20261018
	keywordAbsentSeedLow  = 11
	cjkFirst, cjkLast     = 0x4E00, 0x9FFF
)

// KeywordQuery is one frozen keyword query.
type KeywordQuery struct {
	KeywordID    string `json:"keyword_id"`
	Class        string `json:"class"`
	Keyword      string `json:"keyword"`
	DocID        string `json:"doc_id,omitempty"`
	Version      int    `json:"version,omitempty"`
	Department   string `json:"department,omitempty"`
	DocumentType string `json:"document_type,omitempty"`
	// Chunks is, for a frequent keyword, the number of chunks of the source
	// corpus that contain it.
	Chunks int `json:"chunks,omitempty"`
}

// KeywordsetManifest binds the keyword file to its inputs and rules.
type KeywordsetManifest struct {
	Version              string         `json:"version"`
	Generator            string         `json:"generator"`
	Rules                []string       `json:"rules"`
	SourceCorpusSHA256   string         `json:"source_corpus_sha256"`
	AbsentCorporaSHA256  []string       `json:"absent_checked_corpora_sha256"`
	QuerysetTargetSHA256 string         `json:"queryset_targets_sha256"`
	WindowChars          int            `json:"window_chars"`
	AbsentSeed           [2]uint64      `json:"absent_seed"`
	Counts               map[string]int `json:"counts"`
	KeywordsSHA256       string         `json:"keywords_sha256"`
}

// keywordsetRules is the rule text the manifest carries, matching the
// pre-registration.
var keywordsetRules = []string{
	"anchor: for each E-KB4 target document, segment it with the publication's function (section-window-v1, window_chars), take chunk 0, and from its rune midpoint forward take the first window of 4 runes none of which is whitespace or punctuation (if none, the first such window from the start); the target is any chunk of the document",
	"frequent: the 100 two-rune strings of CJK unified ideographs (U+4E00..U+9FFF) contained in the most chunks of the source corpus, ties by code points",
	"absent: 4-rune strings drawn uniformly from U+4E00..U+9FFF with PCG(20261018, 11), keeping the first 50 that occur in no chunk of any listed corpus",
}

// KeywordTarget is one E-KB4 query-set target document.
type KeywordTarget struct {
	DocID        string `json:"doc_id"`
	Version      int    `json:"version"`
	DocumentType string `json:"document_type"`
	Department   string `json:"department"`
}

// LoadCorpusChunks reads a corpus JSONL file and segments every document with
// the publication's function. It returns the documents by identity, every
// chunk text, and the file's sha256.
func LoadCorpusChunks(path string, windowChars int) (map[string]kbcorpus.Document, []string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", err
	}
	sum := sha256.Sum256(raw)
	documents, err := kbcorpus.DecodeJSONL(raw)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s: %w", path, err)
	}
	byID := make(map[string]kbcorpus.Document, len(documents))
	var texts []string
	for _, doc := range documents {
		byID[documentKey(doc.DocID, doc.Version)] = doc
		chunks, err := kbpublish.Segment(doc, windowChars)
		if err != nil {
			return nil, nil, "", err
		}
		for _, chunk := range chunks {
			texts = append(texts, chunk.Text)
		}
	}
	return byID, texts, hex.EncodeToString(sum[:]), nil
}

func documentKey(docID string, version int) string { return fmt.Sprintf("%s\x00%d", docID, version) }

// GenerateKeywordset applies the frozen rules. source is the corpus the
// anchors and frequent keywords come from; absentCorpora are the chunk texts
// an absent keyword must not occur in (the source included).
func GenerateKeywordset(source map[string]kbcorpus.Document, sourceTexts []string, absentCorpora [][]string, targets []KeywordTarget, windowChars int) ([]KeywordQuery, error) {
	var out []KeywordQuery
	for _, target := range targets {
		doc, ok := source[documentKey(target.DocID, target.Version)]
		if !ok {
			return nil, fmt.Errorf("target %s v%d is not in the source corpus", target.DocID, target.Version)
		}
		chunks, err := kbpublish.Segment(doc, windowChars)
		if err != nil || len(chunks) == 0 {
			return nil, fmt.Errorf("segment target %s v%d: %v", target.DocID, target.Version, err)
		}
		keyword, err := anchorWindow(chunks[0].Text)
		if err != nil {
			return nil, fmt.Errorf("target %s v%d: %w", target.DocID, target.Version, err)
		}
		out = append(out, KeywordQuery{KeywordID: fmt.Sprintf("A-%s-v%d", target.DocID, target.Version), Class: KeywordClassAnchor,
			Keyword: keyword, DocID: target.DocID, Version: target.Version, Department: target.Department, DocumentType: target.DocumentType})
	}
	for rank, frequent := range frequentBigrams(sourceTexts, keywordFrequentCount) {
		out = append(out, KeywordQuery{KeywordID: fmt.Sprintf("F-%03d", rank+1), Class: KeywordClassFrequent, Keyword: frequent.text, Chunks: frequent.chunks})
	}
	absent, err := absentKeywords(absentCorpora, keywordAbsentCount)
	if err != nil {
		return nil, err
	}
	for index, keyword := range absent {
		out = append(out, KeywordQuery{KeywordID: fmt.Sprintf("X-%03d", index+1), Class: KeywordClassAbsent, Keyword: keyword})
	}
	return out, nil
}

func anchorWindow(text string) (string, error) {
	runes := []rune(text)
	usable := func(r rune) bool { return !unicode.IsSpace(r) && !unicode.IsPunct(r) }
	find := func(from, to int) (string, bool) {
		for i := from; i+keywordAnchorRunes <= to; i++ {
			ok := true
			for _, r := range runes[i : i+keywordAnchorRunes] {
				if !usable(r) {
					ok = false
					break
				}
			}
			if ok {
				return string(runes[i : i+keywordAnchorRunes]), true
			}
		}
		return "", false
	}
	if window, ok := find(len(runes)/2, len(runes)); ok {
		return window, nil
	}
	if window, ok := find(0, len(runes)); ok {
		return window, nil
	}
	return "", errors.New("chunk 0 has no window of 4 non-space, non-punctuation runes")
}

func isCJK(r rune) bool { return r >= cjkFirst && r <= cjkLast }

type bigramCount struct {
	text   string
	chunks int
}

func frequentBigrams(texts []string, limit int) []bigramCount {
	counts := map[string]int{}
	for _, text := range texts {
		runes := []rune(text)
		seen := map[string]struct{}{}
		for i := 0; i+1 < len(runes); i++ {
			if !isCJK(runes[i]) || !isCJK(runes[i+1]) {
				continue
			}
			bigram := string(runes[i : i+2])
			if _, dup := seen[bigram]; dup {
				continue
			}
			seen[bigram] = struct{}{}
			counts[bigram]++
		}
	}
	all := make([]bigramCount, 0, len(counts))
	for text, chunks := range counts {
		all = append(all, bigramCount{text, chunks})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].chunks != all[j].chunks {
			return all[i].chunks > all[j].chunks
		}
		return all[i].text < all[j].text
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

func absentKeywords(corpora [][]string, limit int) ([]string, error) {
	rng := rand.New(rand.NewPCG(keywordAbsentSeedHigh, keywordAbsentSeedLow))
	var out []string
	seen := map[string]struct{}{}
	for attempts := 0; len(out) < limit; attempts++ {
		if attempts > 100*limit {
			return nil, errors.New("could not draw enough absent keywords")
		}
		runes := make([]rune, keywordAbsentRunes)
		for i := range runes {
			runes[i] = rune(cjkFirst + rng.IntN(cjkLast-cjkFirst+1))
		}
		candidate := string(runes)
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		present := false
		for _, texts := range corpora {
			for _, text := range texts {
				if strings.Contains(text, candidate) {
					present = true
					break
				}
			}
			if present {
				break
			}
		}
		if !present {
			out = append(out, candidate)
		}
	}
	return out, nil
}

// WriteKeywordset writes keywords.jsonl and manifest.json into dir.
func WriteKeywordset(dir string, keywords []KeywordQuery, manifest KeywordsetManifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, keyword := range keywords {
		line, err := json.Marshal(keyword)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	data := []byte(b.String())
	sum := sha256.Sum256(data)
	manifest.Version, manifest.Rules = KeywordsetVersion, keywordsetRules
	manifest.KeywordsSHA256 = hex.EncodeToString(sum[:])
	manifest.Counts = map[string]int{}
	for _, keyword := range keywords {
		manifest.Counts[keyword.Class]++
	}
	manifest.AbsentSeed = [2]uint64{keywordAbsentSeedHigh, keywordAbsentSeedLow}
	if err := os.WriteFile(filepath.Join(dir, "keywords.jsonl"), data, 0o644); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(encoded, '\n'), 0o644)
}

// LoadKeywordset reads a frozen keyword set and checks it against its manifest.
func LoadKeywordset(dir string) ([]KeywordQuery, KeywordsetManifest, error) {
	var manifest KeywordsetManifest
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, manifest, err
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, manifest, err
	}
	if manifest.Version != KeywordsetVersion {
		return nil, manifest, fmt.Errorf("keyword set version %q, want %q", manifest.Version, KeywordsetVersion)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "keywords.jsonl"))
	if err != nil {
		return nil, manifest, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != manifest.KeywordsSHA256 {
		return nil, manifest, errors.New("keywords.jsonl sha256 does not match the frozen manifest")
	}
	var keywords []KeywordQuery
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var keyword KeywordQuery
		if err := json.Unmarshal(scanner.Bytes(), &keyword); err != nil {
			return nil, manifest, err
		}
		keywords = append(keywords, keyword)
	}
	return keywords, manifest, scanner.Err()
}
