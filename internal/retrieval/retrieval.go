// Package retrieval is the degrader of the closed retrieval language
// (docs/kb-design.md section 4): it turns a search, fetch or aggregate request
// against a knowledge product into a normal form whose digest h_N is the
// request commitment of the composite Outcome Fact. Equivalent requests
// (reordered atoms, reordered or repeated `in` values, whitespace and case
// differences in text, a single-value `in` versus `=`) normalize identically;
// anything outside the language fails closed with a stable code. The package
// performs no I/O and knows nothing about settlement.
package retrieval

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	NormalFormVersion = "taskgate-retrieval-nf-v1"
	normalFormDomain  = "TASKGATE-RETRIEVAL-NF-V1\x00"
	// QuantizationV1 maps each vector component, clipped to [-1, 1], to a
	// little-endian int16 before hashing, so two floating-point renderings of
	// one unit vector commit identically.
	QuantizationV1 = "int16-fixed-point-v1"

	ToolSearch    = "search"
	ToolFetch     = "fetch"
	ToolAggregate = "aggregate"
)

// Error is a closed-form rejection with a stable apierr code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func reject(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Filter is one caller-supplied filter atom before normalization.
type Filter struct {
	Attribute string `json:"attribute"`
	Op        string `json:"op"`
	Value     any    `json:"value,omitempty"`
	Values    []any  `json:"values,omitempty"`
}

// Query is the search commitment input: exactly one of Text, Vector or
// Keywords. Keywords select the chunks whose text contains every keyword
// (docs/kb-design.md section 11).
type Query struct {
	Text     string    `json:"text,omitempty"`
	Vector   []float64 `json:"vector,omitempty"`
	Keywords []string  `json:"keywords,omitempty"`
}

// Keyword-query bounds (docs/kb-design.md section 11.1).
const (
	MaxKeywords     = 8
	MaxKeywordRunes = 64
	keywordDomain   = "TASKGATE-RETRIEVAL-KEYWORDS-V1"
)

// ChunkRef names one chunk a fetch asks for.
type ChunkRef struct {
	DocID      string `json:"doc_id"`
	Version    int    `json:"version"`
	ChunkIndex int    `json:"chunk_index"`
}

// Request is the decoded tool input. Tool selects which fields apply.
type Request struct {
	Tool             string     `json:"tool"`
	KnowledgeProduct string     `json:"knowledge_product"`
	Filters          []Filter   `json:"filters,omitempty"`
	Query            *Query     `json:"query,omitempty"`
	K                int        `json:"k,omitempty"`
	Chunks           []ChunkRef `json:"chunks,omitempty"`
	Field            string     `json:"field,omitempty"`
	Aggregate        string     `json:"aggregate,omitempty"`
}

// DecodeRequest parses tool arguments strictly; unknown fields (scripts,
// re-rankers, more_like_this, ...) are outside the language.
func DecodeRequest(tool string, raw []byte) (Request, error) {
	var request Request
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, reject(apierr.CodeRetrievalQueryUnsupported, "request is outside the closed retrieval language: %v", err)
	}
	if request.Tool != "" && request.Tool != tool {
		return Request{}, reject(apierr.CodeRetrievalQueryUnsupported, "request names tool %q but was sent to %s", request.Tool, tool)
	}
	request.Tool = tool
	return request, nil
}

// Atom is one normalized filter condition. Injected atoms carry the task's
// mandatory scope; they are part of the normal form but are never charged as
// caller-controlled Outcome atoms.
type Atom struct {
	Attribute        string `json:"attribute"`
	Type             string `json:"type"`
	Operator         string `json:"op"`
	CanonicalLiteral string `json:"literal"`
	Injected         bool   `json:"injected,omitempty"`
}

// QueryKindKeyword marks a keyword commitment; text and vector keep their
// historical kind names.
const QueryKindKeyword = "keyword"

type QueryCommitment struct {
	Kind         string `json:"kind"`
	SHA256       string `json:"sha256"`
	Dimension    int    `json:"dimension,omitempty"`
	Quantization string `json:"quantization,omitempty"`
}

type AggregateSpec struct {
	Field     string `json:"field"`
	ValueType string `json:"value_type"`
	Function  string `json:"function"`
}

