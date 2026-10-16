package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/retrieval"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// Retrieval is the second kind of preparation (kb-design §9.7 A, §10a).
//
// A search or fetch over a knowledge product is prepared as the compiled
// relational plan "chunks ⋈ documents, caller atoms as filters, projecting the
// chunk identity and text", produced by the same derivation every query_sql
// plan goes through, with one wrapper around each of its two statements: a
// hit-set CTE that ranks the chunk vectors in the business table by exact
// cosine distance and keeps the first k. The ordinal program, the predicate
// footprint, the field lists and the sidecar grants are therefore the
// compiler's; what retrieval adds is a semi-join and a total order, both of
// which are sealed into the visible and companion target digests like any
// other statement bytes. The finalizer reproduces the preparation by calling
// PrepareRetrievalWith on the same inputs.
const (
	retrievalInputsDomain = "TASKGATE-RETRIEVAL-PREPARATION-INPUTS-V1"
	retrievalVectorDomain = "TASKGATE-RETRIEVAL-QUERY-VECTOR-V1"

	retrievalHitsAlias   = "tg_retrieval_hits"
	retrievalVisibleWrap = "tg_retrieval_visible"
	retrievalCompanWrap  = "tg_retrieval_companion"
)

// RetrievalVectorSource names the physical relation that holds the chunk
// embeddings. It is not a published relation: the vectors are not Facts
// (kb-design §1), so they live in the business table the publication was
// frozen from, and the policy grant admits that table for the hit-set CTE only.
type RetrievalVectorSource struct {
	// LogicalName is the policy-grant name the CTE references.
	LogicalName string
	Schema      string
	Relation    string
	// Collection selects the knowledge product's rows in the shared table.
	Collection       string
	CollectionColumn string
	EmbeddingColumn  string
	// EntityColumns are the chunk identity columns, in order (doc_id, version,
	// chunk_index).
	EntityColumns []string
	// AttributeColumns maps a knowledge-product attribute (the published
	// column name, e.g. kb_department) to the vector relation's column
	// (department). Attributes absent from the map keep their name.
	AttributeColumns map[string]string
}

// Validate rejects a source that cannot render a hit-set CTE.
func (source RetrievalVectorSource) Validate() error {
	for name, value := range map[string]string{
		"logical name": source.LogicalName, "schema": source.Schema, "relation": source.Relation,
		"collection": source.Collection, "collection column": source.CollectionColumn,
		"embedding column": source.EmbeddingColumn,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("retrieval vector source carries no %s", name)
		}
	}
	if !safeIdentifier(source.LogicalName) || !safeIdentifier(source.Schema) || !safeIdentifier(source.Relation) ||
		!safeIdentifier(source.CollectionColumn) || !safeIdentifier(source.EmbeddingColumn) {
		return errors.New("retrieval vector source names are not safe identifiers")
	}
	if len(source.EntityColumns) != 3 {
		return errors.New("retrieval vector source must name the three chunk identity columns")
	}
	for _, column := range source.EntityColumns {
		if !safeIdentifier(column) {
			return fmt.Errorf("retrieval vector source entity column %q is not a safe identifier", column)
		}
	}
	for attribute, column := range source.AttributeColumns {
		if !safeIdentifier(attribute) || !safeIdentifier(column) {
			return fmt.Errorf("retrieval vector source attribute mapping %q -> %q is not safe", attribute, column)
		}
	}
	return nil
}

// RetrievalPreparationInputsV1 is everything a retrieval preparation reads.
type RetrievalPreparationInputsV1 struct {
	Grant            Grant
	Catalog          CatalogView
	SnapshotBindings map[string]SnapshotBinding
	// KnowledgeProduct is the Catalog declaration the normal form was built
	// against; its chunks and documents Products are the plan's sources.
	KnowledgeProduct catalog.KnowledgeProduct
	// NormalForm is the K3.1 normal form, injected scope atoms included.
	NormalForm retrieval.NormalForm
	// QueryVector ranks the hit set. It is required for search and must be
	// absent for fetch.
	QueryVector  []float32
	VectorSource RetrievalVectorSource
}

