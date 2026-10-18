// Package kbdownload holds the closed forms of the document download task
// (docs/kb-design.md §12): the approved document list a download task carries
// in its grant, and the deterministic file a download delivers.
//
// A download is not a new kind of read. The gateway reads the document through
// ordinary prepared operations on the knowledge product's published relations
// (documents, chunks, and values when approved), settles each through the task
// ledger, and only then assembles the file from the settled rows. Nothing in
// the file comes from outside the published cells: the stored body, the section
// headings and the raw front matter are never delivered (§12.2).
package kbdownload

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ScopeKey is the reserved mandatory-scope key under which a download task's
// grant carries its approved documents as sorted "doc_id@version" strings. It
// is not a Catalog scope: no product field carries that name, so the policy
// renderer never injects it, and the download tool checks membership itself.
// An approver narrows it to a non-empty subset like any other enum scope.
const ScopeKey = "kb_documents"

// MaxDocuments bounds one download task's list. It matches the retrieval k_max
// of the evaluated knowledge products; it is an operating value, not a
// security parameter.
const MaxDocuments = 20

// FileVersion names the delivered file format.
const FileVersion = "taskgate-kb-document-file-v1"

// FileMediaType is the delivered file's media type.
const FileMediaType = "text/markdown; charset=utf-8"

var docIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Ref names one document version of a knowledge product.
type Ref struct {
	DocID   string `json:"doc_id"`
	Version int64  `json:"version"`
}

// String renders the reference as it is stored in the grant.
func (r Ref) String() string { return r.DocID + "@" + strconv.FormatInt(r.Version, 10) }

// FileName is the delivered file's name.
func (r Ref) FileName() string { return r.DocID + "-v" + strconv.FormatInt(r.Version, 10) + ".md" }

// Validate refuses a reference the closed form does not admit.
func (r Ref) Validate() error {
	if !docIDPattern.MatchString(r.DocID) {
		return fmt.Errorf("doc_id %q is not an admitted document identifier", r.DocID)
	}
	if r.Version < 0 {
		return fmt.Errorf("document %s has a negative version", r.DocID)
	}
	return nil
}

// ParseRef reads a stored "doc_id@version" reference.
func ParseRef(value string) (Ref, error) {
	docID, version, found := strings.Cut(value, "@")
	if !found || strings.Contains(version, "@") {
		return Ref{}, fmt.Errorf("document reference %q is not doc_id@version", value)
	}
	parsed, err := strconv.ParseInt(version, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != version {
		return Ref{}, fmt.Errorf("document reference %q has a non-canonical version", value)
	}
	ref := Ref{DocID: docID, Version: parsed}
	if err := ref.Validate(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// CanonicalList validates a requested list and returns it as the sorted,
// duplicate-free strings the grant stores. An empty list, a duplicate or more
// than MaxDocuments entries is refused rather than repaired.
func CanonicalList(refs []Ref) ([]string, error) {
	if len(refs) == 0 || len(refs) > MaxDocuments {
		return nil, fmt.Errorf("a download task names 1 to %d documents", MaxDocuments)
	}
	seen := make(map[string]struct{}, len(refs))
	list := make([]string, 0, len(refs))
	for _, ref := range refs {
		if err := ref.Validate(); err != nil {
			return nil, err
		}
		key := ref.String()
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("document %s is listed twice", key)
		}
		seen[key] = struct{}{}
		list = append(list, key)
	}
	sort.Strings(list)
	return list, nil
}

// ApprovedList reads the reserved key from a decoded mandatory scope. present
// is false for a task that is not a download task; a malformed value is an
// error, never "not a download task".
func ApprovedList(scope map[string]any) (refs []string, present bool, err error) {
	raw, present := scope[ScopeKey]
	if !present {
		return nil, false, nil
	}
	values, ok := raw.([]any)
	if !ok {
		if typed, isStrings := raw.([]string); isStrings {
			for _, value := range typed {
				values = append(values, value)
			}
		} else {
			return nil, true, errors.New("the approved document list is not a list")
		}
	}
	if len(values) == 0 || len(values) > MaxDocuments {
		return nil, true, fmt.Errorf("the approved document list has %d entries", len(values))
	}
	refs = make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			return nil, true, errors.New("the approved document list holds a non-string entry")
		}
		if _, err := ParseRef(text); err != nil {
			return nil, true, err
		}
		refs = append(refs, text)
	}
	return refs, true, nil
}

