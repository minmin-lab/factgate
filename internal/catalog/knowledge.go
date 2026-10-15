package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	kbNamespacePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	kbEpochPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

var kbAttributeTypes = map[string]struct{}{"enum": {}, "text": {}, "text_set": {}, "date": {}, "numeric": {}}
var kbValueTypes = map[string]struct{}{"text": {}, "numeric": {}, "integer": {}, "bigint": {}, "date": {}}
var kbAggregates = map[string]struct{}{"count": {}, "sum": {}, "min": {}, "max": {}}

// kbRelationKinds pairs each relation kind with the fields its Product must
// publish for the knowledge Fact identities of docs/kb-design.md to exist.
var kbRelationRequiredFields = map[string][]string{
	"documents": {"doc_id", "version", "content_sha256", "chunk_count"},
	"chunks":    {"doc_id", "version", "chunk_index", "text", "text_sha256"},
	"values":    {"doc_id", "version", "field", "value_type", "canonical_value", "value_text", "value_numeric", "value_date"},
}

// CanonicalDeclarationDigest is the SHA-256 of a declaration's canonical JSON;
// kb_publications digests are compared against it so a Catalog cannot bind an
// epoch to a segmentation, embedding or rule set other than the one it declares.
func CanonicalDeclarationDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func (c *Catalog) validateKnowledgeProducts() ValidationErrors {
	var problems ValidationErrors
	if len(c.KnowledgeProducts) == 0 && len(c.KBPublications) == 0 {
		return nil
	}
	scopes := make(map[string]Scope, len(c.Scopes))
	for _, scope := range c.Scopes {
		scopes[scope.Name] = scope
	}
	publications := make(map[string]KBPublication, len(c.KBPublications))
	for index, publication := range c.KBPublications {
		path := fmt.Sprintf("kb_publications[%d]", index)
		if !kbEpochPattern.MatchString(publication.Epoch) {
			problems = append(problems, fieldError(path+".epoch", "a canonical epoch name is required", ErrInvalidCatalog))
		}
		if _, duplicate := publications[publication.Epoch]; duplicate {
			problems = append(problems, fieldError(path+".epoch", "epoch is duplicated", ErrInvalidCatalog))
		}
		publications[publication.Epoch] = publication
		if _, ok := c.LookupKnowledgeProduct(publication.KnowledgeProduct); !ok {
			problems = append(problems, fieldError(path+".knowledge_product", "referenced knowledge product does not exist", ErrInvalidCatalog))
		}
		for field, digest := range map[string]string{"manifest_digest": publication.ManifestDigest, "segmentation_digest": publication.SegmentationDigest,
			"embedding_digest": publication.EmbeddingDigest, "extraction_rules_digest": publication.ExtractionRulesDigest} {
			if !sha256HexPattern.MatchString(digest) {
				problems = append(problems, fieldError(path+"."+field, field+" must be lowercase SHA-256", ErrInvalidCatalog))
			}
		}
	}
	names := make(map[string]struct{}, len(c.KnowledgeProducts))
	for index, product := range c.KnowledgeProducts {
		path := fmt.Sprintf("knowledge_products[%d]", index)
		if !identifierPattern.MatchString(product.Name) {
			problems = append(problems, fieldError(path+".name", "a lowercase knowledge product name is required", ErrMissingField))
		}
		if _, duplicate := names[product.Name]; duplicate {
			problems = append(problems, fieldError(path+".name", "knowledge product name is duplicated", ErrInvalidCatalog))
		}
		names[product.Name] = struct{}{}
		if _, clash := c.LookupProduct(product.Name); clash {
			problems = append(problems, fieldError(path+".name", "knowledge product name collides with a product", ErrInvalidCatalog))
		}
		if strings.TrimSpace(product.Description) == "" {
			problems = append(problems, fieldError(path+".description", "description is required", ErrMissingField))
		}
		if err := product.Sensitivity.Validate(); err != nil {
			problems = append(problems, fieldError(path+".sensitivity", "a supported sensitivity is required", ErrInvalidCatalog))
		}
		if !identifierPattern.MatchString(product.Collection) {
			problems = append(problems, fieldError(path+".collection", "a lowercase collection name is required", ErrInvalidCatalog))
		}
		if !kbNamespacePattern.MatchString(product.SourceNamespace) {
			problems = append(problems, fieldError(path+".source_namespace", "a dotted lowercase source namespace is required", ErrInvalidCatalog))
		}
		if len(product.Scopes) == 0 || duplicateOrEmpty(product.Scopes) {
			problems = append(problems, fieldError(path+".scopes", "at least one mandatory scope is required", ErrInvalidCatalog))
		}
		mandatory := make(map[string]struct{}, len(product.Scopes))
		for _, name := range product.Scopes {
			mandatory[name] = struct{}{}
			if definition, ok := scopes[name]; !ok || definition.Type != ScopeTypeEnum {
				problems = append(problems, fieldError(path+".scopes", "knowledge product scopes must be enum Catalog scopes", ErrInvalidCatalog))
			}
		}
		relations := map[string]string{"documents": product.Products.Documents, "chunks": product.Products.Chunks, "values": product.Products.Values}
		for _, kind := range []string{"documents", "chunks", "values"} {
			relationPath := path + ".products." + kind
			relation, ok := c.LookupProduct(relations[kind])
			if !ok {
				problems = append(problems, fieldError(relationPath, "referenced product does not exist", ErrInvalidCatalog))
				continue
			}
			if relation.SnapshotPublication == "" {
				problems = append(problems, fieldError(relationPath, "knowledge relations must be snapshot publication products", ErrInvalidCatalog))
			}
			if relation.FactNamespace != product.SourceNamespace+"."+kind {
				problems = append(problems, fieldError(relationPath, "product fact_namespace must be <source_namespace>."+kind, ErrInvalidCatalog))
			}
			if relation.Snapshot != product.IndexPublication {
				problems = append(problems, fieldError(relationPath, "product snapshot must equal the index publication epoch", ErrInvalidCatalog))
			}
			published := make(map[string]struct{}, len(relation.Fields))
			for _, field := range relation.Fields {
				published[field.Name] = struct{}{}
			}
			for _, required := range kbRelationRequiredFields[kind] {
				if _, ok := published[required]; !ok {
					problems = append(problems, fieldError(relationPath, "product must publish field "+required, ErrInvalidCatalog))
				}
			}
			relationScopes := make(map[string]struct{}, len(relation.Scopes))
			for _, scope := range relation.Scopes {
				relationScopes[scope] = struct{}{}
			}
			for scope := range mandatory {
				if _, ok := relationScopes[scope]; !ok {
					problems = append(problems, fieldError(relationPath, "product must carry mandatory scope "+scope, ErrInvalidCatalog))
				}
			}
		}
		if _, ok := publications[product.IndexPublication]; !ok {
			problems = append(problems, fieldError(path+".index_publication", "referenced kb publication epoch does not exist", ErrInvalidCatalog))
		} else if publications[product.IndexPublication].KnowledgeProduct != product.Name {
			problems = append(problems, fieldError(path+".index_publication", "kb publication binds a different knowledge product", ErrInvalidCatalog))
		}
		if len(product.DocumentTypes) == 0 || duplicateOrEmpty(product.DocumentTypes) {
			problems = append(problems, fieldError(path+".document_types", "distinct document types are required", ErrInvalidCatalog))
		}
		documentTypes := make(map[string]struct{}, len(product.DocumentTypes))
		for _, documentType := range product.DocumentTypes {
			documentTypes[documentType] = struct{}{}
			if !identifierPattern.MatchString(documentType) {
				problems = append(problems, fieldError(path+".document_types", "document types must be lowercase identifiers", ErrInvalidCatalog))
			}
		}
		attributes := make(map[string]struct{}, len(product.Attributes))
		scopedAttributes := make(map[string]struct{})
		for attrIndex, attribute := range product.Attributes {
			attrPath := fmt.Sprintf("%s.attributes[%d]", path, attrIndex)
			if !identifierPattern.MatchString(attribute.Name) {
				problems = append(problems, fieldError(attrPath+".name", "a lowercase attribute name is required", ErrInvalidCatalog))
			}
			if _, duplicate := attributes[attribute.Name]; duplicate {
				problems = append(problems, fieldError(attrPath+".name", "attribute name is duplicated", ErrInvalidCatalog))
			}
			attributes[attribute.Name] = struct{}{}
			if _, ok := kbAttributeTypes[attribute.Type]; !ok {
				problems = append(problems, fieldError(attrPath+".type", "attribute type must be enum, text, text_set, date, or numeric", ErrInvalidCatalog))
			}
			if attribute.Type == "enum" && (len(attribute.AllowedValues) == 0 || duplicateOrEmpty(attribute.AllowedValues)) && attribute.Scope == "" {
				problems = append(problems, fieldError(attrPath+".allowed_values", "enum attributes need distinct allowed values or a scope", ErrInvalidCatalog))
			}
			if attribute.Type != "enum" && len(attribute.AllowedValues) != 0 {
				problems = append(problems, fieldError(attrPath+".allowed_values", "allowed values apply to enum attributes only", ErrInvalidCatalog))
			}
			if attribute.Scope != "" {
				if _, ok := mandatory[attribute.Scope]; !ok || attribute.Type != "enum" || attribute.Scope != attribute.Name {
					problems = append(problems, fieldError(attrPath+".scope", "a scoped attribute must be an enum attribute named after one of the knowledge product's mandatory scopes", ErrInvalidCatalog))
				}
				scopedAttributes[attribute.Scope] = struct{}{}
			}
		}
		for scope := range mandatory {
			if _, ok := scopedAttributes[scope]; !ok {
				problems = append(problems, fieldError(path+".attributes", "every mandatory scope needs an attribute bound to it", ErrInvalidCatalog))
			}
		}
		if product.Segmentation.Function != KBSegmentationSectionWindowV1 || product.Segmentation.WindowChars < 64 || product.Segmentation.WindowChars > 8192 {
			problems = append(problems, fieldError(path+".segmentation", "segmentation must be "+KBSegmentationSectionWindowV1+" with window_chars in 64..8192", ErrInvalidCatalog))
		}
		if product.Embedding.Model != KBEmbeddingHashProjectionV1 || product.Embedding.Dimension < 16 || product.Embedding.Dimension > 4096 {
			problems = append(problems, fieldError(path+".embedding", "embedding must be "+KBEmbeddingHashProjectionV1+" with dimension in 16..4096", ErrInvalidCatalog))
		}
		rules := make(map[string]struct{}, len(product.ExtractionRules))
		for ruleIndex, rule := range product.ExtractionRules {
			rulePath := fmt.Sprintf("%s.extraction_rules[%d]", path, ruleIndex)
			if _, ok := documentTypes[rule.DocumentType]; !ok {
				problems = append(problems, fieldError(rulePath+".document_type", "extraction rule names an undeclared document type", ErrInvalidCatalog))
			}
			if !identifierPattern.MatchString(rule.Field) {
				problems = append(problems, fieldError(rulePath+".field", "a lowercase field name is required", ErrInvalidCatalog))
			}
			if _, ok := kbValueTypes[rule.Type]; !ok {
				problems = append(problems, fieldError(rulePath+".type", "extraction type must be text, numeric, integer, bigint, or date", ErrInvalidCatalog))
			}
			if rule.Source != "frontmatter" || rule.Scope != "document" {
				problems = append(problems, fieldError(rulePath, "extraction rules support source=frontmatter and scope=document", ErrInvalidCatalog))
			}
			key := rule.DocumentType + "\x00" + rule.Field
			if _, duplicate := rules[key]; duplicate {
				problems = append(problems, fieldError(rulePath, "extraction rule is duplicated", ErrInvalidCatalog))
			}
			rules[key] = struct{}{}
		}
		if product.Retrieval.KMax < 1 || product.Retrieval.KMax > 1000 {
			problems = append(problems, fieldError(path+".retrieval.k_max", "k_max must be between 1 and 1000", ErrInvalidCatalog))
		}
		if duplicateOrEmpty(product.Retrieval.Aggregates) && len(product.Retrieval.Aggregates) != 0 {
			problems = append(problems, fieldError(path+".retrieval.aggregates", "aggregates must be distinct", ErrInvalidCatalog))
		}
		for _, aggregate := range product.Retrieval.Aggregates {
			if _, ok := kbAggregates[aggregate]; !ok {
				problems = append(problems, fieldError(path+".retrieval.aggregates", "aggregates must be count, sum, min, or max", ErrInvalidCatalog))
			}
		}
		if product.Retrieval.GenerationAdmitted {
			problems = append(problems, fieldError(path+".retrieval.generation_admitted", "generated answers are not admitted in this version", ErrInvalidCatalog))
		}
		if product.Eligibility != nil {
			problems = append(problems, validateEligibility(path+".eligibility", product.Eligibility, product.Scopes, c.Scopes)...)
		}
		if publication, ok := publications[product.IndexPublication]; ok && publication.KnowledgeProduct == product.Name {
			for field, pair := range map[string][2]any{
				"segmentation_digest":     {product.Segmentation, publication.SegmentationDigest},
				"embedding_digest":        {product.Embedding, publication.EmbeddingDigest},
				"extraction_rules_digest": {product.ExtractionRules, publication.ExtractionRulesDigest},
			} {
				digest, err := CanonicalDeclarationDigest(pair[0])
				if err != nil || digest != pair[1] {
					problems = append(problems, fieldError(path+"."+strings.TrimSuffix(field, "_digest"), field+" in kb_publications does not match this declaration", ErrInvalidCatalog))
				}
			}
		}
	}
	return problems
}
