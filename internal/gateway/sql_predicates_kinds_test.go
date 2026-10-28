package gateway

import (
	"reflect"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// TestEffectiveSQLControlsListInUnderItsWord: a product declaring in lists IN
// and not =, one declaring not in lists NOT IN and not <>, and = and <> keep
// listing the comparison together with its list form. BETWEEN is not a
// listed predicate (the lowerer refuses it), so between lists nothing.
func TestEffectiveSQLControlsListInUnderItsWord(t *testing.T) {
	want := map[string][]string{
		"in": {"IN"}, "IN": {"IN"}, " In ": {"IN"}, "not in": {"NOT IN"}, "NOT IN": {"NOT IN"},
		"=": {"=", "IN"}, "<>": {"<>", "NOT IN"}, "between": {}, "not between": {},
	}
	for operator, predicates := range want {
		if got := listedSQLWherePredicates(effectiveSQLControls(catalog.Product{AllowedOperators: []string{operator}})); !reflect.DeepEqual(got, predicates) {
			t.Fatalf("declared [%q] lists %q, want %q", operator, got, predicates)
		}
	}
	both := effectiveSQLControls(catalog.Product{AllowedOperators: []string{"=", "in", "not in"}})
	if got := listedSQLWherePredicates(both); !reflect.DeepEqual(got, []string{"=", "IN", "NOT IN"}) {
		t.Fatalf(`declared ["=", "in", "not in"] lists %q`, got)
	}
	if published := both["operators"].([]string); !reflect.DeepEqual(published, []string{"=", "in", "not in"}) {
		t.Fatalf("declaration published as %q", published)
	}
}

func listedSQLWherePredicates(controls map[string]any) []string {
	listed := []string{}
	for _, predicate := range controls["where_predicates"].([]map[string]string) {
		listed = append(listed, predicate["predicate"])
	}
	return listed
}

// TestSQLWherePredicatesPolicyWordsAuthorizeAlone: each further policy word a
// listed predicate names admits that predicate's sample alone, as discovery
// promises, and does not admit the scalar comparison its symbol also covers.
func TestSQLWherePredicatesPolicyWordsAuthorizeAlone(t *testing.T) {
	service := kbPlanService(t)
	product, found := service.catalog.LookupProduct("policy_kb_documents")
	if !found {
		t.Fatal("fixture declares no policy_kb_documents")
	}
	approved := map[string]struct{}{}
	columns := make([]string, 0, len(product.Fields))
	for _, field := range product.Fields {
		approved[field.Name] = struct{}{}
		columns = append(columns, field.Name)
	}
	products := map[string]queryplan.Product{product.Name: physicalquery.QueryProductFromCatalog(product, approved)}
	statement := func(where string) string { return "SELECT doc_id FROM policy_kb_documents WHERE " + where }
	authorize := func(sql string, operators []string) error {
		_, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{SQL: sql, RowLimit: 10, Grant: sqlpolicy.Grant{
			Products: []sqlpolicy.ProductGrant{{LogicalName: product.Name, PhysicalSchema: "reporting", PhysicalView: "kb_documents",
				ApprovedColumns: columns, AllowedOperators: operators}}}})
		return err
	}
	samples := map[string][2]string{
		"IN": {"title IN ('x', 'y')", "title = 'x'"}, "NOT IN": {"title NOT IN ('x')", "title <> 'x'"},
	}
	worded := 0
	for _, predicate := range sqlWherePredicates {
		if len(predicate.policyWords) == 0 {
			continue
		}
		worded++
		sample, ok := samples[predicate.predicate]
		if !ok {
			t.Fatalf("predicate %s names policy words %q but has no sample", predicate.predicate, predicate.policyWords)
		}
		result, err := sqllowering.Lower(statement(sample[0]), products)
		if err != nil || len(result.Plan.Filters) != 1 || result.Plan.Filters[0].Op != predicate.planOperator {
			t.Fatalf("%s sample does not lower to %s: %+v %v", predicate.predicate, predicate.planOperator, result.Plan.Filters, err)
		}
		for _, word := range predicate.policyWords {
			if err := authorize(statement(sample[0]), []string{word}); err != nil {
				t.Fatalf("%s is not authorized as policy word %q alone: %v", predicate.predicate, word, err)
			}
			if err := authorize(statement(sample[1]), []string{word}); !sqlpolicy.IsCode(err, sqlpolicy.CodeOperatorNotAllowed) {
				t.Fatalf("policy word %q admits the scalar %s: %v", word, sample[1], err)
			}
		}
	}
	if worded != len(samples) {
		t.Fatalf("%d predicates name policy words, want %d", worded, len(samples))
	}
}