// Validate rejects inputs that cannot be prepared deterministically.
func (inputs RetrievalPreparationInputsV1) Validate() error {
	if err := inputs.Grant.Validate(); err != nil {
		return err
	}
	if !inputs.Grant.UsesPredicateFootprint() {
		return fmt.Errorf("retrieval requires the V5 exposure profile, this task carries %q", inputs.Grant.ExposureProfile)
	}
	if err := inputs.Catalog.Validate(); err != nil {
		return err
	}
	if err := inputs.VectorSource.Validate(); err != nil {
		return err
	}
	kp := inputs.KnowledgeProduct
	if strings.TrimSpace(kp.Name) == "" || strings.TrimSpace(kp.Products.Chunks) == "" || strings.TrimSpace(kp.Products.Documents) == "" {
		return errors.New("retrieval knowledge product must name its chunks and documents products")
	}
	nf := inputs.NormalForm
	if nf.KnowledgeProduct != kp.Name {
		return fmt.Errorf("normal form names knowledge product %q, inputs carry %q", nf.KnowledgeProduct, kp.Name)
	}
	switch nf.Tool {
	case retrieval.ToolSearch:
		if nf.K <= 0 || nf.Query == nil {
			return errors.New("search normal form must carry k and a query commitment")
		}
		if len(inputs.QueryVector) == 0 {
			return errors.New("search requires a query vector")
		}
		if nf.Query.Dimension != 0 && nf.Query.Dimension != len(inputs.QueryVector) {
			return fmt.Errorf("query vector has %d dimensions, the commitment names %d", len(inputs.QueryVector), nf.Query.Dimension)
		}
	default:
		return fmt.Errorf("retrieval preparation does not prepare tool %q yet", nf.Tool)
	}
	for _, atom := range nf.Atoms {
		if atom.Injected {
			continue
		}
		if _, ok := retrievalPlanOperators[atom.Operator]; !ok {
			return fmt.Errorf("retrieval atom operator %q on %q has no plan rendering", atom.Operator, atom.Attribute)
		}
	}
	return nil
}

// retrievalPlanOperators maps normal-form operators onto plan filter operators.
var retrievalPlanOperators = map[string]string{
	"=": "=", "<>": "<>", "<": "<", "<=": "<=", ">": ">", ">=": ">=", "in": "IN",
}

// QueryVectorSHA256 is the digest of the vector literal exactly as rendered
// into the statements.
func (inputs RetrievalPreparationInputsV1) QueryVectorSHA256() string {
	sum := sha256.Sum256([]byte(retrievalVectorDomain + "\x00" + vectorLiteral(inputs.QueryVector)))
	return hex.EncodeToString(sum[:])
}

// NormalFormSHA256 is h_N.
func (inputs RetrievalPreparationInputsV1) NormalFormSHA256() (string, error) {
	return inputs.NormalForm.Digest()
}

// PlanInputs is the equivalent relational preparation the retrieval wraps.
func (inputs RetrievalPreparationInputsV1) PlanInputs() (PreparationInputs, error) {
	plan, err := retrievalPlan(inputs.KnowledgeProduct, inputs.NormalForm)
	if err != nil {
		return PreparationInputs{}, err
	}
	return PreparationInputs{Plan: plan, Grant: inputs.Grant, Catalog: inputs.Catalog,
		SnapshotBindings: inputs.SnapshotBindings}, nil
}

