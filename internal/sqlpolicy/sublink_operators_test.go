package sqlpolicy

import (
	"encoding/json"
	"testing"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

const (
	subNames      = `(SELECT employee_name FROM expense_detail)`
	inSubquery    = `employee_name IN ` + subNames
	notInSubquery = `employee_name NOT IN ` + subNames
	notParenIn    = `NOT (employee_name IN ` + subNames + `)`
	eqAnySubquery = `employee_name = ANY ` + subNames
	neAllSubquery = `employee_name <> ALL ` + subNames
	rowInSubquery = `(employee_name, department) IN (SELECT employee_name, department FROM expense_detail)`
)

// whereClause parses SELECT 1 FROM t WHERE predicate and returns the WHERE
// node as pg_query emits it.
func whereClause(t *testing.T, predicate string) (string, map[string]any) {
	t.Helper()
	out, err := pg_query.ParseToJSON(`SELECT 1 FROM t WHERE ` + predicate)
	if err != nil {
		t.Fatalf("parse %s: %v", predicate, err)
	}
	var tree struct {
		Stmts []struct {
			Stmt struct {
				SelectStmt struct {
					WhereClause map[string]any `json:"whereClause"`
				}
			} `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil || len(tree.Stmts) != 1 {
		t.Fatalf("decode %s: %v", predicate, err)
	}
	name, body, ok := unwrapNode(tree.Stmts[0].Stmt.SelectStmt.WhereClause)
	if !ok {
		t.Fatalf("%s: WHERE is not one node: %s", predicate, out)
	}
	return name, body
}

// TestSubLinkParseShapesTheGateRelies: the shapes validateSubLink and
// negatedInSubquery are written against. IN (SELECT) and its row form carry no
// operName; NOT IN (SELECT) is a NOT BoolExpr over that SubLink; op ANY/ALL
// name op; a row compared with a subquery is an A_Expr over an EXPR SubLink, so
// the raw parser emits no ROWCOMPARE SubLink.
func TestSubLinkParseShapesTheGateRelies(t *testing.T) {
	sublink := func(predicate, kind string, operName []string) {
		t.Helper()
		name, body := whereClause(t, predicate)
		if name != "SubLink" || body["subLinkType"] != kind {
			t.Fatalf("%s: %s %v, want SubLink %s", predicate, name, body["subLinkType"], kind)
		}
		_, named := body["operName"]
		if got := nodeStringList(body["operName"]); named != (operName != nil) || len(got) != len(operName) || (named && got[0] != operName[0]) {
			t.Fatalf("%s: operName %v (present %v), want %v", predicate, got, named, operName)
		}
	}
	sublink(inSubquery, "ANY_SUBLINK", nil)
	sublink(rowInSubquery, "ANY_SUBLINK", nil)
	sublink(eqAnySubquery, "ANY_SUBLINK", []string{"="})
	sublink(neAllSubquery, "ALL_SUBLINK", []string{"<>"})
	sublink(`amount < ANY `+subNames, "ANY_SUBLINK", []string{"<"})
	sublink(`employee_name LIKE ANY `+subNames, "ANY_SUBLINK", []string{"~~"})
	sublink(`EXISTS `+subNames, "EXISTS_SUBLINK", nil)
	for _, predicate := range []string{notInSubquery, notParenIn} {
		name, body := whereClause(t, predicate)
		if name != "BoolExpr" {
			t.Fatalf("%s: %s, want BoolExpr", predicate, name)
		}
		if _, ok := negatedInSubquery(body); !ok {
			t.Fatalf("%s: negatedInSubquery did not recognize %v", predicate, body)
		}
	}
	name, body := whereClause(t, `(employee_name, department) = (SELECT employee_name, department FROM expense_detail)`)
	rexprName, rexpr, _ := unwrapNode(body["rexpr"])
	if name != "A_Expr" || rexprName != "SubLink" || rexpr["subLinkType"] != "EXPR_SUBLINK" {
		t.Fatalf("row comparison: %s over %s %v, want A_Expr over EXPR_SUBLINK", name, rexprName, rexpr["subLinkType"])
	}
}

// TestSubLinkComparisonFormsAreGatedLikeTheirOperator: a product declaring only
// < no longer admits IN, NOT IN, = ANY or <> ALL over a subquery; < ANY and
// < ALL pass; EXISTS is not an operator; a scalar subquery's = is the outer
// A_Expr's and stays gated.
func TestSubLinkComparisonFormsAreGatedLikeTheirOperator(t *testing.T) {
	operators := []string{"<"}
	for _, predicate := range []string{inSubquery, notInSubquery, notParenIn, eqAnySubquery, neAllSubquery} {
		wantOperatorRejected(t, operators, predicate)
	}
	for _, predicate := range []string{
		`amount < ANY (SELECT amount FROM expense_detail)`,
		`amount < ALL (SELECT amount FROM expense_detail WHERE amount < 10)`,
		`EXISTS (SELECT 1 FROM expense_detail WHERE amount < 1)`,
		`amount < (SELECT max(amount) FROM expense_detail)`,
	} {
		wantAdmitted(t, operators, predicate)
	}
	wantOperatorRejected(t, operators, `amount = (SELECT max(amount) FROM expense_detail)`)
}

// TestSubLinkInAuthorizesTheInSubqueryOnly: in admits IN (SELECT), in its row
// form too, and not NOT IN (SELECT) or = ANY (SELECT).
func TestSubLinkInAuthorizesTheInSubqueryOnly(t *testing.T) {
	for _, operators := range [][]string{{"in"}, {" IN "}} {
		wantAdmitted(t, operators, inSubquery)
		wantAdmitted(t, operators, rowInSubquery)
		for _, predicate := range []string{notInSubquery, notParenIn, eqAnySubquery, neAllSubquery} {
			wantOperatorRejected(t, operators, predicate)
		}
	}
}

// TestSubLinkEqualsAuthorizesInAndEqualsAny: = admits IN (SELECT) and = ANY
// (SELECT), not NOT IN (SELECT).
func TestSubLinkEqualsAuthorizesInAndEqualsAny(t *testing.T) {
	operators := []string{"="}
	wantAdmitted(t, operators, inSubquery)
	wantAdmitted(t, operators, rowInSubquery)
	wantAdmitted(t, operators, eqAnySubquery)
	wantOperatorRejected(t, operators, notInSubquery)
	wantOperatorRejected(t, operators, neAllSubquery)
}

// TestSubLinkNotInAndNotEqualsAuthorizeTheNegation: not in admits NOT IN
// (SELECT) in both spellings and neither IN (SELECT) nor <> ALL; <> admits NOT
// IN (SELECT) and <> ALL.
func TestSubLinkNotInAndNotEqualsAuthorizeTheNegation(t *testing.T) {
	for _, operators := range [][]string{{"not in"}, {"NOT IN"}} {
		wantAdmitted(t, operators, notInSubquery)
		wantAdmitted(t, operators, notParenIn)
		wantOperatorRejected(t, operators, inSubquery)
		wantOperatorRejected(t, operators, neAllSubquery)
	}
	operators := []string{"<>"}
	wantAdmitted(t, operators, notInSubquery)
	wantAdmitted(t, operators, notParenIn)
	wantAdmitted(t, operators, neAllSubquery)
	wantOperatorRejected(t, operators, inSubquery)
}

// TestSubLinkUndeclaredUsesTheDefaults: a product declaring no operators admits
// IN and NOT IN (SELECT) through = and <>, and rejects ~ ANY (SELECT) as it
// rejects ~.
func TestSubLinkUndeclaredUsesTheDefaults(t *testing.T) {
	wantAdmitted(t, nil, inSubquery)
	wantAdmitted(t, nil, notInSubquery)
	wantOperatorRejected(t, nil, `employee_name ~ 'a'`)
	wantOperatorRejected(t, nil, `employee_name ~ ANY `+subNames)
}

// TestSubLinkSubqueryIsStillAnalyzed: the subselect still goes through
// analyzeSelect, so an undeclared operator or an unapproved column inside it
// is rejected even when the IN itself is declared.
func TestSubLinkSubqueryIsStillAnalyzed(t *testing.T) {
	operators := []string{"in"}
	wantAdmitted(t, operators, inSubquery)
	wantOperatorRejected(t, operators, `employee_name IN (SELECT employee_name FROM expense_detail WHERE amount = 1)`)
	if err := authorizeWhere(operators, `employee_name IN (SELECT salary FROM expense_detail)`); !IsCode(err, CodeColumnNotAllowed) {
		t.Fatalf("unapproved column in the subquery: error = %v, want %s", err, CodeColumnNotAllowed)
	}
}

// TestSubLinkTaskGlobalSetUsesTheSameKeys: a subquery predicate over no product
// is checked against the Config set.
func TestSubLinkTaskGlobalSetUsesTheSameKeys(t *testing.T) {
	authorize := func(operators []string, expression string) error {
		_, err := New(Config{AllowedOperators: operators}).Authorize(Request{
			SQL: `SELECT ` + expression + ` AS c`, Grant: testGrant(), RowLimit: 1,
		})
		return err
	}
	admitted := map[string][]string{
		`1 IN (SELECT 2)`: {"in"}, `1 NOT IN (SELECT 2)`: {"not in"}, `1 = ANY (SELECT 2)`: {"="},
		`1 <> ALL (SELECT 2)`: {"<>"},
	}
	for expression, operators := range admitted {
		if err := authorize(operators, expression); err != nil {
			t.Fatalf("Config %q: %s rejected: %v", operators, expression, err)
		}
		if err := authorize([]string{"<"}, expression); !IsCode(err, CodeOperatorNotAllowed) {
			t.Fatalf("Config [<]: %s error = %v, want %s", expression, err, CodeOperatorNotAllowed)
		}
	}
}
