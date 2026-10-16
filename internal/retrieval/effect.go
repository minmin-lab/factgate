package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	ContextVersion = "taskgate-retrieval-context-v1"
	contextDomain  = "TASKGATE-RETRIEVAL-CONTEXT-V1\x00"
	// ChunkTextField is the bare column whose cell Fact is what a returned
	// chunk releases; single-source plans bind cells by bare column name.
	ChunkTextField = "text"
)

// Hit is one chunk returned to the caller by search or fetch.
type Hit struct {
	DocID      string
	Version    int
	ChunkIndex int
	Text       string
	// Attributes holds the chunk's values for the attributes the caller
	// filtered on, as canonical tokens (enum/text: the token; date:
	// YYYY-MM-DD; numeric: the decimal). The general algebra counts a chunk
	// cell for every attribute a plan filter reads (an outer_filter witness),
	// so a hit must carry those values for the effect to be computed.
	Attributes map[string]string
}

// Binding is the immutable identity the effect is computed under: the
// knowledge product's namespace and epoch, the Catalog digest, the two
// publication manifests, and the collation its text attributes carry.
type Binding struct {
	KnowledgeProduct           string
	SourceNamespace            string
	Epoch                      string
	CatalogSHA256              string
	ChunksProduct              string
	DocumentsProduct           string
	ChunksPublicationSHA256    string
	DocumentsPublicationSHA256 string
	CollationName              string
	CollationVersion           string
}

// Effect is the three-dimensional effect of one successful retrieval.
type Effect struct {
	Release                 []exposure.FactID
	Dependency              []exposure.FactID
	Atoms                   []exposure.FactID
	Composite               exposure.FactID
	PredicateContextSHA256  string
	PredicateSetSHA256      string
	ResultObservationSHA256 string
	NormalFormSHA256        string
}

// ChunkEntityKey is the canonical entity key of a chunk row, composed exactly
// as the snapshot compiler composes it for entity_key_fields
// [doc_id, version, chunk_index].
func ChunkEntityKey(namespace, docID string, version, chunkIndex int) (string, error) {
	return entityKey(namespace+".chunks", []keyField{{"doc_id", "text", docID}, {"version", "integer", int64(version)}, {"chunk_index", "integer", int64(chunkIndex)}})
}

// DocumentEntityKey is the canonical entity key of a document version row.
func DocumentEntityKey(namespace, docID string, version int) (string, error) {
	return entityKey(namespace+".documents", []keyField{{"doc_id", "text", docID}, {"version", "integer", int64(version)}})
}

type keyField struct {
	name    string
	sqlType string
	value   any
}