// NormalForm is the canonical request. Its JSON is the preimage of h_N.
type NormalForm struct {
	Version          string           `json:"version"`
	Tool             string           `json:"tool"`
	KnowledgeProduct string           `json:"knowledge_product"`
	Epoch            string           `json:"epoch"`
	Atoms            []Atom           `json:"atoms"`
	Query            *QueryCommitment `json:"query,omitempty"`
	K                int              `json:"k,omitempty"`
	Chunks           []ChunkRef       `json:"chunks,omitempty"`
	Aggregate        *AggregateSpec   `json:"aggregate,omitempty"`
}

// Digest is h_N: the domain-separated SHA-256 of the canonical JSON.
func (n NormalForm) Digest() (string, error) {
	encoded, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(normalFormDomain), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

// CallerAtoms returns the atoms the caller controlled (the Outcome footprint).
func (n NormalForm) CallerAtoms() []Atom {
	var atoms []Atom
	for _, atom := range n.Atoms {
		if !atom.Injected {
			atoms = append(atoms, atom)
		}
	}
	return atoms
}

var operatorsByType = map[string]map[string]struct{}{
	"enum":     {"=": {}, "<>": {}, "in": {}},
	"text":     {"=": {}, "<>": {}, "in": {}},
	"text_set": {"has": {}},
	"date":     {"=": {}, "<>": {}, "<": {}, "<=": {}, ">": {}, ">=": {}, "in": {}},
	"numeric":  {"=": {}, "<>": {}, "<": {}, "<=": {}, ">": {}, ">=": {}, "in": {}},
}

// Normalize compiles a request against the knowledge product declaration and
// the task's mandatory scope (scope name -> admitted values, as the grant
// stores it). It is total over the closed language and rejects everything else.
func Normalize(product catalog.KnowledgeProduct, mandatoryScope map[string][]string, request Request) (NormalForm, error) {
	if request.KnowledgeProduct != "" && request.KnowledgeProduct != product.Name {
		return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "request names knowledge product %q, task binds %q", request.KnowledgeProduct, product.Name)
	}
	attributes := make(map[string]catalog.KBAttribute, len(product.Attributes))
	for _, attribute := range product.Attributes {
		attributes[attribute.Name] = attribute
	}
	form := NormalForm{Version: NormalFormVersion, Tool: request.Tool, KnowledgeProduct: product.Name, Epoch: product.IndexPublication}
	switch request.Tool {
	case ToolSearch, ToolAggregate:
	case ToolFetch:
		if len(request.Filters) != 0 || request.Query != nil || request.K != 0 || request.Field != "" || request.Aggregate != "" {
			return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "fetch accepts chunk references only")
		}
	default:
		return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "unknown retrieval tool %q", request.Tool)
	}

	// Caller atoms, grouped by attribute so scope intersection can be checked.
	callerValues := map[string]map[string]struct{}{}
	var atoms []Atom
	for index, filter := range request.Filters {
		attribute, ok := attributes[filter.Attribute]
		if !ok {
			return NormalForm{}, reject(apierr.CodeRetrievalFilterUnsupported, "filters[%d]: attribute %q is not in the knowledge product vocabulary", index, filter.Attribute)
		}
		if _, ok := operatorsByType[attribute.Type][filter.Op]; !ok {
			return NormalForm{}, reject(apierr.CodeRetrievalFilterUnsupported, "filters[%d]: operator %q is not admitted on %s attribute %q", index, filter.Op, attribute.Type, filter.Attribute)
		}
		var raw []any
		switch {
		case filter.Op == "in":
			if filter.Value != nil || len(filter.Values) == 0 {
				return NormalForm{}, reject(apierr.CodeRetrievalFilterUnsupported, "filters[%d]: in requires a non-empty values list and no value", index)
			}
			raw = filter.Values
		default:
			if filter.Values != nil || filter.Value == nil {
				return NormalForm{}, reject(apierr.CodeRetrievalFilterUnsupported, "filters[%d]: %s requires exactly one value", index, filter.Op)
			}
			raw = []any{filter.Value}
		}
		operator := filter.Op
		if operator == "in" && len(raw) == 1 {
			operator = "="
		}
		for _, value := range raw {
			literal, err := canonicalLiteral(attribute, value)
			if err != nil {
				return NormalForm{}, reject(apierr.CodeRetrievalFilterUnsupported, "filters[%d]: %v", index, err)
			}
			atoms = append(atoms, Atom{Attribute: attribute.Name, Type: attribute.Type, Operator: operator, CanonicalLiteral: literal})
			if attribute.Scope != "" && (operator == "=" || operator == "in") {
				if callerValues[attribute.Name] == nil {
					callerValues[attribute.Name] = map[string]struct{}{}
				}
				callerValues[attribute.Name][literal] = struct{}{}
			}
		}
	}
	if request.Tool == ToolFetch {
		atoms = nil
	}

	// Mandatory scope injection: every scoped attribute receives the task's
	// admitted values; a caller atom on that attribute must intersect them.
	scopedNames := make([]string, 0)
	for _, attribute := range product.Attributes {
		if attribute.Scope != "" {
			scopedNames = append(scopedNames, attribute.Name)
		}
	}
	sort.Strings(scopedNames)
	for _, name := range scopedNames {
		attribute := attributes[name]
		admitted := append([]string(nil), mandatoryScope[attribute.Scope]...)
		if len(admitted) == 0 {
			return NormalForm{}, reject(apierr.CodeRetrievalScopeExceeded, "task grant carries no mandatory scope %q required by knowledge product %s", attribute.Scope, product.Name)
		}
		canonicalAdmitted := make([]string, 0, len(admitted))
		for _, value := range admitted {
			literal, err := canonicalLiteral(attribute, value)
			if err != nil {
				return NormalForm{}, reject(apierr.CodeRetrievalScopeExceeded, "mandatory scope %q value %v: %v", attribute.Scope, value, err)
			}
			canonicalAdmitted = append(canonicalAdmitted, literal)
		}
		if caller := callerValues[name]; len(caller) > 0 {
			intersects := false
			for _, literal := range canonicalAdmitted {
				if _, ok := caller[literal]; ok {
					intersects = true
					break
				}
			}
			if !intersects {
				return NormalForm{}, reject(apierr.CodeRetrievalScopeExceeded, "filter on %q does not intersect the task's mandatory scope", name)
			}
		}
		if request.Tool != ToolFetch {
			operator := "in"
			if len(canonicalAdmitted) == 1 {
				operator = "="
			}
			for _, literal := range canonicalAdmitted {
				atoms = append(atoms, Atom{Attribute: name, Type: attribute.Type, Operator: operator, CanonicalLiteral: literal, Injected: true})
			}
		}
	}
	form.Atoms = canonicalAtoms(atoms)

	switch request.Tool {
	case ToolSearch:
		if request.Query == nil {
			return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "search requires a query")
		}
		commitment, err := commitQuery(*request.Query, product.Embedding.Dimension, product.Retrieval.Keyword)
		if err != nil {
			return NormalForm{}, err
		}
		form.Query = &commitment
		if request.K < 1 || request.K > product.Retrieval.KMax {
			return NormalForm{}, reject(apierr.CodeRetrievalKExceeded, "k must be between 1 and %d", product.Retrieval.KMax)
		}
		form.K = request.K
		if len(request.Chunks) != 0 || request.Field != "" || request.Aggregate != "" {
			return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "search accepts filters, query and k only")
		}
	case ToolFetch:
		if len(request.Chunks) == 0 {
			return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "fetch requires at least one chunk reference")
		}
		refs, err := canonicalChunkRefs(request.Chunks)
		if err != nil {
			return NormalForm{}, err
		}
		form.Chunks = refs
	case ToolAggregate:
		if request.Query != nil || request.K != 0 || len(request.Chunks) != 0 {
			return NormalForm{}, reject(apierr.CodeRetrievalQueryUnsupported, "aggregate accepts filters, field and aggregate only")
		}
		spec, err := aggregateSpec(product, request.Field, request.Aggregate)
		if err != nil {
			return NormalForm{}, err
		}
		form.Aggregate = &spec
	}
	return form, nil
}

