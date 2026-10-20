package gateway

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// TestSQLWherePredicatesDescribeTheLowerer lowers a sample of every listed
// predicate through the frozen lowerer: each admitted one lowers to its plan
// operator and is authorized as its policy operator alone, each rejected one
// does not lower. A predicate the lowerer changed would fail here.
func TestSQLWherePredicatesDescribeTheLowerer(t *testing.T) {
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
	admitted := map[string]string{
		"=": "title = 'x'", "<>": "title <> 'x'", "<": "version < 2", "<=": "version <= 2", ">": "version > 2",
		">=": "version >= 2", "IN": "title IN ('x', 'y')", "NOT IN": "title NOT IN ('x')", "LIKE": "title LIKE '%工资%'",
	}
	for _, predicate := range sqlWherePredicates {
		sample, ok := admitted[predicate.predicate]
		if !ok {
			t.Fatalf("listed predicate %s has no sample", predicate.predicate)
		}
		result, err := sqllowering.Lower(statement(sample), products)
		if err != nil {
			t.Fatalf("listed predicate %s does not lower: %v", predicate.predicate, err)
		}
		if len(result.Plan.Filters) != 1 || result.Plan.Filters[0].Op != predicate.planOperator {
			t.Fatalf("%s lowered to %+v, want plan operator %s", predicate.predicate, result.Plan.Filters, predicate.planOperator)
		}
		if err := authorize(statement(sample), []string{predicate.policyOperator}); err != nil {
			t.Fatalf("%s is not authorized as policy operator %q alone: %v", predicate.predicate, predicate.policyOperator, err)
		}
	}
	if err := authorize(statement(admitted["LIKE"]), []string{"="}); err == nil {
		t.Fatal("LIKE was authorized without ~~; the policy operator check proves nothing")
	}
	if result, err := sqllowering.Lower(statement("title != 'x'"), products); err != nil || result.Plan.Filters[0].Op != "<>" {
		t.Fatalf("!= is not the <> predicate: %+v %v", result.Plan.Filters, err)
	}
	rejected := map[string][]string{
		"OR / NOT":                      {"title = 'x' OR title = 'y'", "NOT (title = 'x')"},
		"NOT LIKE / ILIKE / SIMILAR TO": {"title NOT LIKE 'x%'", "title ILIKE 'x%'", "title SIMILAR TO 'x%'"},
		"BETWEEN":                       {"version BETWEEN 1 AND 2"},
		"IS NULL / IS NOT NULL":         {"title IS NULL", "title IS NOT NULL"},
		"列与列比较、WHERE 中的函数或表达式": {"title = document_type", "lower(title) = 'x'"},
	}
	for _, entry := range sqlWhereRejected {
		samples := rejected[entry["predicate"]]
		if len(samples) == 0 {
			t.Fatalf("rejected entry %q has no sample", entry["predicate"])
		}
		for _, sample := range samples {
			if _, err := sqllowering.Lower(statement(sample), products); err == nil {
				t.Fatalf("%q is listed as rejected but lowers", sample)
			}
		}
	}
}

func TestEffectiveSQLControlsResolveTheDefaults(t *testing.T) {
	service := kbPlanService(t)
	documents, _ := service.catalog.LookupProduct("policy_kb_documents")
	controls := effectiveSQLControls(documents)
	if controls["operators_declared"] != false || !contains(controls["operators"].([]string), "~~") {
		t.Fatalf("undeclared operators do not resolve to the policy default: %v", controls)
	}
	if len(controls["where_predicates"].([]map[string]string)) != len(sqlWherePredicates) || controls["aggregates_note"] == nil {
		t.Fatalf("documents controls %v", controls)
	}
	summary, _ := service.catalog.LookupProduct("expense_summary")
	declared := effectiveSQLControls(summary)
	for _, predicate := range declared["where_predicates"].([]map[string]string) {
		if predicate["predicate"] == "LIKE" {
			t.Fatalf("expense_summary declares operators without ~~ but LIKE is listed: %v", declared)
		}
	}
	if declared["operators_declared"] != true || declared["aggregates_note"] != nil {
		t.Fatalf("expense_summary controls %v", declared)
	}
}