func entityKey(sourceNamespace string, fields []keyField) (string, error) {
	components := []string{sourceNamespace}
	for _, field := range fields {
		canonical, err := exposure.CanonicalSQLValue(field.sqlType, field.value)
		if err != nil {
			return "", err
		}
		components = append(components, field.name, field.sqlType, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("base-entity", components...)
}

type contextPayload struct {
	Version                    string `json:"version"`
	CatalogSHA256              string `json:"catalog_sha256"`
	KnowledgeProduct           string `json:"knowledge_product"`
	Epoch                      string `json:"epoch"`
	ChunksPublicationSHA256    string `json:"chunks_publication_sha256"`
	DocumentsPublicationSHA256 string `json:"documents_publication_sha256"`
	EffectiveScopeSHA256       string `json:"effective_scope_sha256"`
}

// ContextDigest binds caller atoms to the Catalog, the epoch and the injected
// mandatory scope, as the SQL path's predicate context does.
func ContextDigest(binding Binding, form NormalForm) (string, error) {
	var injected []Atom
	for _, atom := range form.Atoms {
		if atom.Injected {
			injected = append(injected, atom)
		}
	}
	if injected == nil {
		injected = []Atom{}
	}
	scopeJSON, err := json.Marshal(injected)
	if err != nil {
		return "", err
	}
	scope := sha256.Sum256(scopeJSON)
	payload := contextPayload{Version: ContextVersion, CatalogSHA256: binding.CatalogSHA256, KnowledgeProduct: binding.KnowledgeProduct,
		Epoch: binding.Epoch, ChunksPublicationSHA256: binding.ChunksPublicationSHA256,
		DocumentsPublicationSHA256: binding.DocumentsPublicationSHA256, EffectiveScopeSHA256: hex.EncodeToString(scope[:])}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(contextDomain), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func atomSQLType(attributeType string) string {
	switch attributeType {
	case "date":
		return "date"
	case "numeric":
		return "numeric"
	default:
		return "text"
	}
}

// atomOperator maps a retrieval operator onto the V5 predicate atom
// vocabulary. `in` and `has` atomize per literal as equality tests, exactly as
// the SQL path atomizes IN lists.
func atomOperator(operator string) (string, error) {
	switch operator {
	case "=", "in", "has":
		return "EQ", nil
	case "<>":
		return "NE", nil
	case "<":
		return "LT", nil
	case "<=":
		return "LE", nil
	case ">":
		return "GT", nil
	case ">=":
		return "GE", nil
	default:
		return "", fmt.Errorf("operator %q has no atom form", operator)
	}
}

func expressionDigest(namespace, attribute string) string {
	sum := sha256.Sum256([]byte(namespace + ".chunks." + attribute))
	return hex.EncodeToString(sum[:])
}

// BuildEffect computes F_R, F_D and F_O of a search or fetch whose normalized
// request is form and whose returned chunks are hits. It performs no I/O.
func BuildEffect(binding Binding, form NormalForm, hits []Hit) (Effect, error) {
	if form.Tool != ToolSearch && form.Tool != ToolFetch {
		return Effect{}, errors.New("BuildEffect covers search and fetch; aggregate settles through the values product plan")
	}
	if binding.SourceNamespace == "" || binding.Epoch == "" || binding.KnowledgeProduct == "" || binding.ChunksProduct == "" {
		return Effect{}, errors.New("retrieval binding is incomplete")
	}
	effect := Effect{}
	normalFormDigest, err := form.Digest()
	if err != nil {
		return Effect{}, err
	}
	effect.NormalFormSHA256 = normalFormDigest

	seenChunks := map[string]struct{}{}
	dependency := map[string]exposure.FactID{}
	chunksNS, documentsNS := binding.SourceNamespace+".chunks", binding.SourceNamespace+".documents"
	filtered := filteredAttributes(form)
	addDependency := func(facts ...exposure.FactID) error {
		for _, fact := range facts {
			hash, err := fact.Hash()
			if err != nil {
				return err
			}
			dependency[hash] = fact
		}
		return nil
	}
	for _, hit := range hits {
		chunkKey, err := ChunkEntityKey(binding.SourceNamespace, hit.DocID, hit.Version, hit.ChunkIndex)
		if err != nil {
			return Effect{}, err
		}
		if _, duplicate := seenChunks[chunkKey]; duplicate {
			return Effect{}, fmt.Errorf("hit %s v%d #%d is returned twice", hit.DocID, hit.Version, hit.ChunkIndex)
		}
		seenChunks[chunkKey] = struct{}{}
		// Release: the four visible cells of the chunk, under the relational
		// field identity (role.column) the join preparation gives them.
		var cells []exposure.FactID
		for _, column := range []struct {
			name, sqlType string
			value         any
		}{{"doc_id", "text", hit.DocID}, {"version", "integer", int64(hit.Version)}, {"chunk_index", "integer", int64(hit.ChunkIndex)}, {ChunkTextField, "text", hit.Text}} {
			cell, err := exposure.NewBaseCellFactV2(chunksNS, binding.Epoch, chunkKey, binding.ChunksProduct+"."+column.name, column.sqlType, column.value)
			if err != nil {
				return Effect{}, err
			}
			cells = append(cells, cell)
		}
		effect.Release = append(effect.Release, cells...)
		chunkRow, err := exposure.NewBaseRowFactV2(chunksNS, binding.Epoch, chunkKey)
		if err != nil {
			return Effect{}, err
		}
		documentKey, err := DocumentEntityKey(binding.SourceNamespace, hit.DocID, hit.Version)
		if err != nil {
			return Effect{}, err
		}
		documentRow, err := exposure.NewBaseRowFactV2(documentsNS, binding.Epoch, documentKey)
		if err != nil {
			return Effect{}, err
		}
		documentDocID, err := exposure.NewBaseCellFactV2(documentsNS, binding.Epoch, documentKey, binding.DocumentsProduct+".doc_id", "text", hit.DocID)
		if err != nil {
			return Effect{}, err
		}
		documentVersion, err := exposure.NewBaseCellFactV2(documentsNS, binding.Epoch, documentKey, binding.DocumentsProduct+".version", "integer", int64(hit.Version))
		if err != nil {
			return Effect{}, err
		}
		if err := addDependency(append(cells, chunkRow, documentRow, documentDocID, documentVersion)...); err != nil {
			return Effect{}, err
		}
		// One chunk cell per attribute the caller filtered on.
		for _, attribute := range filtered {
			raw, present := hit.Attributes[attribute.Attribute]
			if !present {
				return Effect{}, fmt.Errorf("hit %s v%d #%d carries no value for filtered attribute %q", hit.DocID, hit.Version, hit.ChunkIndex, attribute.Attribute)
			}
			sqlType, value, err := attributeCellValue(attribute.Type, raw)
			if err != nil {
				return Effect{}, fmt.Errorf("attribute %q: %w", attribute.Attribute, err)
			}
			cell, err := exposure.NewBaseCellFactV2(chunksNS, binding.Epoch, chunkKey, binding.ChunksProduct+"."+attribute.Attribute, sqlType, value)
			if err != nil {
				return Effect{}, err
			}
			if err := addDependency(cell); err != nil {
				return Effect{}, err
			}
		}
	}
	effect.Release = sortFacts(effect.Release)
	effect.Dependency = sortFactMap(dependency)

	effect.PredicateContextSHA256, err = ContextDigest(binding, form)
	if err != nil {
		return Effect{}, err
	}
	for _, atom := range form.CallerAtoms() {
		sqlType := atomSQLType(atom.Type)
		operator, err := atomOperator(atom.Operator)
		if err != nil {
			return Effect{}, err
		}
		input := exposure.PredicateAtomFactV5{PredicateContextSHA256: effect.PredicateContextSHA256,
			SemanticProductID: binding.ChunksProduct, StableRole: binding.ChunksProduct, PublicFieldID: atom.Attribute,
			ResolvedExpressionSHA256: expressionDigest(binding.SourceNamespace, atom.Attribute), SQLType: sqlType,
			Operator: operator, CanonicalLiteral: atom.CanonicalLiteral}
		if sqlType == "text" {
			input.CollationName, input.CollationVersion = binding.CollationName, binding.CollationVersion
		}
		fact, err := exposure.NewPredicateAtomFactV5(input)
		if err != nil {
			return Effect{}, fmt.Errorf("atom %s %s: %w", atom.Attribute, atom.Operator, err)
		}
		effect.Atoms = append(effect.Atoms, fact)
	}
	effect.Atoms = sortFacts(effect.Atoms)
	if effect.PredicateSetSHA256, err = exposure.PredicateSetHashV1(effect.Atoms); err != nil {
		return Effect{}, err
	}
	if effect.ResultObservationSHA256, err = exposure.ReleaseOutcomeDigest(effect.Release, int64(len(hits))); err != nil {
		return Effect{}, err
	}
	effect.Composite, err = exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion: NormalFormVersion, QueryNormalFormSHA256: normalFormDigest,
		ResultObservationSHA256: effect.ResultObservationSHA256, VisibleRows: int64(len(hits)),
		PredicateContextSHA256: effect.PredicateContextSHA256, PredicateSetSHA256: effect.PredicateSetSHA256,
		PredicateAtomCount: int64(len(effect.Atoms))})
	if err != nil {
		return Effect{}, err
	}
	return effect, nil
}

// Outcome is F_O: the caller atoms plus the composite.
func (e Effect) Outcome() []exposure.FactID {
	return sortFacts(append(append([]exposure.FactID(nil), e.Atoms...), e.Composite))
}

func sortFacts(facts []exposure.FactID) []exposure.FactID {
	type keyed struct {
		hash string
		fact exposure.FactID
	}
	items := make([]keyed, 0, len(facts))
	for _, fact := range facts {
		hash, _ := fact.Hash()
		items = append(items, keyed{hash: hash, fact: fact})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].hash < items[j].hash })
	out := make([]exposure.FactID, 0, len(items))
	for _, item := range items {
		out = append(out, item.fact)
	}
	return out
}

