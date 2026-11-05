package sqlpolicy

import (
	"errors"
	"testing"
)

// authorizeWhere authorizes one WHERE predicate over expense_detail with the
// product declaring operators (nil means it declares none).
func authorizeWhere(operators []string, predicate string) error {
	grant := testGrant()
	grant.Products[0].AllowedOperators = operators
	_, err := New(Config{}).Authorize(Request{
		SQL:   `SELECT department FROM expense_detail WHERE ` + predicate,
		Grant: grant, RowLimit: 10,
	})
	return err
}

func wantAdmitted(t *testing.T, operators []string, predicate string) {
	t.Helper()
	if err := authorizeWhere(operators, predicate); err != nil {
		t.Fatalf("%q: %s rejected: %v", operators, predicate, err)
	}
}

func wantOperatorRejected(t *testing.T, operators []string, predicate string) {
	t.Helper()
	if err := authorizeWhere(operators, predicate); !IsCode(err, CodeOperatorNotAllowed) {
		t.Fatalf("%q: %s error = %v, want %s", operators, predicate, err, CodeOperatorNotAllowed)
	}
}

const (
	inList       = `employee_name IN ('a', 'b')`
	notInList    = `employee_name NOT IN ('a')`
	between      = `amount BETWEEN 1 AND 2`
	notBetween   = `amount NOT BETWEEN 1 AND 2`
	betweenSym   = `amount BETWEEN SYMMETRIC 1 AND 2`
	notBetweenSy = `amount NOT BETWEEN SYMMETRIC 1 AND 2`
)

// TestOperatorKindInAuthorizesTheListOnly: in admits an IN list and nothing
// else pg_query names =: not the scalar comparison, IS DISTINCT FROM or = ANY.
func TestOperatorKindInAuthorizesTheListOnly(t *testing.T) {
	for _, operators := range [][]string{{"in"}, {"IN"}, {" in "}} {
		wantAdmitted(t, operators, inList)
		for _, predicate := range []string{
			`employee_name = 'a'`, notInList, `employee_name IS DISTINCT FROM 'a'`,
			`employee_name IS NOT DISTINCT FROM 'a'`, `employee_name = ANY(ARRAY['a'])`,
		} {
			wantOperatorRejected(t, operators, predicate)
		}
	}
	err := authorizeWhere([]string{"in"}, `employee_name = ANY(ARRAY['a'])`)
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("= ANY under in: %v is not a policy rejection", err)
	}
	t.Logf("= ANY under [\"in\"] is rejected with %s", policyErr.Code)
}

// TestOperatorKindEqualsStillAuthorizesInAndDistinct: = keeps admitting an IN
// list (the formal campaign's catalogs rely on it) and IS [NOT] DISTINCT FROM,
// which pg_query also names =.
func TestOperatorKindEqualsStillAuthorizesInAndDistinct(t *testing.T) {
	operators := []string{"="}
	for _, predicate := range []string{
		`employee_name = 'a'`, inList, `employee_name IS DISTINCT FROM 'a'`, `employee_name IS NOT DISTINCT FROM 'a'`,
	} {
		wantAdmitted(t, operators, predicate)
	}
	wantOperatorRejected(t, operators, notInList)
}

// TestOperatorKindNotInAuthorizesTheListOnly: not in admits a NOT IN list and
// not <>; <> keeps admitting NOT IN.
func TestOperatorKindNotInAuthorizesTheListOnly(t *testing.T) {
	for _, operators := range [][]string{{"not in"}, {"NOT IN"}} {
		wantAdmitted(t, operators, notInList)
		for _, predicate := range []string{`employee_name <> 'a'`, `employee_name != 'a'`, inList} {
			wantOperatorRejected(t, operators, predicate)
		}
	}
	wantAdmitted(t, []string{"<>"}, notInList)
	wantAdmitted(t, []string{"<>"}, `employee_name != 'a'`)
	wantOperatorRejected(t, []string{"<>"}, inList)
}