func canonicalAtoms(atoms []Atom) []Atom {
	sort.Slice(atoms, func(i, j int) bool {
		a, b := atoms[i], atoms[j]
		if a.Attribute != b.Attribute {
			return a.Attribute < b.Attribute
		}
		if a.Injected != b.Injected {
			return !a.Injected
		}
		if a.Operator != b.Operator {
			return a.Operator < b.Operator
		}
		return a.CanonicalLiteral < b.CanonicalLiteral
	})
	out := atoms[:0]
	for index, atom := range atoms {
		if index > 0 && atom == atoms[index-1] {
			continue
		}
		out = append(out, atom)
	}
	if out == nil {
		out = []Atom{}
	}
	return out
}

// canonicalLiteral renders a literal with the exposure canonical encoding of
// the attribute's value type: the same bytes a V5 SQL atom would carry.
func canonicalLiteral(attribute catalog.KBAttribute, value any) (string, error) {
	switch attribute.Type {
	case "enum", "text", "text_set":
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("attribute %q expects a string", attribute.Name)
		}
		if text == "" || text != strings.TrimSpace(text) || strings.ContainsAny(text, "\x00\r\n\t") {
			return "", fmt.Errorf("attribute %q value must be a canonical non-empty token", attribute.Name)
		}
		if attribute.Type == "enum" && len(attribute.AllowedValues) > 0 && !contains(attribute.AllowedValues, text) {
			return "", fmt.Errorf("attribute %q does not admit value %q", attribute.Name, text)
		}
		return exposure.CanonicalSQLValue("text", text)
	case "date":
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("attribute %q expects an ISO date string", attribute.Name)
		}
		parsed, err := time.Parse("2006-01-02", text)
		if err != nil {
			return "", fmt.Errorf("attribute %q expects YYYY-MM-DD", attribute.Name)
		}
		return exposure.CanonicalSQLValue("date", parsed)
	case "numeric":
		switch typed := value.(type) {
		case json.Number:
			return exposure.CanonicalSQLValue("numeric", typed)
		case string:
			return exposure.CanonicalSQLValue("numeric", typed)
		case float64:
			// encoding/json without UseNumber hands floats; render exactly when integral.
			if typed == math.Trunc(typed) && math.Abs(typed) < 1e15 {
				return exposure.CanonicalSQLValue("numeric", fmt.Sprintf("%d", int64(typed)))
			}
			return "", fmt.Errorf("attribute %q: send numeric literals as strings to avoid binary float rounding", attribute.Name)
		default:
			return "", fmt.Errorf("attribute %q expects a numeric literal", attribute.Name)
		}
	default:
		return "", fmt.Errorf("attribute %q has unsupported type %q", attribute.Name, attribute.Type)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// NormalizeText is the text commitment preimage: Unicode NFC, case folding,