// SHA256 is the retrieval input digest sealed as PreparationInputsSHA256.
func (inputs RetrievalPreparationInputsV1) SHA256() (string, error) {
	if err := inputs.Validate(); err != nil {
		return "", err
	}
	planInputs, err := inputs.PlanInputs()
	if err != nil {
		return "", err
	}
	planDigest, err := planInputs.SHA256()
	if err != nil {
		return "", err
	}
	normalForm, err := inputs.NormalFormSHA256()
	if err != nil {
		return "", err
	}
	kp, err := approval.CanonicalJSON(inputs.KnowledgeProduct)
	if err != nil {
		return "", err
	}
	return domainDigest(retrievalInputsDomain, struct {
		PlanInputsSHA256     string                `json:"plan_inputs_sha256"`
		NormalFormSHA256     string                `json:"normal_form_sha256"`
		QueryVectorSHA256    string                `json:"query_vector_sha256"`
		KnowledgeProductJSON json.RawMessage       `json:"knowledge_product"`
		VectorSource         RetrievalVectorSource `json:"vector_source"`
		CatalogContentSHA256 string                `json:"catalog_content_sha256,omitempty"`
	}{planDigest, normalForm, inputs.QueryVectorSHA256(), kp, canonicalVectorSource(inputs.VectorSource), ""})
}

func canonicalVectorSource(source RetrievalVectorSource) RetrievalVectorSource {
	out := source
	out.EntityColumns = append([]string(nil), source.EntityColumns...)
	out.AttributeColumns = cloneStringMap(source.AttributeColumns)
	return out
}

// PrepareRetrieval derives a retrieval operation's statements from immutable
// inputs with this binary's compiler identity.
func PrepareRetrieval(inputs RetrievalPreparationInputsV1) (PreparedOperation, error) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		return PreparedOperation{}, err
	}
	return PrepareRetrievalWith(inputs, compiler)
}

// PrepareRetrievalWith is PrepareRetrieval against a stated compiler identity,
// for a finalizer reproducing a preparation made by another binary.
func PrepareRetrievalWith(inputs RetrievalPreparationInputsV1, compiler CompilerIdentityV1) (PreparedOperation, error) {
	if err := inputs.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	planInputs, err := inputs.PlanInputs()
	if err != nil {
		return PreparedOperation{}, err
	}
	if err := planInputs.Validate(); err != nil {
		return PreparedOperation{}, err
	}
	draft, err := derivePreparation(planInputs)
	if err != nil {
		return PreparedOperation{}, fmt.Errorf("retrieval plan: %w", err)
	}
	if draft.OrdinalProgram == nil || draft.PredicateFootprint == nil {
		return PreparedOperation{}, errors.New("retrieval plan prepared no ordinal program or predicate footprint")
	}
	if err := wrapRetrievalDraft(&draft, inputs); err != nil {
		return PreparedOperation{}, err
	}
	identities, err := planInputs.identities()
	if err != nil {
		return PreparedOperation{}, err
	}
	// The input digest names the retrieval inputs, not the bare plan: the
	// vector, the normal form and the vector source all change the statements.
	if identities.inputs, err = inputs.SHA256(); err != nil {
		return PreparedOperation{}, err
	}
	return newPreparedOperation(identities, compiler, draft)
}

// RequireRetrievalInputs is RequireInputs for a retrieval operation.
func (prepared PreparedOperation) RequireRetrievalInputs(inputs RetrievalPreparationInputsV1, compiler CompilerIdentityV1) error {
	if err := prepared.Validate(); err != nil {
		return err
	}
	if err := compiler.Validate(); err != nil {
		return err
	}
	inputsDigest, err := inputs.SHA256()
	if err != nil {
		return err
	}
	planInputs, err := inputs.PlanInputs()
	if err != nil {
		return err
	}
	grantDigest, err := planInputs.Grant.SHA256()
	if err != nil {
		return err
	}
	planDigest, err := planInputs.PlanSHA256()
	if err != nil {
		return err
	}
	snapshotDigest, err := planInputs.SnapshotBindingSetSHA256()
	if err != nil {
		return err
	}
	normalForm, err := inputs.NormalFormSHA256()
	if err != nil {
		return err
	}
	binding := prepared.binding
	for _, member := range []struct {
		name         string
		wanted, held string
	}{
		{"retrieval inputs", inputsDigest, binding.PreparationInputsSHA256},
		{"grant", grantDigest, binding.GrantSHA256},
		{"catalog", inputs.Catalog.Digest, binding.CatalogSHA256},
		{"plan", planDigest, binding.PlanSHA256},
		{"snapshot binding set", snapshotDigest, binding.SnapshotBindingSetSHA256},
		{"compiler identity", compiler.SHA256, binding.CompilerIdentitySHA256},
		{"normal form", normalForm, binding.NormalFormSHA256},
	} {
		if member.wanted != member.held {
			return fmt.Errorf("prepared retrieval does not name these %s", member.name)
		}
	}
	return nil
}