// TestOperatorKindBetweenFamilyIsGrantedFormByForm: between, in any case or
// spacing, admits BETWEEN alone; not between admits NOT BETWEEN alone; neither
// admits a SYMMETRIC form.
func TestOperatorKindBetweenFamilyIsGrantedFormByForm(t *testing.T) {
	for _, operators := range [][]string{{"between"}, {"BETWEEN"}, {" Between "}} {
		wantAdmitted(t, operators, between)
		for _, predicate := range []string{notBetween, betweenSym, notBetweenSy, `amount >= 1`} {
			wantOperatorRejected(t, operators, predicate)
		}
	}
	for _, operators := range [][]string{{"not between"}, {"NOT BETWEEN"}} {
		wantAdmitted(t, operators, notBetween)
		for _, predicate := range []string{between, betweenSym, notBetweenSy} {
			wantOperatorRejected(t, operators, predicate)
		}
	}
	wantOperatorRejected(t, []string{"=", "<=", ">="}, between)
}

// TestOperatorKindUndeclaredKeepsTheDefaults: a product declaring no operators
// still rejects BETWEEN and admits IN and NOT IN, as before.
func TestOperatorKindUndeclaredKeepsTheDefaults(t *testing.T) {
	wantOperatorRejected(t, nil, between)
	wantOperatorRejected(t, nil, notBetween)
	wantAdmitted(t, nil, inList)
	wantAdmitted(t, nil, notInList)
}

// TestOperatorKindStructuralNodesAreNotGated: AND, OR, NOT and IS [NOT] NULL
// are BoolExpr and NullTest nodes, which allowed_operators never constrains, so
// a product declaring only = admits them. This is why the Catalog vocabulary
// does not list them.
func TestOperatorKindStructuralNodesAreNotGated(t *testing.T) {
	for _, predicate := range []string{
		`employee_name = 'a' AND department = 'b'`, `employee_name = 'a' OR employee_name = 'b'`,
		`NOT (employee_name = 'a')`, `employee_name IS NULL`, `employee_name IS NOT NULL`,
	} {
		wantAdmitted(t, []string{"="}, predicate)
	}
}

// TestOperatorKindTaskGlobalSetUsesTheSameKeys: an expression over no product
// is checked against the Config set under the same kind-aware keys.
func TestOperatorKindTaskGlobalSetUsesTheSameKeys(t *testing.T) {
	authorize := func(operators []string, expression string) error {
		_, err := New(Config{AllowedOperators: operators}).Authorize(Request{
			SQL: `SELECT ` + expression + ` AS c`, Grant: testGrant(), RowLimit: 1,
		})
		return err
	}
	admitted := map[string][]string{
		`1 IN (1, 2)`: {"in"}, `1 NOT IN (2)`: {"not in"}, `1 BETWEEN 0 AND 2`: {"BETWEEN"},
		`1 NOT BETWEEN 0 AND 2`: {"not between"},
	}
	for expression, operators := range admitted {
		if err := authorize(operators, expression); err != nil {
			t.Fatalf("Config %q: %s rejected: %v", operators, expression, err)
		}
		if err := authorize([]string{"<"}, expression); !IsCode(err, CodeOperatorNotAllowed) {
			t.Fatalf("Config [<]: %s error = %v, want %s", expression, err, CodeOperatorNotAllowed)
		}
	}
	if _, err := New(Config{}).Authorize(Request{SQL: `SELECT 1 BETWEEN 0 AND 2 AS c`, Grant: testGrant(), RowLimit: 1}); !IsCode(err, CodeOperatorNotAllowed) {
		t.Fatalf("default Config: BETWEEN error = %v, want %s", err, CodeOperatorNotAllowed)
	}
}

// TestOperatorKeyNormalizesWordsOnly: a word is trimmed and lowercased (and a
// LIKE-family word renamed); a symbol is only trimmed.
func TestOperatorKeyNormalizesWordsOnly(t *testing.T) {
	want := map[string]string{
		"in": "in", "IN": "in", " In ": "in", "NOT IN": "not in", "BETWEEN": "between",
		" Between ": "between", "Not Between": "not between", "LIKE": "~~", " not ilike ": "!~~*",
		" = ": "=", "<=>": "<=>", "~~*": "~~*", "": "",
	}
	for operator, key := range want {
		if got := OperatorKey(operator); got != key {
			t.Fatalf("OperatorKey(%q) = %q, want %q", operator, got, key)
		}
	}
	for _, operator := range DefaultOperators() {
		if got := OperatorKey(operator); got != operator {
			t.Fatalf("OperatorKey(%q) = %q, want the default operator unchanged", operator, got)
		}
	}
}
