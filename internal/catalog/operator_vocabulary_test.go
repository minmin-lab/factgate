package catalog

import (
	"errors"
	"strings"
	"testing"
)

// TestCatalogAcceptsLikeOperatorUnderEitherSpelling: the LIKE family may be
// declared by its word or by its PostgreSQL AST symbol; anything else is still
// outside the safe vocabulary.
func TestCatalogAcceptsLikeOperatorUnderEitherSpelling(t *testing.T) {
	const declared = `allowed_operators: ["=", ">=", "<="]`
	base := validCatalogYAML(t)
	if !strings.Contains(base, declared) {
		t.Fatalf("fixture no longer declares %s", declared)
	}
	declare := func(operators string) error {
		_, err := Parse([]byte(strings.Replace(base, declared, "allowed_operators: "+operators, 1)))
		return err
	}
	for _, operators := range []string{
		`["=", "~~"]`, `["=", "like"]`, `["=", "~~*"]`, `["=", "ilike"]`, `["=", "!~~"]`, `["=", "!~~*"]`,
		`["=", "not like"]`, `["=", "not ilike"]`,
	} {
		if err := declare(operators); err != nil {
			t.Fatalf("%s rejected: %v", operators, err)
		}
	}
	for _, operators := range []string{`["=", "~~~"]`, `["=", "~"]`, `["=", "similar to"]`} {
		err := declare(operators)
		if !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), "operator is not in the safe catalog vocabulary") {
			t.Fatalf("%s error = %v, want safe-vocabulary rejection", operators, err)
		}
	}
}