// retrievalPlan is the equivalent relational plan: chunks ⋈ documents on the
// document identity, the chunk identity and text projected, caller atoms as
// filters. Injected scope atoms are not plan filters: the policy renderer
// applies the task's mandatory scope to every relation, the vector relation
// included, exactly as it does for query_sql.
func retrievalPlan(kp catalog.KnowledgeProduct, nf retrieval.NormalForm) (queryplan.QueryPlan, error) {
	chunks, documents := kp.Products.Chunks, kp.Products.Documents
	plan := queryplan.QueryPlan{
		From: &queryplan.From{Join: &queryplan.Join{
			Left:  queryplan.Scan{Product: chunks, Role: chunks},
			Right: queryplan.Scan{Product: documents, Role: documents},
			On: []queryplan.JoinPredicate{
				{Left: chunks + ".doc_id", Right: documents + ".doc_id"},
				{Left: chunks + ".version", Right: documents + ".version"},
			},
		}},
		Columns: retrievalColumnIDs(chunks),
	}
	filters, err := retrievalPlanFilters(chunks, nf)
	if err != nil {
		return queryplan.QueryPlan{}, err
	}
	plan.Filters = filters
	return plan, nil
}

// retrievalColumnIDs is the projection, in order: identity then text.
func retrievalColumnIDs(chunks string) []string {
	return []string{chunks + ".doc_id", chunks + ".version", chunks + ".chunk_index", chunks + ".text"}
}

// retrievalPlanFilters folds caller atoms into plan filters. Atoms that the
// normalizer produced from one IN list share attribute and operator "in" and
// fold back into one IN filter; every other atom is one filter. The order is
// canonical (attribute, operator, literal) so the plan is a function of the
// normal form alone.
func retrievalPlanFilters(chunks string, nf retrieval.NormalForm) ([]queryplan.Filter, error) {
	type key struct{ attribute, operator string }
	grouped := map[key][]retrieval.Atom{}
	var order []key
	for _, atom := range nf.Atoms {
		if atom.Injected {
			continue
		}
		k := key{atom.Attribute, atom.Operator}
		if _, seen := grouped[k]; !seen {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], atom)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].attribute != order[j].attribute {
			return order[i].attribute < order[j].attribute
		}
		return order[i].operator < order[j].operator
	})
	filters := make([]queryplan.Filter, 0, len(order))
	for _, k := range order {
		op, ok := retrievalPlanOperators[k.operator]
		if !ok {
			return nil, fmt.Errorf("retrieval atom operator %q has no plan rendering", k.operator)
		}
		atoms := grouped[k]
		sort.Slice(atoms, func(i, j int) bool { return atoms[i].CanonicalLiteral < atoms[j].CanonicalLiteral })
		column := chunks + "." + k.attribute
		if op == "IN" {
			values := make([]any, 0, len(atoms))
			for _, atom := range atoms {
				value, err := retrievalFilterValue(atom)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			filters = append(filters, queryplan.Filter{Column: column, Op: op, Value: values})
			continue
		}
		for _, atom := range atoms {
			value, err := retrievalFilterValue(atom)
			if err != nil {
				return nil, err
			}
			filters = append(filters, queryplan.Filter{Column: column, Op: op, Value: value})
		}
	}
	return filters, nil
}