// whitespace collapsed to single spaces, trimmed.
func NormalizeText(text string) string {
	folded := strings.ToLower(norm.NFC.String(text))
	return strings.Join(strings.FieldsFunc(folded, unicode.IsSpace), " ")
}

// CanonicalKeywords is the keyword commitment preimage. Matching is
// case-sensitive substring containment, so case and inner whitespace are
// kept: each keyword is NFC-normalized and trimmed, and the list is
// deduplicated and sorted by bytes.
func CanonicalKeywords(keywords []string) ([]string, error) {
	if len(keywords) == 0 || len(keywords) > MaxKeywords {
		return nil, fmt.Errorf("a keyword query carries 1 to %d keywords", MaxKeywords)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(keywords))
	for index, keyword := range keywords {
		if !utf8.ValidString(keyword) {
			return nil, fmt.Errorf("keywords[%d] is not valid UTF-8", index)
		}
		canonical := strings.TrimSpace(norm.NFC.String(keyword))
		runes := utf8.RuneCountInString(canonical)
		if runes == 0 || runes > MaxKeywordRunes {
			return nil, fmt.Errorf("keywords[%d] must hold 1 to %d characters after normalization", index, MaxKeywordRunes)
		}
		for _, r := range canonical {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("keywords[%d] contains a control character", index)
			}
		}
		if _, duplicate := seen[canonical]; duplicate {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	sort.Strings(out)
	return out, nil
}

// KeywordCommitmentSHA256 is the commitment digest of a canonical keyword list.
func KeywordCommitmentSHA256(canonical []string) string {
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(append([]byte(keywordDomain+"\x00"), encoded...))
	return hex.EncodeToString(sum[:])
}

func commitQuery(query Query, dimension int, keywordAdmitted bool) (QueryCommitment, error) {
	hasText, hasVector, hasKeywords := strings.TrimSpace(query.Text) != "", len(query.Vector) != 0, len(query.Keywords) != 0
	forms := 0
	for _, present := range []bool{hasText, hasVector, hasKeywords} {
		if present {
			forms++
		}
	}
	switch {
	case forms > 1:
		return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "query must be exactly one of text, vector or keywords")
	case hasKeywords:
		if !keywordAdmitted {
			return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "this knowledge product does not admit keyword search")
		}
		canonical, err := CanonicalKeywords(query.Keywords)
		if err != nil {
			return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "%v", err)
		}
		return QueryCommitment{Kind: QueryKindKeyword, SHA256: KeywordCommitmentSHA256(canonical)}, nil
	case hasText:
		normalized := NormalizeText(query.Text)
		if len(normalized) > 4096 {
			return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "query text exceeds 4096 bytes after normalization")
		}
		sum := sha256.Sum256([]byte(normalized))
		return QueryCommitment{Kind: "text", SHA256: hex.EncodeToString(sum[:])}, nil
	case hasVector:
		if dimension == 0 || len(query.Vector) != dimension {
			return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "query vector must have dimension %d", dimension)
		}
		quantized, err := QuantizeVector(query.Vector)
		if err != nil {
			return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "%v", err)
		}
		sum := sha256.Sum256(quantized)
		return QueryCommitment{Kind: "vector", SHA256: hex.EncodeToString(sum[:]), Dimension: dimension, Quantization: QuantizationV1}, nil
	default:
		return QueryCommitment{}, reject(apierr.CodeRetrievalQueryUnsupported, "query must carry text, a vector or keywords")
	}
}

