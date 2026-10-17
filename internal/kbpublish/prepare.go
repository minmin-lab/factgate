package kbpublish

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

// Chunk is one deterministic segment of a document version with the
// attributes it is filtered and accounted by.
type Chunk struct {
	DocID          string
	Version        int
	ChunkIndex     int
	Text           string
	TextSHA256     string
	Embedding      []float32
	Department     string
	Classification string
	JobRole        string
	Labels         []string
	ValidUntil     string
}

// Value is one extracted content fact of a document version.
type Value struct {
	DocID          string
	Version        int
	DocumentType   string
	Field          string
	ValueType      string
	CanonicalValue string
	// Raw is the front-matter text; the loader casts it to the typed column
	// selected by ValueType (value_text, value_numeric or value_date).
	Raw            string
	Department     string
	Classification string
}

// Prepared is the frozen content of one collection before it is loaded.
type Prepared struct {
	Documents []kbcorpus.Document
	Chunks    []Chunk
	Values    []Value
	Labels    []string
}

// Segment applies section-window-v1: split on sections, then pack whole
// sentences into windows of at most windowChars runes. A chunk inherits the
// classification of its section and every other attribute of its document.
func Segment(doc kbcorpus.Document, windowChars int) ([]Chunk, error) {
	_, rest, err := kbcorpus.ParseFrontMatter(doc.Body)
	if err != nil {
		return nil, fmt.Errorf("%s v%d: %w", doc.DocID, doc.Version, err)
	}
	sections, err := kbcorpus.ParseSections(rest)
	if err != nil {
		return nil, fmt.Errorf("%s v%d: %w", doc.DocID, doc.Version, err)
	}
	var chunks []Chunk
	for _, section := range sections {
		for _, window := range windows(section.Text, windowChars) {
			sum := sha256.Sum256([]byte(window))
			chunks = append(chunks, Chunk{
				DocID: doc.DocID, Version: doc.Version, ChunkIndex: len(chunks), Text: window,
				TextSHA256: fmt.Sprintf("%x", sum), Department: doc.Department,
				Classification: section.Classification, JobRole: doc.JobRole,
				Labels: append([]string(nil), doc.Labels...), ValidUntil: doc.ValidUntil,
			})
		}
	}
	if len(chunks) == 0 {
		return nil, fmt.Errorf("%s v%d: no chunks", doc.DocID, doc.Version)
	}
	return chunks, nil
}

func windows(text string, limit int) []string {
	sentences := splitSentences(text)
	var out []string
	var current strings.Builder
	currentRunes := 0
	for _, sentence := range sentences {
		runes := utf8.RuneCountInString(sentence)
		if currentRunes > 0 && currentRunes+1+runes > limit {
			out = append(out, current.String())
			current.Reset()
			currentRunes = 0
		}
		if currentRunes > 0 {
			current.WriteByte(' ')
			currentRunes++
		}
		current.WriteString(sentence)
		currentRunes += runes
	}
	if currentRunes > 0 {
		out = append(out, current.String())
	}
	return out
}

func splitSentences(text string) []string {
	var sentences []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '.' && (index+1 == len(text) || text[index+1] == ' ') {
			sentence := strings.TrimSpace(text[start : index+1])
			if sentence != "" {
				sentences = append(sentences, sentence)
			}
			start = index + 1
		}
	}
	if tail := strings.TrimSpace(text[start:]); tail != "" {
		sentences = append(sentences, tail)
	}
	return sentences
}

// Embed is hash-projection-v1: signed feature hashing of lowercase word
// unigrams into dimension buckets, L2-normalized. It is a deterministic,
// dependency-free stand-in for a learned model; the experiments measure ledger
// behaviour, not retrieval quality.
func Embed(text string, dimension int) []float32 {
	vector := make([]float32, dimension)
	for _, token := range tokens(text) {
		sum := sha256.Sum256([]byte(EmbeddingHashProjectionV1 + "\x00" + token))
		bucket := binary.BigEndian.Uint32(sum[:4]) % uint32(dimension)
		if sum[4]&1 == 1 {
			vector[bucket]++
		} else {
			vector[bucket]--
		}
	}
	var norm float64
	for _, component := range vector {
		norm += float64(component) * float64(component)
	}
	if norm > 0 {
		scale := float32(1 / math.Sqrt(norm))
		for index := range vector {
			vector[index] *= scale
		}
	}
	return vector
}

func tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// VectorLiteral renders an embedding as a pgvector text literal with a fixed
// precision, so the stored vector is a function of the text alone.
func VectorLiteral(vector []float32) string {
	parts := make([]string, len(vector))
	for index, component := range vector {
		parts[index] = strconv.FormatFloat(float64(component), 'f', 6, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// Extract applies the front-matter extraction rules of the document's type and
// canonicalizes each value with the exposure encoding of its declared type.
func Extract(doc kbcorpus.Document, rules []ExtractionRule) ([]Value, error) {
	fields, _, err := kbcorpus.ParseFrontMatter(doc.Body)
	if err != nil {
		return nil, fmt.Errorf("%s v%d: %w", doc.DocID, doc.Version, err)
	}
	var values []Value
	for _, rule := range rules {
		if rule.DocumentType != doc.DocumentType {
			continue
		}
		raw, ok := fields[rule.Field]
		if !ok {
			return nil, fmt.Errorf("%s v%d: front matter lacks field %q required by an extraction rule", doc.DocID, doc.Version, rule.Field)
		}
		canonical, err := canonicalValue(rule.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("%s v%d field %s: %w", doc.DocID, doc.Version, rule.Field, err)
		}
		values = append(values, Value{
			DocID: doc.DocID, Version: doc.Version, DocumentType: doc.DocumentType, Field: rule.Field,
			ValueType: rule.Type, CanonicalValue: canonical, Raw: raw, Department: doc.Department, Classification: doc.Classification,
		})
	}
	return values, nil
}

func canonicalValue(valueType, raw string) (string, error) {
	switch valueType {
	case "date":
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return "", err
		}
		return exposure.CanonicalSQLValue("date", parsed)
	case "integer", "bigint":
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return "", err
		}
		return exposure.CanonicalSQLValue(valueType, parsed)
	default:
		return exposure.CanonicalSQLValue(valueType, raw)
	}
}

// Prepare loads the corpus named by the spec, checks its digest, and derives
// chunks, values and the label vocabulary deterministically, embedding chunk
// text with the spec's provider.
func Prepare(spec Spec, corpus []byte) (Prepared, error) {
	return PrepareWith(spec, corpus, nil)
}

// PrepareWith is Prepare with an explicit embedder (nil selects the provider
// the spec names: the hash provider needs nothing, bge-m3 needs an endpoint).
func PrepareWith(spec Spec, corpus []byte, embedder Embedder) (Prepared, error) {
	if embedder == nil {
		var err error
		if embedder, err = EmbedderFor(spec, ""); err != nil {
			return Prepared{}, err
		}
	}
	if err := embedder.Check(spec.Embedding); err != nil {
		return Prepared{}, err
	}
	if sha256Hex(corpus) != spec.Corpus.SHA256 {
		return Prepared{}, errors.New("corpus bytes do not match the sha256 declared by the publish spec")
	}
	documents, err := kbcorpus.DecodeJSONL(corpus)
	if err != nil {
		return Prepared{}, err
	}
	if spec.Corpus.MaxDocuments > 0 {
		kept := documents[:0]
		seen := map[string]struct{}{}
		for _, doc := range documents {
			if _, ok := seen[doc.DocID]; !ok {
				if len(seen) == spec.Corpus.MaxDocuments {
					break
				}
				seen[doc.DocID] = struct{}{}
			}
			kept = append(kept, doc)
		}
		documents = kept
	}
	if len(documents) == 0 {
		return Prepared{}, errors.New("corpus contains no documents")
	}
	prepared := Prepared{Documents: documents}
	labels := map[string]struct{}{}
	for _, doc := range documents {
		chunks, err := Segment(doc, spec.Segmentation.WindowChars)
		if err != nil {
			return Prepared{}, err
		}
		prepared.Chunks = append(prepared.Chunks, chunks...)
		values, err := Extract(doc, spec.ExtractionRules)
		if err != nil {
			return Prepared{}, err
		}
		prepared.Values = append(prepared.Values, values...)
		for _, label := range doc.Labels {
			labels[label] = struct{}{}
		}
	}
	for label := range labels {
		prepared.Labels = append(prepared.Labels, label)
	}
	sort.Strings(prepared.Labels)
	embedded := make(map[string][]float32)
	for start := 0; start < len(prepared.Chunks); start += embedBatch {
		end := start + embedBatch
		if end > len(prepared.Chunks) {
			end = len(prepared.Chunks)
		}
		texts := make([]string, 0, end-start)
		for _, chunk := range prepared.Chunks[start:end] {
			texts = append(texts, chunk.Text)
		}
		vectors, err := embedDistinct(embedder, texts, embedded)
		if err != nil {
			return Prepared{}, fmt.Errorf("embed chunks %d..%d: %w", start, end, err)
		}
		if len(vectors) != len(texts) {
			return Prepared{}, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(texts))
		}
		for offset, vector := range vectors {
			if len(vector) != spec.Embedding.Dimension {
				return Prepared{}, fmt.Errorf("embedder returned dimension %d, spec declares %d", len(vector), spec.Embedding.Dimension)
			}
			prepared.Chunks[start+offset].Embedding = append([]float32(nil), vector...)
		}
	}
	return prepared, nil
}

// embedDistinct embeds, once, each text the publication has not embedded
// yet, and hands every occurrence the same vector. Chunks with the same text
// therefore carry bit-identical vectors whatever the provider's batch
// numerics, so an exact tie between them is broken by the chunk-identity
// total order in every arm (E-KB4 relies on this; with a provider that embeds
// single and batched inputs slightly differently, two separately embedded
// copies of one text could otherwise be ordered by noise).
func embedDistinct(embedder Embedder, texts []string, embedded map[string][]float32) ([][]float32, error) {
	var pending []string
	queued := make(map[string]struct{})
	for _, text := range texts {
		if _, done := embedded[text]; done {
			continue
		}
		if _, waiting := queued[text]; waiting {
			continue
		}
		queued[text] = struct{}{}
		pending = append(pending, text)
	}
	if len(pending) > 0 {
		vectors, err := embedder.Embed(pending)
		if err != nil {
			return nil, err
		}
		if len(vectors) != len(pending) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d texts", len(vectors), len(pending))
		}
		for index, vector := range vectors {
			embedded[pending[index]] = vector
		}
	}
	out := make([][]float32, len(texts))
	for index, text := range texts {
		out[index] = embedded[text]
	}
	return out, nil
}

const embedBatch = 128