// retrievalFilterValue types the canonical literal the way a JSON plan would
// carry it, so the compiler's footprint canonicalizes it to the same atom.
func retrievalFilterValue(atom retrieval.Atom) (any, error) {
	switch atom.Type {
	case "numeric":
		if _, err := strconv.ParseFloat(atom.CanonicalLiteral, 64); err != nil {
			return nil, fmt.Errorf("atom on %q carries non-numeric literal %q", atom.Attribute, atom.CanonicalLiteral)
		}
		return json.Number(atom.CanonicalLiteral), nil
	case "enum", "text", "date":
		return atom.CanonicalLiteral, nil
	default:
		return nil, fmt.Errorf("atom type %q on %q has no plan literal", atom.Type, atom.Attribute)
	}
}

// wrapRetrievalDraft replaces the compiled statements by their hit-set
// wrappers, binds the normal form, and admits the vector relation.
func wrapRetrievalDraft(draft *OperationDraft, inputs RetrievalPreparationInputsV1) error {
	kp := inputs.KnowledgeProduct
	chunks := kp.Products.Chunks
	hits, err := renderRetrievalHits(inputs)
	if err != nil {
		return err
	}
	// Visible: the compiled projection, in its order, joined to the hits.
	ids := retrievalColumnIDs(chunks)
	aliases := make([]string, 0, len(ids))
	for _, id := range ids {
		aliases = append(aliases, queryplan.RelationalOutputAlias(id))
	}
	entity := inputs.VectorSource.EntityColumns
	var visible strings.Builder
	visible.WriteString("WITH " + quoteIdent(retrievalHitsAlias) + " AS (" + hits + ") SELECT ")
	for i, alias := range aliases {
		if i > 0 {
			visible.WriteString(", ")
		}
		visible.WriteString(quoteIdent(retrievalVisibleWrap) + "." + quoteIdent(alias))
	}
	visible.WriteString(" FROM (" + draft.VisibleSQL + ") AS " + quoteIdent(retrievalVisibleWrap) +
		" INNER JOIN " + quoteIdent(retrievalHitsAlias) + " ON ")
	for i := 0; i < 3; i++ {
		if i > 0 {
			visible.WriteString(" AND ")
		}
		visible.WriteString(quoteIdent(retrievalVisibleWrap) + "." + quoteIdent(aliases[i]) + " = " +
			quoteIdent(retrievalHitsAlias) + "." + quoteIdent(entity[i]))
	}
	visible.WriteString(retrievalHitOrder(entity))
	// Companion: the compiled provenance projection joined on the chunk
	// identity evidence columns.
	keys := []string{"tg_" + chunks + "_doc_id", "tg_" + chunks + "_version", "tg_" + chunks + "_chunk_index"}
	present := map[string]bool{}
	for _, field := range draft.ProvenanceFields {
		present[field] = true
	}
	for _, key := range keys {
		if !present[key] {
			return fmt.Errorf("compiled provenance projects no %q, retrieval cannot join its hit set", key)
		}
	}
	var companion strings.Builder
	companion.WriteString("WITH " + quoteIdent(retrievalHitsAlias) + " AS (" + hits + ") SELECT ")
	for i, field := range draft.ProvenanceFields {
		if i > 0 {
			companion.WriteString(", ")
		}
		companion.WriteString(quoteIdent(retrievalCompanWrap) + "." + quoteIdent(field))
	}
	companion.WriteString(" FROM (" + draft.CompanionSQL + ") AS " + quoteIdent(retrievalCompanWrap) +
		" INNER JOIN " + quoteIdent(retrievalHitsAlias) + " ON ")
	for i := 0; i < 3; i++ {
		if i > 0 {
			companion.WriteString(" AND ")
		}
		companion.WriteString(quoteIdent(retrievalCompanWrap) + "." + quoteIdent(keys[i]) + " = " +
			quoteIdent(retrievalHitsAlias) + "." + quoteIdent(entity[i]))
	}
	companion.WriteString(retrievalHitOrder(entity))

	draft.VisibleSQL = visible.String()
	draft.CompanionSQL = companion.String()
	// The relational compilation describes what executes; Validate ties its
	// visible statement to the one about to run.
	if draft.RelationalCompilation != nil {
		compilation := *draft.RelationalCompilation
		compilation.VisibleSQL = draft.VisibleSQL
		compilation.ProvenanceSQL = draft.CompanionSQL
		draft.RelationalCompilation = &compilation
	}
	normalForm, err := inputs.NormalFormSHA256()
	if err != nil {
		return err
	}
	draft.NormalFormSHA256 = normalForm
	draft.EstimatedBaseFacts = uint64(inputs.NormalForm.K)

	vectorGrant, err := retrievalVectorGrant(inputs)
	if err != nil {
		return err
	}
	draft.SidecarGrants = append(draft.SidecarGrants, vectorGrant)
	if draft.PolicyGrant, err = widenForSidecars(draft.PolicyGrant, []sqlpolicy.ProductGrant{vectorGrant}); err != nil {
		return err
	}
	return nil
}

