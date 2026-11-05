package catalog

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// declareOperators parses the valid fixture with expense_summary declaring
// operators (a YAML flow sequence) instead of its own list.
func declareOperators(t *testing.T, operators string) error {
	t.Helper()
	const declared = `allowed_operators: ["=", ">=", "<="]`
	base := validCatalogYAML(t)
	if !strings.Contains(base, declared) {
		t.Fatalf("fixture no longer declares %s", declared)
	}
	_, err := Parse([]byte(strings.Replace(base, declared, "allowed_operators: "+operators, 1)))
	return err
}

// TestCatalogVocabularyNamesWhatThePolicyGates: not in, not between and any
// casing of between are declarable; the structural words, which the policy
// never gates, and BETWEEN SYMMETRIC are not.
func TestCatalogVocabularyNamesWhatThePolicyGates(t *testing.T) {
	for _, operators := range []string{
		`["=", "in"]`, `["=", "not in"]`, `["=", "between"]`, `["=", "not between"]`, `["=", "BETWEEN"]`,
	} {
		if err := declareOperators(t, operators); err != nil {
			t.Fatalf("%s rejected: %v", operators, err)
		}
	}
	for _, operators := range []string{
		`["=", "and"]`, `["=", "or"]`, `["=", "not"]`, `["=", "is"]`, `["=", "is not"]`, `["=", "between symmetric"]`,
	} {
		err := declareOperators(t, operators)
		if !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), "operator is not in the safe catalog vocabulary") {
			t.Fatalf("%s error = %v, want safe-vocabulary rejection", operators, err)
		}
	}
}

// TestCatalogVocabularyEveryOperatorAdmitsItsForm keeps the vocabulary and
// sqlpolicy on one set of names: every word the Catalog accepts, declared
// alone, must admit a sample of its form (and an unrelated operator must not,
// so the sample does exercise it). A word added without a sample fails here.
// != is the one exception: PostgreSQL parses it as <>, so <> admits it.
func TestCatalogVocabularyEveryOperatorAdmitsItsForm(t *testing.T) {
	samples := map[string]string{
		"=": "amount = 1", "<>": "amount <> 1", "<": "amount < 1", "<=": "amount <= 1",
		">": "amount > 1", ">=": "amount >= 1", "+": "amount + 1", "-": "amount - 1",
		"*": "amount * 2", "/": "amount / 2", "%": "amount % 2",
		"in": "amount IN (1, 2)", "not in": "amount NOT IN (1, 2)",
		"between": "amount BETWEEN 1 AND 2", "not between": "amount NOT BETWEEN 1 AND 2",
		"like": "employee_name LIKE 'x%'", "~~": "employee_name LIKE 'x%'",
		"ilike": "employee_name ILIKE 'x%'", "~~*": "employee_name ILIKE 'x%'",
		"not like": "employee_name NOT LIKE 'x%'", "!~~": "employee_name NOT LIKE 'x%'",
		"not ilike": "employee_name NOT ILIKE 'x%'", "!~~*": "employee_name NOT ILIKE 'x%'",
	}
	parsedAs := map[string]string{"!=": "<>"}
	authorize := func(operators []string, expression string) error {
		_, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
			SQL: "SELECT " + expression + " AS r FROM expense_detail", RowLimit: 10,
			Grant: sqlpolicy.Grant{Products: []sqlpolicy.ProductGrant{{
				LogicalName: "expense_detail", PhysicalSchema: "reporting", PhysicalView: "expense_detail",
				ApprovedColumns: []string{"department", "amount", "employee_name"}, AllowedOperators: operators,
				MandatoryScope: []sqlpolicy.ScopePredicate{{Column: "department", Operator: sqlpolicy.ScopeEqual, Values: []string{"x"}}},
			}}},
		})
		return err
	}
	words := make([]string, 0, len(safeOperators))
	for word := range safeOperators {
		words = append(words, word)
	}
	sort.Strings(words)
	for _, word := range words {
		if err := declareOperators(t, `["`+word+`"]`); err != nil {
			t.Fatalf("Catalog rejects %q declared alone: %v", word, err)
		}
		declared, sample := word, samples[word]
		if alias, ok := parsedAs[word]; ok {
			declared, sample = alias, "amount "+word+" 1"
		}
		if sample == "" {
			t.Fatalf("vocabulary word %q has no sample proving it admits anything", word)
		}
		if err := authorize([]string{declared}, sample); err != nil {
			t.Fatalf("%q declared alone does not admit %s: %v", declared, sample, err)
		}
		unrelated := "/"
		if word == "/" {
			unrelated = "+"
		}
		if err := authorize([]string{unrelated}, sample); !sqlpolicy.IsCode(err, sqlpolicy.CodeOperatorNotAllowed) {
			t.Fatalf("%s under [%s] error = %v, want %s", sample, unrelated, err, sqlpolicy.CodeOperatorNotAllowed)
		}
	}
	for word := range samples {
		if _, ok := safeOperators[word]; !ok {
			t.Fatalf("sample for %q, which the vocabulary does not hold", word)
		}
	}
}
