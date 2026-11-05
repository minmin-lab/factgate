package physicalquery

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// What a task must approve for retrieval to prepare, read off the equivalent
// plan itself so that discovery, request-time readiness and the precise
// refusal can never disagree with what PrepareRetrieval compiles. Nothing here
// enters a sealed digest: these functions only read retrievalPlan.

// RetrievalPlanColumns names, per published relation, every column the
// equivalent plan of a search or fetch reads: the projection, both sides of
// the join and the caller filters. The plan is compiled against the task's
// approved columns only, so a grant must carry each of them; the mandatory
// scope is applied by the policy renderer and adds none.
func RetrievalPlanColumns(kp catalog.KnowledgeProduct, nf retrieval.NormalForm) (map[string][]string, error) {
	plan, err := retrievalPlan(kp, nf)
	if err != nil {
		return nil, err
	}
	return joinPlanColumns(plan)
}

// RetrievalRequiredColumns is RetrievalPlanColumns for a search that filters
// on the named attributes. Which columns a plan reads depends on which
// attributes are filtered, not on the values, so each attribute stands in
// with a literal of its declared type.
func RetrievalRequiredColumns(kp catalog.KnowledgeProduct, filterAttributes ...string) (map[string][]string, error) {
	types := make(map[string]string, len(kp.Attributes))
	for _, attribute := range kp.Attributes {
		types[attribute.Name] = attribute.Type
	}
	nf := retrieval.NormalForm{Tool: retrieval.ToolSearch, KnowledgeProduct: kp.Name}
	for _, name := range filterAttributes {
		attributeType, declared := types[name]
		if !declared {
			return nil, fmt.Errorf("knowledge product %q declares no attribute %q", kp.Name, name)
		}
		literal, renderable := retrievalStandInLiterals[attributeType]
		if !renderable {
			return nil, fmt.Errorf("attribute %q of type %q has no plan rendering", name, attributeType)
		}
		nf.Atoms = append(nf.Atoms, retrieval.Atom{Attribute: name, Type: attributeType, Operator: "=", CanonicalLiteral: literal})
	}
	return RetrievalPlanColumns(kp, nf)
}

// retrievalStandInLiterals are well-formed literals of each attribute type
// retrievalFilterValue renders; text_set has none (its "has" atom has no plan
// rendering).
var retrievalStandInLiterals = map[string]string{"enum": "s:x", "text": "s:x", "numeric": "n:0", "date": "d:2000-01-01"}

// RetrievalOperatorRendered reports whether a normal-form operator has a plan
// rendering. The language admits operators the plan cannot render yet, and
// Validate refuses those at preparation.
func RetrievalOperatorRendered(operator string) bool {
	_, rendered := retrievalPlanOperators[operator]
	return rendered
}

// joinPlanColumns maps a join plan's column references back to the products
// of its two scans.
func joinPlanColumns(plan queryplan.QueryPlan) (map[string][]string, error) {
	if plan.From == nil || plan.From.Join == nil {
		return nil, errors.New("retrieval plan is not a join of two scans")
	}
	join := plan.From.Join
	products := map[string]string{join.Left.Role: join.Left.Product, join.Right.Role: join.Right.Product}
	sets := map[string]map[string]struct{}{}
	add := func(reference string) error {
		role, column, found := strings.Cut(reference, ".")
		product, known := products[role]
		if !found || !known || column == "" {
			return fmt.Errorf("plan column %q names no scan of the join", reference)
		}
		if sets[product] == nil {
			sets[product] = map[string]struct{}{}
		}
		sets[product][column] = struct{}{}
		return nil
	}
	references := append([]string(nil), plan.Columns...)
	for _, predicate := range join.On {
		references = append(references, predicate.Left, predicate.Right)
	}
	for _, filter := range plan.Filters {
		references = append(references, filter.Column)
	}
	for _, reference := range references {
		if err := add(reference); err != nil {
			return nil, err
		}
	}
	out := make(map[string][]string, len(sets))
	for product, set := range sets {
		columns := make([]string, 0, len(set))
		for column := range set {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		out[product] = columns
	}
	return out, nil
}
