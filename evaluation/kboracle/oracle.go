// Package kboracle is the independent oracle for knowledge-product retrieval
// effects (KB-1 K3.2). It recomputes the Result, Dependency and Outcome member
// sets of a search or fetch from the definitions in docs/kb-design.md section 5
// using only the exposure primitives, without importing internal/retrieval's
// effect builder, and compares member by member. It imports retrieval only for
// the normal-form type and digest, which are the inputs under test.
package kboracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// Expected is the oracle's member sets, as sorted hex hashes.
type Expected struct {
	Release    []string
	Dependency []string
	Outcome    []string
}

// Chunk is what the oracle knows about a returned chunk.
type Chunk struct {
	DocID      string
	Version    int
	ChunkIndex int
	Text       string
	// Attributes are the chunk's values for the caller-filtered attributes,
	// as canonical tokens (see retrieval.Hit).
	Attributes map[string]string
}

func canonical(sqlType string, value any) string {
	out, err := exposure.CanonicalSQLValue(sqlType, value)
	if err != nil {
		panic(err)
	}
	return out
}

func key(parts ...string) string {
	out, err := exposure.ComposeCanonicalKeyV2("base-entity", parts...)
	if err != nil {
		panic(err)
	}
	return out
}

func hashOf(fact exposure.FactID, err error) string {
	if err != nil {
		panic(err)
	}
	hash, err := fact.Hash()
	if err != nil {
		panic(err)
	}
	return hash
}

// Compute derives the expected sets. Every identity is spelled out from the
// definition rather than reused from the builder.
func Compute(binding retrieval.Binding, form retrieval.NormalForm, chunks []Chunk) (Expected, error) {
	ns := binding.SourceNamespace
	release := map[string]struct{}{}
	dependency := map[string]struct{}{}
	var releaseFacts []exposure.FactID
	// The attributes the caller filtered on, once each: a plan filter reads
	// the chunk's cell, so the general algebra counts it as a dependency.
	filteredTypes := map[string]string{}
	var filtered []string
	for _, atom := range form.Atoms {
		if atom.Injected {
			continue
		}
		if _, seen := filteredTypes[atom.Attribute]; !seen {
			filtered = append(filtered, atom.Attribute)
		}
		filteredTypes[atom.Attribute] = atom.Type
	}
	for _, chunk := range chunks {
		chunkKey := key(ns+".chunks", "doc_id", "text", canonical("text", chunk.DocID), "version", "integer", canonical("integer", int64(chunk.Version)),
			"chunk_index", "integer", canonical("integer", int64(chunk.ChunkIndex)))
		docKey := key(ns+".documents", "doc_id", "text", canonical("text", chunk.DocID), "version", "integer", canonical("integer", int64(chunk.Version)))
		// Release: four visible cells under the relational field identity role.column.
		for _, column := range []struct {
			name, sqlType string
			value         any
		}{{"doc_id", "text", chunk.DocID}, {"version", "integer", int64(chunk.Version)}, {"chunk_index", "integer", int64(chunk.ChunkIndex)}, {"text", "text", chunk.Text}} {
			cell, err := exposure.NewBaseCellFactV2(ns+".chunks", binding.Epoch, chunkKey, binding.ChunksProduct+"."+column.name, column.sqlType, column.value)
			if err != nil {
				return Expected{}, err
			}
			releaseFacts = append(releaseFacts, cell)
			release[hashOf(cell, nil)] = struct{}{}
			dependency[hashOf(cell, nil)] = struct{}{}
		}
		dependency[hashOf(exposure.NewBaseRowFactV2(ns+".chunks", binding.Epoch, chunkKey))] = struct{}{}
		dependency[hashOf(exposure.NewBaseRowFactV2(ns+".documents", binding.Epoch, docKey))] = struct{}{}
		dependency[hashOf(exposure.NewBaseCellFactV2(ns+".documents", binding.Epoch, docKey, binding.DocumentsProduct+".doc_id", "text", chunk.DocID))] = struct{}{}
		dependency[hashOf(exposure.NewBaseCellFactV2(ns+".documents", binding.Epoch, docKey, binding.DocumentsProduct+".version", "integer", int64(chunk.Version)))] = struct{}{}
		for _, attribute := range filtered {
			raw, present := chunk.Attributes[attribute]
			if !present {
				return Expected{}, fmt.Errorf("chunk %s v%d #%d lacks filtered attribute %q", chunk.DocID, chunk.Version, chunk.ChunkIndex, attribute)
			}
			var sqlType string
			var value any
			switch filteredTypes[attribute] {
			case "date":
				parsed, err := time.Parse("2006-01-02", raw)
				if err != nil {
					return Expected{}, err
				}
				sqlType, value = "date", parsed
			case "numeric":
				sqlType, value = "numeric", json.Number(raw)
			default:
				sqlType, value = "text", raw
			}
			dependency[hashOf(exposure.NewBaseCellFactV2(ns+".chunks", binding.Epoch, chunkKey, binding.ChunksProduct+"."+attribute, sqlType, value))] = struct{}{}
		}
	}

	// Outcome: caller atoms under the retrieval context, plus one composite.
	var injected []retrieval.Atom
	for _, atom := range form.Atoms {
		if atom.Injected {
			injected = append(injected, atom)
		}
	}
	if injected == nil {
		injected = []retrieval.Atom{}
	}
	scopeJSON, _ := json.Marshal(injected)
	scopeDigest := sha256.Sum256(scopeJSON)
	// The builder encodes a struct in this field order; spell the JSON out so
	// the oracle commits to the documented order rather than to a shared type.
	ordered := fmt.Sprintf(`{"version":%q,"catalog_sha256":%q,"knowledge_product":%q,"epoch":%q,"chunks_publication_sha256":%q,"documents_publication_sha256":%q,"effective_scope_sha256":%q}`,
		retrieval.ContextVersion, binding.CatalogSHA256, binding.KnowledgeProduct, binding.Epoch, binding.ChunksPublicationSHA256,
		binding.DocumentsPublicationSHA256, hex.EncodeToString(scopeDigest[:]))
	contextSum := sha256.Sum256(append([]byte("TASKGATE-RETRIEVAL-CONTEXT-V1\x00"), []byte(ordered)...))
	contextDigest := hex.EncodeToString(contextSum[:])

	outcome := map[string]struct{}{}
	var atoms []exposure.FactID
	for _, atom := range form.Atoms {
		if atom.Injected {
			continue
		}
		sqlType := "text"
		if atom.Type == "date" || atom.Type == "numeric" {
			sqlType = atom.Type
		}
		expression := sha256.Sum256([]byte(ns + ".chunks." + atom.Attribute))
		operator := map[string]string{"=": "EQ", "in": "EQ", "has": "EQ", "<>": "NE", "<": "LT", "<=": "LE", ">": "GT", ">=": "GE"}[atom.Operator]
		if operator == "" {
			return Expected{}, fmt.Errorf("operator %q has no atom form", atom.Operator)
		}
		input := exposure.PredicateAtomFactV5{PredicateContextSHA256: contextDigest, SemanticProductID: binding.ChunksProduct,
			StableRole: binding.ChunksProduct, PublicFieldID: atom.Attribute, ResolvedExpressionSHA256: hex.EncodeToString(expression[:]),
			SQLType: sqlType, Operator: operator, CanonicalLiteral: atom.CanonicalLiteral}
		if sqlType == "text" {
			input.CollationName, input.CollationVersion = binding.CollationName, binding.CollationVersion
		}
		fact, err := exposure.NewPredicateAtomFactV5(input)
		if err != nil {
			return Expected{}, err
		}
		atoms = append(atoms, fact)
		outcome[hashOf(fact, nil)] = struct{}{}
	}
	setDigest, err := exposure.PredicateSetHashV1(atoms)
	if err != nil {
		return Expected{}, err
	}
	normalForm, err := form.Digest()
	if err != nil {
		return Expected{}, err
	}
	observation, err := exposure.ReleaseOutcomeDigest(releaseFacts, int64(len(chunks)))
	if err != nil {
		return Expected{}, err
	}
	composite, err := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion: retrieval.NormalFormVersion, QueryNormalFormSHA256: normalForm, ResultObservationSHA256: observation,
		VisibleRows: int64(len(chunks)), PredicateContextSHA256: contextDigest, PredicateSetSHA256: setDigest, PredicateAtomCount: int64(len(atoms))})
	if err != nil {
		return Expected{}, err
	}
	outcome[hashOf(composite, nil)] = struct{}{}
	return Expected{Release: sorted(release), Dependency: sorted(dependency), Outcome: sorted(outcome)}, nil
}

func sorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for hash := range set {
		out = append(out, hash)
	}
	sort.Strings(out)
	return out
}

// Difference lists members present in one side and absent in the other.
type Difference struct {
	Dimension string
	Missing   []string // expected by the oracle, absent from the effect
	Extra     []string // in the effect, not expected by the oracle
}

// Compare checks an effect's member sets against the oracle's.
func Compare(expected Expected, release, dependency, outcome []string) []Difference {
	var differences []Difference
	for _, pair := range []struct {
		name      string
		want, got []string
	}{{"release", expected.Release, release}, {"dependency", expected.Dependency, dependency}, {"outcome", expected.Outcome, outcome}} {
		wantSet, gotSet := map[string]struct{}{}, map[string]struct{}{}
		for _, hash := range pair.want {
			wantSet[hash] = struct{}{}
		}
		for _, hash := range pair.got {
			gotSet[hash] = struct{}{}
		}
		difference := Difference{Dimension: pair.name}
		for hash := range wantSet {
			if _, ok := gotSet[hash]; !ok {
				difference.Missing = append(difference.Missing, hash)
			}
		}
		for hash := range gotSet {
			if _, ok := wantSet[hash]; !ok {
				difference.Extra = append(difference.Extra, hash)
			}
		}
		sort.Strings(difference.Missing)
		sort.Strings(difference.Extra)
		if len(difference.Missing)+len(difference.Extra) > 0 {
			differences = append(differences, difference)
		}
	}
	return differences
}
