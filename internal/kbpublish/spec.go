// Package kbpublish freezes one document collection into one knowledge-product
// index publication epoch (docs/kb-design.md). It writes the kb.* source
// relations, exposes them as frozen reporting materialized views, compiles each
// through the existing V4 snapshot publication compiler, and binds the three
// bundles with the segmentation, embedding and extraction declarations in one
// digest-bound manifest. All judgment (segmentation, extraction, embedding)
// happens here, before publication; the online path only looks identities up.
package kbpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	SpecVersion     = "taskgate-kb-publish-spec-v1"
	ManifestVersion = "taskgate-kb-publication-v1"

	SegmentationSectionWindowV1 = "section-window-v1"
	EmbeddingHashProjectionV1   = "hash-projection-v1"
	ExtractionSourceFrontMatter = "frontmatter"
	ExtractionScopeDocument     = "document"
)

var (
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	epochPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	nsPattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	shaPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Spec struct {
	Version          string           `json:"version"`
	KnowledgeProduct string           `json:"knowledge_product"`
	Description      string           `json:"description"`
	Sensitivity      string           `json:"sensitivity"`
	Collection       string           `json:"collection"`
	SourceID         string           `json:"source_id"`
	CatalogSource    string           `json:"catalog_source"`
	SourceNamespace  string           `json:"source_namespace"`
	Epoch            string           `json:"epoch"`
	SchemaDigest     string           `json:"schema_digest"`
	Corpus           CorpusRef        `json:"corpus"`
	Segmentation     Segmentation     `json:"segmentation"`
	Embedding        Embedding        `json:"embedding"`
	ExtractionRules  []ExtractionRule `json:"extraction_rules"`
	DocumentTypes    []string         `json:"document_types"`
	Retrieval        Retrieval        `json:"retrieval"`
}

type Retrieval struct {
	KMax       int      `json:"k_max"`
	Aggregates []string `json:"aggregates"`
}

type CorpusRef struct {
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
	MaxDocuments int    `json:"max_documents,omitempty"`
}

type Segmentation struct {
	Function    string `json:"function"`
	WindowChars int    `json:"window_chars"`
}

type Embedding struct {
	Model     string `json:"model"`
	Dimension int    `json:"dimension"`
}

type ExtractionRule struct {
	DocumentType string `json:"document_type"`
	Field        string `json:"field"`
	Type         string `json:"type"`
	Source       string `json:"source"`
	Scope        string `json:"scope"`
}

func LoadSpec(path string) (Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, fmt.Errorf("read publish spec: %w", err)
	}
	return ParseSpec(data)
}