// QuantizeVector renders a vector as int16-fixed-point-v1 bytes.
func QuantizeVector(vector []float64) ([]byte, error) {
	out := make([]byte, 0, 2*len(vector))
	for _, component := range vector {
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return nil, errors.New("query vector contains NaN or Inf")
		}
		clipped := math.Max(-1, math.Min(1, component))
		quantized := int16(math.Round(clipped * 32767))
		out = binary.LittleEndian.AppendUint16(out, uint16(quantized))
	}
	return out, nil
}

func canonicalChunkRefs(refs []ChunkRef) ([]ChunkRef, error) {
	out := make([]ChunkRef, 0, len(refs))
	seen := map[ChunkRef]struct{}{}
	for index, ref := range refs {
		if ref.DocID == "" || ref.DocID != strings.TrimSpace(ref.DocID) || ref.Version < 1 || ref.ChunkIndex < 0 {
			return nil, reject(apierr.CodeRetrievalQueryUnsupported, "chunks[%d] is not a canonical chunk reference", index)
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].ChunkIndex < out[j].ChunkIndex
	})
	if len(out) > 1000 {
		return nil, reject(apierr.CodeRetrievalQueryUnsupported, "fetch references more than 1000 chunks")
	}
	return out, nil
}

func aggregateSpec(product catalog.KnowledgeProduct, field, function string) (AggregateSpec, error) {
	if !contains(product.Retrieval.Aggregates, function) {
		return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "aggregate %q is not admitted for knowledge product %s", function, product.Name)
	}
	valueType := ""
	for _, rule := range product.ExtractionRules {
		if rule.Field == field {
			if valueType != "" && valueType != rule.Type {
				return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "field %q is extracted with more than one type", field)
			}
			valueType = rule.Type
		}
	}
	if valueType == "" {
		return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "field %q is not an extracted field of knowledge product %s", field, product.Name)
	}
	switch function {
	case "count":
	case "sum":
		if valueType != "numeric" && valueType != "integer" && valueType != "bigint" {
			return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "sum requires a numeric field; %q is %s", field, valueType)
		}
	case "min", "max":
		if valueType == "text" {
			return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "%s requires a numeric or date field; %q is text", function, field)
		}
	default:
		return AggregateSpec{}, reject(apierr.CodeAggregateUnsupported, "aggregate %q is unknown", function)
	}
	return AggregateSpec{Field: field, ValueType: valueType, Function: function}, nil
}