func sortFactMap(facts map[string]exposure.FactID) []exposure.FactID {
	out := make([]exposure.FactID, 0, len(facts))
	for _, fact := range facts {
		out = append(out, fact)
	}
	return sortFacts(out)
}

// Hashes renders a fact list as sorted hex hashes (for receipts and oracles).
func Hashes(facts []exposure.FactID) ([]string, error) {
	out := make([]string, 0, len(facts))
	for _, fact := range facts {
		hash, err := fact.Hash()
		if err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	sort.Strings(out)
	return out, nil
}

// filteredAttributes lists, once each and in attribute order, the attributes
// the caller's atoms read. Injected scope atoms are applied by the policy
// renderer, not by a plan filter, and leave no witness.
func filteredAttributes(form NormalForm) []Atom {
	seen := map[string]struct{}{}
	var out []Atom
	for _, atom := range form.CallerAtoms() {
		if _, done := seen[atom.Attribute]; done {
			continue
		}
		seen[atom.Attribute] = struct{}{}
		out = append(out, atom)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attribute < out[j].Attribute })
	return out
}

// attributeCellValue types a chunk attribute token for its cell Fact.
func attributeCellValue(attributeType, raw string) (string, any, error) {
	switch attributeType {
	case "enum", "text":
		return "text", raw, nil
	case "date":
		parsed, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return "", nil, fmt.Errorf("date attribute value %q is not YYYY-MM-DD", raw)
		}
		return "date", parsed, nil
	case "numeric":
		return "numeric", json.Number(raw), nil
	default:
		return "", nil, fmt.Errorf("attribute type %q has no cell form", attributeType)
	}
}