// DisplayValue renders an extracted value's canonical encoding (the exposure
// canonical form the values relation publishes, internal/exposure
// CanonicalSQLValue) for a reader: the type prefix of text ("s:"), date ("d:")
// and integer ("i:") values is dropped; an exact numeric ("n:p/q", a reduced
// fraction) becomes its terminating decimal when q has no prime factor other
// than 2 and 5, and stays the exact fraction otherwise. Anything else is shown
// as published. It is a pure function of the published cell, so the file
// carries no information the cell does not.
func DisplayValue(canonical string) string {
	prefix, rest, found := strings.Cut(canonical, ":")
	if !found {
		return canonical
	}
	switch prefix {
	case "s", "d", "i":
		return rest
	case "n":
		rational, ok := new(big.Rat).SetString(rest)
		if !ok {
			return canonical
		}
		if rational.IsInt() {
			return rational.Num().String()
		}
		denominator := new(big.Int).Set(rational.Denom())
		zero, two, five := big.NewInt(0), big.NewInt(2), big.NewInt(5)
		twos, fives := 0, 0
		for new(big.Int).Mod(denominator, two).Cmp(zero) == 0 {
			denominator.Quo(denominator, two)
			twos++
		}
		for new(big.Int).Mod(denominator, five).Cmp(zero) == 0 {
			denominator.Quo(denominator, five)
			fives++
		}
		if denominator.Cmp(big.NewInt(1)) != 0 {
			return rational.RatString()
		}
		return rational.FloatString(max(twos, fives))
	}
	return canonical
}

// Value is one extracted typed value of the document.
type Value struct {
	Field          string `json:"field"`
	ValueType      string `json:"value_type"`
	CanonicalValue string `json:"canonical_value"`
}

// Chunk is one section-window chunk of the document.
type Chunk struct {
	Index      int64  `json:"chunk_index"`
	TextSHA256 string `json:"text_sha256"`
	Text       string `json:"-"`
}

// File is everything a delivered file is assembled from.
type File struct {
	Ref    Ref
	Title  string
	Values []Value // nil when the task's grant does not include the values relation
	Chunks []Chunk
}

// Assemble renders the file deterministically: UTF-8, LF line ends, values
// ordered by field then value type then value, chunks by index, one blank line
// between blocks and a single trailing LF. Every chunk text must hash to its
// published digest; the indexes must be exactly 0..n-1, so a file is never
// assembled from a partial document.
func Assemble(file File) ([]byte, error) {
	if err := file.Ref.Validate(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(file.Title, "\r\n") || strings.TrimSpace(file.Title) == "" {
		return nil, errors.New("the document title is empty or spans lines")
	}
	if len(file.Chunks) == 0 {
		return nil, errors.New("a document file needs at least one chunk")
	}
	chunks := append([]Chunk(nil), file.Chunks...)
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
	for position, chunk := range chunks {
		if chunk.Index != int64(position) {
			return nil, fmt.Errorf("chunk indexes are not exactly 0..%d", len(chunks)-1)
		}
		if sha256Hex(chunk.Text) != chunk.TextSHA256 {
			return nil, fmt.Errorf("chunk %d does not hash to its published digest", chunk.Index)
		}
		if strings.Contains(chunk.Text, "\r") {
			return nil, fmt.Errorf("chunk %d carries a carriage return", chunk.Index)
		}
	}
	values := append([]Value(nil), file.Values...)
	sort.Slice(values, func(i, j int) bool {
		if values[i].Field != values[j].Field {
			return values[i].Field < values[j].Field
		}
		if values[i].ValueType != values[j].ValueType {
			return values[i].ValueType < values[j].ValueType
		}
		return values[i].CanonicalValue < values[j].CanonicalValue
	})
	var out strings.Builder
	out.WriteString("# " + file.Title + "\n\n")
	out.WriteString("doc_id: " + file.Ref.DocID + "\n")
	out.WriteString("version: " + strconv.FormatInt(file.Ref.Version, 10) + "\n")
	if len(values) > 0 {
		out.WriteString("\n## 字段\n\n")
		for _, value := range values {
			if strings.ContainsAny(value.Field+value.ValueType+value.CanonicalValue, "\r\n") {
				return nil, errors.New("an extracted value spans lines")
			}
			out.WriteString("- " + value.Field + " (" + value.ValueType + "): " + DisplayValue(value.CanonicalValue) + "\n")
		}
	}
	out.WriteString("\n## 正文\n")
	for _, chunk := range chunks {
		out.WriteString("\n" + chunk.Text + "\n")
	}
	return []byte(out.String()), nil
}

// SHA256 is the hex digest of a delivered file.
func SHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