// retrievalHitOrder is the total order both wrappers share: rank, then the
// chunk identity, so the two statements agree on the set and on ties.
func retrievalHitOrder(entity []string) string {
	return " ORDER BY " + quoteIdent(retrievalHitsAlias) + "." + quoteIdent("tg_distance") + " ASC, " +
		quoteIdent(retrievalHitsAlias) + "." + quoteIdent(entity[0]) + " ASC, " +
		quoteIdent(retrievalHitsAlias) + "." + quoteIdent(entity[1]) + " ASC, " +
		quoteIdent(retrievalHitsAlias) + "." + quoteIdent(entity[2]) + " ASC"
}

// renderRetrievalHits renders the hit-set CTE body over the vector relation:
// exact cosine ranking, the caller atoms on the vector relation's columns, a
// total order and LIMIT k. The mandatory scope is not rendered here; the
// policy renderer applies it to the vector relation through its grant.
func renderRetrievalHits(inputs RetrievalPreparationInputsV1) (string, error) {
	source := inputs.VectorSource
	entity := source.EntityColumns
	vector := vectorLiteral(inputs.QueryVector)
	distance := quoteIdent(source.EmbeddingColumn) + " <=> " + sqlStringLiteral(vector)
	var b strings.Builder
	// The identity columns keep their names: the policy analyzer resolves a
	// CTE's columns by their source names, and only an expression may carry an
	// alias of its own.
	b.WriteString("SELECT " + quoteIdent(entity[0]) + ", " + quoteIdent(entity[1]) + ", " + quoteIdent(entity[2]) + ", " +
		distance + " AS " + quoteIdent("tg_distance") +
		" FROM " + quoteIdent(source.LogicalName) +
		" WHERE " + quoteIdent(source.CollectionColumn) + " = " + sqlStringLiteral(source.Collection))
	filters, err := retrievalPlanFilters(inputs.KnowledgeProduct.Products.Chunks, inputs.NormalForm)
	if err != nil {
		return "", err
	}
	prefix := inputs.KnowledgeProduct.Products.Chunks + "."
	for _, filter := range filters {
		attribute := strings.TrimPrefix(filter.Column, prefix)
		column := attribute
		if mapped, ok := source.AttributeColumns[attribute]; ok {
			column = mapped
		}
		if !safeIdentifier(column) {
			return "", fmt.Errorf("retrieval attribute %q maps to unsafe column %q", attribute, column)
		}
		switch filter.Op {
		case "IN":
			values, _ := filter.Value.([]any)
			literals := make([]string, 0, len(values))
			for _, value := range values {
				literal, err := retrievalLiteral(value)
				if err != nil {
					return "", err
				}
				literals = append(literals, literal)
			}
			b.WriteString(" AND " + quoteIdent(column) + " IN (" + strings.Join(literals, ", ") + ")")
		default:
			literal, err := retrievalLiteral(filter.Value)
			if err != nil {
				return "", err
			}
			b.WriteString(" AND " + quoteIdent(column) + " " + filter.Op + " " + literal)
		}
	}
	b.WriteString(" ORDER BY " + distance + " ASC, " + quoteIdent(entity[0]) + " ASC, " +
		quoteIdent(entity[1]) + " ASC, " + quoteIdent(entity[2]) + " ASC LIMIT " + strconv.Itoa(inputs.NormalForm.K))
	return b.String(), nil
}

