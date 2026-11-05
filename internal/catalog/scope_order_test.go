package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Ordered scope dimensions (KB1-LOOP-152): an enum scope may list its values
// lowest first; validation requires that list to be a permutation of
// allowed_values, and DownClosure widens a request to every value not above
// its highest.

var fiveGrades = []string{"public", "internal", "secret", "confidential", "top_secret"}

// catalogWithScope is the repository Catalog with one more scope declaration.
func catalogWithScope(t *testing.T, scope map[string]any) []byte {
	t.Helper()
	base, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(base, &document); err != nil {
		t.Fatal(err)
	}
	scopes, _ := document["scopes"].([]any)
	document["scopes"] = append(scopes, scope)
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func gradeScope(order ...string) map[string]any {
	scope := map[string]any{"name": "grade", "type": "enum", "description": "classification grade",
		"allowed_values": []any{"confidential", "internal", "public", "secret", "top_secret"}}
	if order != nil {
		values := make([]any, 0, len(order))
		for _, value := range order {
			values = append(values, value)
		}
		scope["order"] = values
	}
	return scope
}

func TestScopeOrderIsAPermutationOfAllowedValues(t *testing.T) {
	parsed, err := Parse(catalogWithScope(t, gradeScope(fiveGrades...)))
	if err != nil {
		t.Fatalf("a valid order is refused: %v", err)
	}
	if got := parsed.ScopeOrder("grade"); !reflect.DeepEqual(got, fiveGrades) {
		t.Fatalf("ScopeOrder(grade) = %v, want %v", got, fiveGrades)
	}
	if got := parsed.ScopeOrder("department"); got != nil {
		t.Fatalf("an unordered scope reports order %v", got)
	}
	if got := parsed.ScopeOrder("nope"); got != nil {
		t.Fatalf("an unknown scope reports order %v", got)
	}
	// allowed_values keeps its own (byte) order; the order is a separate list.
	if _, err := Parse(catalogWithScope(t, gradeScope())); err != nil {
		t.Fatalf("a scope without order is refused: %v", err)
	}

	dateRange := map[string]any{"name": "graded_date", "type": "date_range", "description": "a date range",
		"min": "2026-01-01", "max": "2026-12-31", "order": []any{"2026-01-01"}}
	for name, scope := range map[string]map[string]any{
		"missing a value": gradeScope("public", "internal", "secret", "confidential"),
		"unknown value":   gradeScope("public", "internal", "secret", "confidential", "leadership"),
		"duplicate value": gradeScope("public", "internal", "secret", "secret", "top_secret"),
		"extra value":     gradeScope("public", "internal", "secret", "confidential", "top_secret", "leadership"),
		"not an enum":     dateRange,
	} {
		_, err := Parse(catalogWithScope(t, scope))
		if !errors.Is(err, ErrInvalidCatalog) || !strings.Contains(err.Error(), ".order") {
			t.Fatalf("%s: error = %v, want an invalid-catalog error on the order", name, err)
		}
	}
}

func TestScopeDownClosure(t *testing.T) {
	ordered := Scope{Name: "grade", Type: ScopeTypeEnum, AllowedValues: []string{"confidential", "internal", "public", "secret", "top_secret"},
		Order: fiveGrades}
	for _, tc := range []struct {
		values []string
		want   []string
	}{
		{[]string{"top_secret"}, fiveGrades},
		{[]string{"secret"}, []string{"public", "internal", "secret"}},
		{[]string{"internal", "confidential"}, []string{"public", "internal", "secret", "confidential"}},
		{[]string{"public"}, []string{"public"}},
		{[]string{"secret", "internal", "secret"}, []string{"public", "internal", "secret"}},
		// Defensive: a value the order does not list is kept, after the closure.
		{[]string{"leadership", "internal"}, []string{"public", "internal", "leadership"}},
	} {
		if got := ordered.DownClosure(tc.values); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("DownClosure(%v) = %v, want %v", tc.values, got, tc.want)
		}
	}
	if got := ordered.DownClosure(nil); len(got) != 0 {
		t.Fatalf("DownClosure(nil) = %v, want empty", got)
	}
	if got := ordered.DownClosure([]string{}); len(got) != 0 {
		t.Fatalf("DownClosure([]) = %v, want empty", got)
	}
	unordered := Scope{Name: "department", Type: ScopeTypeEnum, AllowedValues: []string{"a", "b", "c"}}
	values := []string{"c", "a"}
	got := unordered.DownClosure(values)
	if !reflect.DeepEqual(got, []string{"c", "a"}) {
		t.Fatalf("an unordered scope changed the values: %v", got)
	}
	got[0] = "changed"
	if values[0] != "c" {
		t.Fatal("DownClosure of an unordered scope aliases its input")
	}
}
