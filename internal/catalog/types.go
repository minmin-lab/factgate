package catalog

import (
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

// Catalog is the complete, versioned logical data contract loaded at startup.
type Catalog struct {
	CatalogVersion string `yaml:"catalog_version" json:"catalog_version"`
	// SHA256 is the lowercase digest of the exact validated catalog artifact
	// bytes. It is carried into manifests and receipts so formatting or policy
	// edits necessarily create a new authorization binding.
	SHA256               string                `yaml:"-" json:"catalog_sha256"`
	Sources              []Source              `yaml:"sources" json:"sources"`
	SnapshotPublications []SnapshotPublication `yaml:"snapshot_publications,omitempty" json:"snapshot_publications,omitempty"`
	Products             []Product             `yaml:"products" json:"products"`
	Scopes               []Scope               `yaml:"scopes" json:"scopes"`
	ApprovalRoutes       []ApprovalRoute       `yaml:"approval_routes" json:"approval_routes"`
	BudgetProfiles       []BudgetProfile       `yaml:"budget_profiles" json:"budget_profiles"`
	// KnowledgeProducts declare governed document collections whose frozen
	// relations are ordinary snapshot-publication Products (docs/kb-design.md).
	KnowledgeProducts []KnowledgeProduct `yaml:"knowledge_products,omitempty" json:"knowledge_products,omitempty"`
	// KBPublications bind each knowledge product's index publication epoch to
	// the digest-bound manifest cmd/kb-publish produced.
	KBPublications []KBPublication `yaml:"kb_publications,omitempty" json:"kb_publications,omitempty"`
	// DelegatedAuthorization switches on the demo's long-lived authorization
	// model (delegated.go); absent in the formal round.
	DelegatedAuthorization *DelegatedAuthorization `yaml:"delegated_authorization,omitempty" json:"delegated_authorization,omitempty"`
}

// V4Enabled reports the deployment mode selected by the validated Catalog.
// Validation guarantees that snapshot publications and every reachable
// approval route agree on this mode; scanning both keeps the method
// fail-closed for callers holding a manually constructed Catalog in tests.
func (c *Catalog) V4Enabled() bool {
	if c == nil {
		return false
	}
	if len(c.SnapshotPublications) != 0 {
		return true
	}
	profiles := make(map[string]string, len(c.BudgetProfiles))
	for _, profile := range c.BudgetProfiles {
		profiles[profile.Name] = profile.ExposureProfileVersion
	}
	for _, route := range c.ApprovalRoutes {
		if profiles[route.BudgetProfile] == exposureProfileV4 || profiles[route.BudgetProfile] == exposureProfileV5 {
			return true
		}
	}
	return false
}

// SnapshotPublication binds an immutable business snapshot to the exact V4
// ordinal sidecar and content-addressed dictionary artifacts approved by the
// Catalog. Digests are verified again when SnapshotRegistry resolves it.
type SnapshotPublication struct {
	Name             string `yaml:"name" json:"name"`
	Source           string `yaml:"source" json:"source"`
	SourceNamespace  string `yaml:"source_namespace" json:"source_namespace"`
	Snapshot         string `yaml:"snapshot" json:"snapshot"`
	OrdinalSidecar   string `yaml:"ordinal_sidecar" json:"ordinal_sidecar"`
	SidecarDigest    string `yaml:"sidecar_digest" json:"sidecar_digest"`
	DictionaryDigest string `yaml:"dictionary_digest" json:"dictionary_digest"`
	ManifestDigest   string `yaml:"manifest_digest" json:"manifest_digest"`
}

type Source struct {
	Name                   string `yaml:"name" json:"name"`
	DatasourceID           string `yaml:"datasource_id" json:"datasource_id"`
	Type                   string `yaml:"type" json:"type"`
	Address                string `yaml:"address" json:"address"`
	Port                   int    `yaml:"port" json:"port"`
	Database               string `yaml:"database" json:"database"`
	User                   string `yaml:"user" json:"user"`
	PostgreSQLMajorVersion int    `yaml:"postgres_major_version" json:"postgres_major_version"`
	SchemaDigest           string `yaml:"schema_digest" json:"schema_digest"`
	SecretRef              string `yaml:"secretRef" json:"secret_ref"`

	// These fields exist only so a strict decoder can return a deliberate,
	// redacted security error instead of treating secret-bearing input as an
	// ordinary unknown field.
	Password string `yaml:"password,omitempty" json:"-"`
	DSN      string `yaml:"dsn,omitempty" json:"-"`
}

type Product struct {
	Name                string             `yaml:"name" json:"name"`
	Source              string             `yaml:"source" json:"source"`
	ReportingView       string             `yaml:"reporting_view" json:"reporting_view"`
	Description         string             `yaml:"description" json:"description"`
	Sensitivity         domain.Sensitivity `yaml:"sensitivity" json:"sensitivity"`
	Fields              []Field            `yaml:"fields" json:"fields"`
	Scopes              []string           `yaml:"scopes" json:"scopes"`
	AllowedFunctions    []string           `yaml:"allowed_functions,omitempty" json:"allowed_functions,omitempty"`
	AllowedOperators    []string           `yaml:"allowed_operators,omitempty" json:"allowed_operators,omitempty"`
	AllowedAggregates   []string           `yaml:"allowed_aggregates,omitempty" json:"allowed_aggregates,omitempty"`
	Snapshot            string             `yaml:"snapshot" json:"snapshot"`
	SnapshotPublication string             `yaml:"snapshot_publication,omitempty" json:"snapshot_publication,omitempty"`
	EntityKey           []string           `yaml:"entity_key" json:"entity_key"`
	// FactNamespace is the Catalog-owned canonical semantic relation, not the
	// logical reporting view name. StableRelationRole disambiguates self-joins.
	FactNamespace      string `yaml:"fact_namespace,omitempty" json:"fact_namespace,omitempty"`
	StableRelationRole string `yaml:"stable_relation_role,omitempty" json:"stable_relation_role,omitempty"`
	// A derived/generalized product must pin its trusted base-lineage manifest.
	LineageManifestDigest string `yaml:"lineage_manifest_digest,omitempty" json:"lineage_manifest_digest,omitempty"`
	// ViewContract enables dependency-aware semantic View governance. It is
	// optional so existing immutable publication products retain their exact
	// Catalog and authorization encodings. When present, all four digests are
	// generated from one PostgreSQL registry snapshot and validated together.
	ViewContract *ViewContract `yaml:"view_contract,omitempty" json:"view_contract,omitempty"`
	// Eligibility compiles the requesting subject's attested attributes into
	// the envelope a request may ask for before any approval route is
	// consulted. A request outside the envelope is rejected and never reaches
	// an approver; an approver can only narrow a request inside it.
	Eligibility *Eligibility `yaml:"eligibility,omitempty" json:"eligibility,omitempty"`
	// StewardDepartment names the department that stewards this product's
	// records company-wide (KB1-LOOP-148, the delegated-authorization demo;
	// for example hr for personal records). A task whose every product
	// declares one is routed, for the departments outside the requester's
	// authority, to the steward department instead (ApprovalChain.StewardScope).
	StewardDepartment string `yaml:"steward_department,omitempty" json:"steward_department,omitempty"`
}

const ViewContractV1 = "taskgate-view-contract-v1"

// ViewContract pins one governed semantic View to its exact source revision,
// transitive dependency closure, expanded typed plan, and ordered output
// interface. PostgreSQL OIDs and SQL aliases are deliberately absent.
// Eligibility is the per-Product predicate over subject attributes that
// bounds what may be requested (author decision 2026-10-15: requests outside
// the envelope are rejected outright, not escalated).
type Eligibility struct {
	// Require lists conditions that must all hold for the requesting subject.
	Require []AttributeCondition `yaml:"require,omitempty" json:"require,omitempty"`
	// ScopeEnvelope restricts an enum scope of this Product to the subject's
	// own attribute values unless a widening condition holds.
	ScopeEnvelope map[string]ScopeEnvelopeRule `yaml:"scope_envelope,omitempty" json:"scope_envelope,omitempty"`
}

// AttributeCondition holds when the subject carries at least one of the
// listed values for the attribute at request time.
type AttributeCondition struct {
	Attribute string   `yaml:"attribute" json:"attribute"`
	AnyOf     []string `yaml:"any_of" json:"any_of"`
}

// ScopeEnvelopeRule derives the admissible values of one enum scope from a
// subject attribute. WidenIf conditions admit every Catalog-allowed value.
type ScopeEnvelopeRule struct {
	FromAttribute string               `yaml:"from_attribute" json:"from_attribute"`
	WidenIf       []AttributeCondition `yaml:"widen_if,omitempty" json:"widen_if,omitempty"`
}

type ViewContract struct {
	ProfileVersion      string `yaml:"profile_version" json:"profile_version"`
	DefinitionDigest    string `yaml:"definition_digest" json:"definition_digest"`
	DependencyDigest    string `yaml:"dependency_digest" json:"dependency_digest"`
	CanonicalPlanDigest string `yaml:"canonical_plan_digest" json:"canonical_plan_digest"`
	InterfaceDigest     string `yaml:"interface_digest" json:"interface_digest"`
}

type Field struct {
	Name             string             `yaml:"name" json:"name"`
	Type             string             `yaml:"type" json:"type"`
	Description      string             `yaml:"description" json:"description"`
	Sensitivity      domain.Sensitivity `yaml:"sensitivity,omitempty" json:"sensitivity,omitempty"`
	Collation        string             `yaml:"collation,omitempty" json:"collation,omitempty"`
	CollationVersion string             `yaml:"collation_version,omitempty" json:"collation_version,omitempty"`
}

type Scope struct {
	Name          string   `yaml:"name" json:"name"`
	Type          string   `yaml:"type" json:"type"`
	Description   string   `yaml:"description" json:"description"`
	AllowedValues []string `yaml:"allowed_values,omitempty" json:"allowed_values,omitempty"`
	Min           string   `yaml:"min,omitempty" json:"min,omitempty"`
	Max           string   `yaml:"max,omitempty" json:"max,omitempty"`
}

const (
	ScopeTypeEnum      = "enum"
	ScopeTypeDateRange = "date_range"
)

type ApprovalRoute struct {
	Sensitivity domain.Sensitivity `yaml:"sensitivity" json:"sensitivity"`
	// Products optionally binds this route to one exact sorted product set.
	// Product-scoped routes are fail-closed: a named product cannot be mixed
	// into another set and fall back to the sensitivity-wide route.
	Products      []string            `yaml:"products,omitempty" json:"products,omitempty"`
	Mode          domain.ApprovalMode `yaml:"mode" json:"mode"`
	Approver      string              `yaml:"approver,omitempty" json:"approver,omitempty"`
	BudgetProfile string              `yaml:"budget_profile" json:"budget_profile"`
	// ScopeCondition optionally restricts the route to tasks whose mandatory
	// scope holds at least one listed value of a Catalog enum scope (for
	// example classification: [leadership]). A conditioned route needs an
	// unconditioned route for the same sensitivity or product set, takes
	// precedence over it in declared order, and decides who approves (and
	// under which budget), never what is granted.
	ScopeCondition map[string][]string `yaml:"scope_condition,omitempty" json:"scope_condition,omitempty"`
	// VerifyApprover requires the approver to be a subject of the attested
	// attribute snapshot and to be eligible, under the same envelopes as a
	// requester, for everything the task requests: no one approves data they
	// could not request themselves. Off by default, so catalogs whose
	// approvers are not in the snapshot keep their behaviour.
	VerifyApprover bool `yaml:"verify_approver,omitempty" json:"verify_approver,omitempty"`
	// Chain routes the task through an org-chart approval chain instead of a
	// single approver (approval_chain.go). A chain route names no approver
	// and no scope condition; it always verifies each approver's authority.
	Chain *ApprovalChain `yaml:"chain,omitempty" json:"chain,omitempty"`
}

// routeKey identifies the routes that compete for one request: the same
// sensitivity and the same exact product set (empty for sensitivity-wide).
func (route ApprovalRoute) routeKey() string {
	return string(route.Sensitivity) + "\x00" + strings.Join(route.Products, "\x00")
}

// Matches reports whether the route's scope condition holds for a task's
// mandatory scope; an unconditioned route always matches.
func (route ApprovalRoute) Matches(scope map[string][]string) bool {
	for name, values := range route.ScopeCondition {
		requested := make(map[string]struct{}, len(scope[name]))
		for _, value := range scope[name] {
			requested[value] = struct{}{}
		}
		matched := false
		for _, value := range values {
			if _, ok := requested[value]; ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

type BudgetProfile struct {
	Name                   string                             `yaml:"name" json:"name"`
	MaxQueries             int64                              `yaml:"max_queries" json:"max_queries"`
	MaxRows                int64                              `yaml:"max_rows" json:"max_rows"`
	MaxDBTime              Duration                           `yaml:"max_db_time" json:"max_db_time"`
	QueryTimeout           Duration                           `yaml:"query_timeout" json:"query_timeout"`
	TaskTTL                Duration                           `yaml:"task_ttl" json:"task_ttl"`
	MaxReleaseFacts        int64                              `yaml:"max_release_facts,omitempty" json:"max_release_facts,omitempty"`
	MaxInfluenceFacts      int64                              `yaml:"max_influence_facts,omitempty" json:"max_influence_facts,omitempty"`
	MaxOutcomeFacts        int64                              `yaml:"max_outcome_facts,omitempty" json:"max_outcome_facts,omitempty"`
	ExposureProfileVersion string                             `yaml:"exposure_profile_version,omitempty" json:"exposure_profile_version,omitempty"`
	PredicateFootprint     *domain.PredicateFootprintLimitsV1 `yaml:"predicate_footprint,omitempty" json:"predicate_footprint,omitempty"`
}

func (p BudgetProfile) Budget() domain.Budget {
	return domain.Budget{
		MaxQueries:      p.MaxQueries,
		MaxRows:         p.MaxRows,
		MaxDBTime:       p.MaxDBTime.Duration,
		PerQueryTimeout: p.QueryTimeout.Duration,
		TaskTTL:         p.TaskTTL.Duration,
		MaxReleaseFacts: p.MaxReleaseFacts, MaxInfluenceFacts: p.MaxInfluenceFacts, MaxOutcomeFacts: p.MaxOutcomeFacts,
		ExposureProfileVersion: p.ExposureProfileVersion,
		PredicateFootprint:     clonePredicateFootprintLimits(p.PredicateFootprint),
	}
}

func clonePredicateFootprintLimits(input *domain.PredicateFootprintLimitsV1) *domain.PredicateFootprintLimitsV1 {
	if input == nil {
		return nil
	}
	copy := *input
	return &copy
}

// EffectiveSensitivity includes field-level classifications so a product can
// never be routed below its most sensitive published field.
func (p Product) EffectiveSensitivity() (domain.Sensitivity, error) {
	values := []domain.Sensitivity{p.Sensitivity}
	for _, field := range p.Fields {
		if field.Sensitivity != "" {
			values = append(values, field.Sensitivity)
		}
	}
	return domain.HighestSensitivity(values...)
}

func (p Product) FieldNames() []string {
	names := make([]string, 0, len(p.Fields))
	for _, field := range p.Fields {
		names = append(names, field.Name)
	}
	return names
}

// ExpiresAt derives the immutable grant expiry from a selected profile.
func (p BudgetProfile) ExpiresAt(approvedAt time.Time) time.Time {
	return approvedAt.Add(p.TaskTTL.Duration)
}

// KnowledgeProduct is a governed document collection: three frozen relations
// (documents, chunks, extracted values) published as ordinary Products, the
// closed attribute vocabulary filter atoms may name, and the segmentation,
// embedding and extraction declarations whose digests the index publication
// epoch binds.
type KnowledgeProduct struct {
	Name             string                    `yaml:"name" json:"name"`
	Description      string                    `yaml:"description" json:"description"`
	Sensitivity      domain.Sensitivity        `yaml:"sensitivity" json:"sensitivity"`
	Collection       string                    `yaml:"collection" json:"collection"`
	SourceNamespace  string                    `yaml:"source_namespace" json:"source_namespace"`
	Products         KnowledgeProductRelations `yaml:"products" json:"products"`
	IndexPublication string                    `yaml:"index_publication" json:"index_publication"`
	DocumentTypes    []string                  `yaml:"document_types" json:"document_types"`
	Attributes       []KBAttribute             `yaml:"attributes" json:"attributes"`
	Segmentation     KBSegmentation            `yaml:"segmentation" json:"segmentation"`
	Embedding        KBEmbedding               `yaml:"embedding" json:"embedding"`
	ExtractionRules  []KBExtractionRule        `yaml:"extraction_rules,omitempty" json:"extraction_rules,omitempty"`
	Retrieval        KBRetrieval               `yaml:"retrieval" json:"retrieval"`
	Scopes           []string                  `yaml:"scopes" json:"scopes"`
	Eligibility      *Eligibility              `yaml:"eligibility,omitempty" json:"eligibility,omitempty"`
}

type KnowledgeProductRelations struct {
	Documents string `yaml:"documents" json:"documents"`
	Chunks    string `yaml:"chunks" json:"chunks"`
	Values    string `yaml:"values" json:"values"`
	// CollectionValues is the fourth relation, present exactly when some
	// extraction rule is collection-scoped: one row per distinct (field,
	// canonical value, department, classification), so a value shared by many
	// documents is one Fact within the task's mandatory scope.
	CollectionValues string `yaml:"collection_values,omitempty" json:"collection_values,omitempty"`
	// Files is the originals relation (docs/kb-design.md §12.9): one row per
	// document version with the original file's media type, size and digest.
	// Approving it is the right to download the original file.
	Files string `yaml:"files,omitempty" json:"files,omitempty"`
}

// Relations lists every relation of the knowledge product that is declared.
func (r KnowledgeProductRelations) Relations() []string {
	names := []string{r.Documents, r.Chunks, r.Values}
	for _, optional := range []string{r.CollectionValues, r.Files} {
		if optional != "" {
			names = append(names, optional)
		}
	}
	return names
}

// Extraction rule scopes. A document-scoped value is one Fact per document
// version; a collection-scoped value is one Fact per distinct value within
// the mandatory scope attributes.
const (
	KBExtractionScopeDocument   = "document"
	KBExtractionScopeCollection = "collection"
)

// KBAttribute is one filterable attribute of a collection. Scope binds it to
// a mandatory Catalog Scope so the task's mandatory scope is injected as a
// filter atom on this attribute.
type KBAttribute struct {
	Name          string   `yaml:"name" json:"name"`
	Type          string   `yaml:"type" json:"type"`
	Scope         string   `yaml:"scope,omitempty" json:"scope,omitempty"`
	AllowedValues []string `yaml:"allowed_values,omitempty" json:"allowed_values,omitempty"`
}

// KBSegmentation, KBEmbedding and KBExtractionRule share their JSON shape with
// internal/kbpublish so the digests declared in kb_publications are the
// SHA-256 of these declarations' canonical JSON.
type KBSegmentation struct {
	Function    string `yaml:"function" json:"function"`
	WindowChars int    `yaml:"window_chars" json:"window_chars"`
}

type KBEmbedding struct {
	Model     string `yaml:"model" json:"model"`
	Dimension int    `yaml:"dimension" json:"dimension"`
	// WeightsSHA256 binds a learned model's weight file; required for every
	// model except the deterministic test provider.
	WeightsSHA256 string `yaml:"weights_sha256,omitempty" json:"weights_sha256,omitempty"`
}

type KBExtractionRule struct {
	DocumentType string `yaml:"document_type" json:"document_type"`
	Field        string `yaml:"field" json:"field"`
	Type         string `yaml:"type" json:"type"`
	Source       string `yaml:"source" json:"source"`
	Scope        string `yaml:"scope" json:"scope"`
}

type KBRetrieval struct {
	KMax               int      `yaml:"k_max" json:"k_max"`
	Exact              bool     `yaml:"exact" json:"exact"`
	Aggregates         []string `yaml:"aggregates,omitempty" json:"aggregates,omitempty"`
	GenerationAdmitted bool     `yaml:"generation_admitted" json:"generation_admitted"`
	// Keyword admits keyword search (substring containment over chunk text,
	// docs/kb-design.md section 11). Omitted when false, so declarations
	// written before keyword search keep their canonical bytes.
	Keyword bool `yaml:"keyword,omitempty" json:"keyword,omitempty"`
}

// KBPublication binds an index publication epoch to its kb-publish manifest.
type KBPublication struct {
	Epoch                 string `yaml:"epoch" json:"epoch"`
	KnowledgeProduct      string `yaml:"knowledge_product" json:"knowledge_product"`
	ManifestDigest        string `yaml:"manifest_digest" json:"manifest_digest"`
	SegmentationDigest    string `yaml:"segmentation_digest" json:"segmentation_digest"`
	EmbeddingDigest       string `yaml:"embedding_digest" json:"embedding_digest"`
	ExtractionRulesDigest string `yaml:"extraction_rules_digest" json:"extraction_rules_digest"`
}

const (
	KBSegmentationSectionWindowV1 = "section-window-v1"
	// KBEmbeddingHashProjectionV1 is the deterministic, dependency-free test
	// provider; KBEmbeddingBGEM3 is the learned model deployments use.
	KBEmbeddingHashProjectionV1 = "hash-projection-v1"
	KBEmbeddingBGEM3            = "bge-m3"
)

// LookupKnowledgeProduct returns the knowledge product with the given name.
func (c *Catalog) LookupKnowledgeProduct(name string) (KnowledgeProduct, bool) {
	for _, product := range c.KnowledgeProducts {
		if product.Name == name {
			return product, true
		}
	}
	return KnowledgeProduct{}, false
}

// LookupKBPublication returns the index publication epoch binding.
func (c *Catalog) LookupKBPublication(epoch string) (KBPublication, bool) {
	for _, publication := range c.KBPublications {
		if publication.Epoch == epoch {
			return publication, true
		}
	}
	return KBPublication{}, false
}