// retrievalVectorGrant admits the vector relation to the policy grant with the
// task's mandatory scope mapped onto its column names and the distance
// operator admitted.
func retrievalVectorGrant(inputs RetrievalPreparationInputsV1) (sqlpolicy.ProductGrant, error) {
	source := inputs.VectorSource
	var scope map[string]any
	if len(inputs.Grant.MandatoryScope) > 0 {
		if err := json.Unmarshal(inputs.Grant.MandatoryScope, &scope); err != nil {
			return sqlpolicy.ProductGrant{}, fmt.Errorf("preparation grant mandatory scope is not JSON: %w", err)
		}
	}
	// Scope predicates are derived by the same helper the published products
	// use, against the knowledge product's scoped attributes, then renamed to
	// the vector relation's columns.
	scoped := catalog.Product{}
	for _, attribute := range inputs.KnowledgeProduct.Attributes {
		if attribute.Scope != "" {
			scoped.Fields = append(scoped.Fields, catalog.Field{Name: attribute.Scope})
		}
	}
	for _, name := range inputs.KnowledgeProduct.Scopes {
		if _, present := scope[name]; !present {
			return sqlpolicy.ProductGrant{}, fmt.Errorf("this task carries no value for mandatory scope %q of knowledge product %q", name, inputs.KnowledgeProduct.Name)
		}
	}
	predicates, err := scopePredicates(scoped, scope)
	if err != nil {
		return sqlpolicy.ProductGrant{}, err
	}
	columns := map[string]struct{}{source.CollectionColumn: {}, source.EmbeddingColumn: {}}
	for _, column := range source.EntityColumns {
		columns[column] = struct{}{}
	}
	for i := range predicates {
		// A scope keyed by the scope name applies to the attribute carrying it.
		column := predicates[i].Column
		for _, attribute := range inputs.KnowledgeProduct.Attributes {
			if attribute.Scope == predicates[i].Column {
				column = attribute.Name
			}
		}
		if mapped, ok := source.AttributeColumns[column]; ok {
			column = mapped
		}
		predicates[i].Column = column
		columns[column] = struct{}{}
	}
	for _, atom := range inputs.NormalForm.Atoms {
		column := atom.Attribute
		if mapped, ok := source.AttributeColumns[column]; ok {
			column = mapped
		}
		columns[column] = struct{}{}
	}
	approved := make([]string, 0, len(columns))
	for column := range columns {
		approved = append(approved, column)
	}
	sort.Strings(approved)
	return sqlpolicy.ProductGrant{
		LogicalName: source.LogicalName, PhysicalSchema: source.Schema, PhysicalView: source.Relation,
		ApprovedColumns:  approved,
		AllowedOperators: []string{"=", "<>", "<", "<=", ">", ">=", "IN", "<=>"},
		MandatoryScope:   predicates,
	}, nil
}

// vectorLiteral renders the pgvector input form. Each component is the
// shortest decimal that round-trips the float32, so two processes holding the
// same vector render the same bytes.
func vectorLiteral(vector []float32) string {
	parts := make([]string, 0, len(vector))
	for _, value := range vector {
		parts = append(parts, strconv.FormatFloat(float64(value), 'g', -1, 32))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func retrievalLiteral(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return sqlStringLiteral(typed), nil
	case json.Number:
		if _, err := strconv.ParseFloat(string(typed), 64); err != nil {
			return "", errors.New("numeric literal is invalid")
		}
		return string(typed), nil
	default:
		return "", fmt.Errorf("literal type %T is not supported in a retrieval hit set", value)
	}
}

func sqlStringLiteral(text string) string {
	escaped := strings.ReplaceAll(text, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "'", "''")
	return "E'" + escaped + "'"
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func safeIdentifier(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
