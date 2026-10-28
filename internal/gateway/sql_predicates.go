package gateway

import (
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// The WHERE predicates taskgate-reporting-sql-v1 lowers (KB1-LOOP-137).
//
// internal/sqllowering is frozen by the held-out protocol, so the list lives
// here and describes it; TestSQLWherePredicatesDescribeTheLowerer lowers a
// sample of every entry, admitted and rejected, so the two cannot drift.
// policyOperator is the operator name sqlpolicy authorizes the predicate as
// (an IN list is "=", NOT IN is "<>", LIKE is "~~"); policyWords are further
// names that admit it alone (in for IN, not in for NOT IN). Declaring any one
// lists the predicate.
type sqlWherePredicate struct {
	predicate, form, planOperator, policyOperator string
	policyWords                                   []string
}

func (p sqlWherePredicate) policyOperators() []string {
	return append([]string{p.policyOperator}, p.policyWords...)
}

var sqlWherePredicates = []sqlWherePredicate{
	{"=", "column = literal", "=", "=", nil},
	{"<>", "column <> literal（!= 同义）", "<>", "<>", nil},
	{"<", "column < literal", "<", "<", nil},
	{"<=", "column <= literal", "<=", "<=", nil},
	{">", "column > literal", ">", ">", nil},
	{">=", "column >= literal", ">=", ">=", nil},
	{"IN", "column IN (literal, ...)", "IN", "=", []string{"in"}},
	{"NOT IN", "column NOT IN (literal, ...)", "NOT IN", "<>", []string{"not in"}},
	{"LIKE", "text_column LIKE 'pattern'：只支持正向、区分大小写、字面量模式（% 与 _ 是通配符）", "LIKE", "~~", nil},
}

// sqlWhereRejected names what the profile refuses and how to rewrite it.
var sqlWhereRejected = []map[string]string{
	{"predicate": "OR / NOT", "rewrite": "WHERE 只接受用 AND 连接的谓词；OR 拆成多次查询，或改用 IN"},
	{"predicate": "NOT LIKE / ILIKE / SIMILAR TO", "rewrite": "只支持区分大小写的正向 LIKE"},
	{"predicate": "BETWEEN", "rewrite": "改写为 column >= a AND column <= b"},
	{"predicate": "IS NULL / IS NOT NULL", "rewrite": "不支持空值谓词"},
	{"predicate": "列与列比较、WHERE 中的函数或表达式", "rewrite": "只能把一个已授权列与一个字面量比较"},
}

func publicWherePredicates(predicates []sqlWherePredicate) []map[string]string {
	public := make([]map[string]string, 0, len(predicates))
	for _, predicate := range predicates {
		public = append(public, map[string]string{"predicate": predicate.predicate, "form": predicate.form})
	}
	return public
}

// effectiveSQLControls is what query_sql admits on one product: its declared
// operators, or the policy's default set when it declares none, and its
// declared aggregates, which are none when it declares none. A declared
// operator is matched under the key sqlpolicy authorizes it as, so a product
// declaring like lists LIKE and one declaring in lists IN but not =.
func effectiveSQLControls(product catalog.Product) map[string]any {
	operators, declared := product.AllowedOperators, len(product.AllowedOperators) > 0
	if !declared {
		operators = sqlpolicy.DefaultOperators()
	}
	authorized := make([]string, 0, len(operators))
	for _, operator := range operators {
		authorized = append(authorized, sqlpolicy.OperatorKey(operator))
	}
	admitted := []sqlWherePredicate{}
	for _, predicate := range sqlWherePredicates {
		for _, operator := range predicate.policyOperators() {
			if contains(authorized, operator) {
				admitted = append(admitted, predicate)
				break
			}
		}
	}
	aggregates := append([]string{}, product.AllowedAggregates...)
	controls := map[string]any{
		"operators_declared": declared, "operators": append([]string{}, operators...),
		"where_predicates": publicWherePredicates(admitted),
		"aggregates":       aggregates,
	}
	if len(aggregates) == 0 {
		controls["aggregates_note"] = "该产品未声明 allowed_aggregates，SQL 中不能用 COUNT/SUM/MIN/MAX/AVG；只能逐行取回"
	}
	return controls
}