func ParseSpec(data []byte) (Spec, error) {
	var spec Spec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("parse publish spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func (s Spec) Validate() error {
	switch {
	case s.Version != SpecVersion:
		return fmt.Errorf("publish spec: version must be %q", SpecVersion)
	case !namePattern.MatchString(s.KnowledgeProduct):
		return errors.New("publish spec: knowledge_product must be a lowercase identifier")
	case strings.TrimSpace(s.Description) == "":
		return errors.New("publish spec: description is required")
	case s.Sensitivity != "public" && s.Sensitivity != "low" && s.Sensitivity != "medium" && s.Sensitivity != "high" && s.Sensitivity != "restricted":
		return errors.New("publish spec: sensitivity must be public, low, medium, high, or restricted")
	case len(s.DocumentTypes) == 0:
		return errors.New("publish spec: document_types are required")
	case s.Retrieval.KMax < 1 || s.Retrieval.KMax > 1000:
		return errors.New("publish spec: retrieval.k_max must be between 1 and 1000")
	case !namePattern.MatchString(s.Collection):
		return errors.New("publish spec: collection must be a lowercase identifier")
	case strings.TrimSpace(s.SourceID) == "" || strings.TrimSpace(s.CatalogSource) == "":
		return errors.New("publish spec: source_id and catalog_source are required")
	case !nsPattern.MatchString(s.SourceNamespace):
		return errors.New("publish spec: source_namespace must be dotted lowercase identifiers")
	case !epochPattern.MatchString(s.Epoch):
		return errors.New("publish spec: epoch is not a canonical publication name")
	case !shaPattern.MatchString(s.SchemaDigest):
		return errors.New("publish spec: schema_digest must be 64 lowercase hex characters")
	case strings.TrimSpace(s.Corpus.Path) == "" || !shaPattern.MatchString(s.Corpus.SHA256):
		return errors.New("publish spec: corpus path and sha256 are required")
	case s.Corpus.MaxDocuments < 0:
		return errors.New("publish spec: max_documents cannot be negative")
	case s.Segmentation.Function != SegmentationSectionWindowV1:
		return fmt.Errorf("publish spec: segmentation function must be %q", SegmentationSectionWindowV1)
	case s.Segmentation.WindowChars < 64 || s.Segmentation.WindowChars > 8192:
		return errors.New("publish spec: window_chars must be between 64 and 8192")
	case s.Embedding.Model != EmbeddingHashProjectionV1:
		return fmt.Errorf("publish spec: embedding model must be %q", EmbeddingHashProjectionV1)
	case s.Embedding.Dimension < 16 || s.Embedding.Dimension > 4096:
		return errors.New("publish spec: embedding dimension must be between 16 and 4096")
	}
	documentTypes := map[string]struct{}{}
	for _, documentType := range s.DocumentTypes {
		if !namePattern.MatchString(documentType) {
			return errors.New("publish spec: document_types must be lowercase identifiers")
		}
		if _, duplicate := documentTypes[documentType]; duplicate {
			return errors.New("publish spec: document_types must be distinct")
		}
		documentTypes[documentType] = struct{}{}
	}
	for _, aggregate := range s.Retrieval.Aggregates {
		switch aggregate {
		case "count", "sum", "min", "max":
		default:
			return fmt.Errorf("publish spec: retrieval aggregate %q is not admitted", aggregate)
		}
	}
	seen := map[string]struct{}{}
	for index, rule := range s.ExtractionRules {
		if _, ok := documentTypes[rule.DocumentType]; !ok {
			return fmt.Errorf("publish spec: extraction_rules[%d] names an undeclared document type", index)
		}
		if !namePattern.MatchString(rule.DocumentType) || !namePattern.MatchString(rule.Field) {
			return fmt.Errorf("publish spec: extraction_rules[%d] needs lowercase document_type and field", index)
		}
		switch rule.Type {
		case "text", "numeric", "integer", "bigint", "date":
		default:
			return fmt.Errorf("publish spec: extraction_rules[%d] type %q is not supported", index, rule.Type)
		}
		if rule.Source != ExtractionSourceFrontMatter || rule.Scope != ExtractionScopeDocument {
			return fmt.Errorf("publish spec: extraction_rules[%d] supports only source=frontmatter scope=document in KB-1", index)
		}
		key := rule.DocumentType + "\x00" + rule.Field
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("publish spec: extraction_rules[%d] duplicates %s.%s", index, rule.DocumentType, rule.Field)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// epochToken makes the epoch usable inside PostgreSQL identifiers.
func epochToken(epoch string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(epoch)
}

// RelationKinds are the three frozen relations of a knowledge product in
// publication order.
var RelationKinds = []string{"documents", "chunks", "values"}

// RelationNames derives the physical and Catalog names of one relation kind.
type RelationNames struct {
	Kind            string
	Product         string
	PublicationName string
	SourceRelation  string
	OrdinalSidecar  string
	SourceNamespace string
}

func (s Spec) Relation(kind string) RelationNames {
	token := epochToken(s.Epoch)
	return RelationNames{
		Kind:            kind,
		Product:         s.KnowledgeProduct + "_" + kind,
		PublicationName: s.Epoch + "-" + kind,
		SourceRelation:  "reporting.kb_" + s.KnowledgeProduct + "_" + token + "_" + kind,
		OrdinalSidecar:  "taskgate_ordinal.kb_" + s.KnowledgeProduct + "_" + token + "_" + kind,
		SourceNamespace: s.SourceNamespace + "." + kind,
	}
}

func canonicalDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
