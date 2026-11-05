package sqlpolicy

import "testing"

// TestDeclaredLikeAuthorizesLikeOnly: a product declaring like, LIKE or ~~ is
// granted LIKE and nothing more of the family; one declaring only = is not.
func TestDeclaredLikeAuthorizesLikeOnly(t *testing.T) {
	engine := New(Config{})
	authorize := func(operators []string, predicate string) error {
		grant := testGrant()
		grant.Products[0].AllowedOperators = operators
		_, err := engine.Authorize(Request{
			SQL:   `SELECT department FROM expense_detail WHERE employee_name ` + predicate,
			Grant: grant, RowLimit: 10,
		})
		return err
	}
	const like, ilike, notLike, notIlike = `LIKE 'x%'`, `ILIKE 'x%'`, `NOT LIKE 'x%'`, `NOT ILIKE 'x%'`
	for _, operators := range [][]string{{"=", "like"}, {"=", "~~"}, {"=", "LIKE"}, {"=", " like "}} {
		if err := authorize(operators, like); err != nil {
			t.Fatalf("%q: LIKE rejected: %v", operators, err)
		}
		for _, predicate := range []string{ilike, notLike, notIlike} {
			if err := authorize(operators, predicate); !IsCode(err, CodeOperatorNotAllowed) {
				t.Fatalf("%q: %s error = %v, want %s", operators, predicate, err, CodeOperatorNotAllowed)
			}
		}
	}
	if err := authorize([]string{"="}, like); !IsCode(err, CodeOperatorNotAllowed) {
		t.Fatalf(`["="]: LIKE error = %v, want %s`, err, CodeOperatorNotAllowed)
	}
	granted := map[string][]string{
		ilike:    {"ilike", "~~*"},
		notLike:  {"not like", "!~~"},
		notIlike: {"not ilike", "!~~*"},
	}
	for predicate, spellings := range granted {
		for _, spelling := range spellings {
			operators := []string{"=", spelling}
			if err := authorize(operators, predicate); err != nil {
				t.Fatalf("%q: %s rejected: %v", operators, predicate, err)
			}
			if err := authorize(operators, like); !IsCode(err, CodeOperatorNotAllowed) {
				t.Fatalf("%q: LIKE error = %v, want %s", operators, err, CodeOperatorNotAllowed)
			}
		}
	}
}

// TestConfigLikeOperatorIsCanonicalToo: the task-global set New builds from
// Config reads like as ~~ as well.
func TestConfigLikeOperatorIsCanonicalToo(t *testing.T) {
	sql := `SELECT 'abc' LIKE 'a%' AS c`
	if _, err := New(Config{AllowedOperators: []string{"like"}}).Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 1}); err != nil {
		t.Fatalf("Config like: %v", err)
	}
	if _, err := New(Config{AllowedOperators: []string{"="}}).Authorize(Request{SQL: sql, Grant: testGrant(), RowLimit: 1}); !IsCode(err, CodeOperatorNotAllowed) {
		t.Fatalf("Config = error = %v, want %s", err, CodeOperatorNotAllowed)
	}
}

// TestCanonicalOperatorRenamesOnlyTheLikeFamily: every other name, the default
// set and the Catalog vocabulary among them, comes back byte for byte.
func TestCanonicalOperatorRenamesOnlyTheLikeFamily(t *testing.T) {
	renamed := map[string]string{
		"like": "~~", "LIKE": "~~", " like ": "~~", "ilike": "~~*", "ILike": "~~*",
		"not like": "!~~", "NOT LIKE": "!~~", "not ilike": "!~~*",
	}
	for operator, want := range renamed {
		if got := CanonicalOperator(operator); got != want {
			t.Fatalf("CanonicalOperator(%q) = %q, want %q", operator, got, want)
		}
	}
	unchanged := append(DefaultOperators(),
		"and", "or", "not", "in", "IN", "between", "is", "is not", "<=>", " = ", "", "~~~", "not  like")
	for _, operator := range unchanged {
		if got := CanonicalOperator(operator); got != operator {
			t.Fatalf("CanonicalOperator(%q) = %q, want it unchanged", operator, got)
		}
	}
}
