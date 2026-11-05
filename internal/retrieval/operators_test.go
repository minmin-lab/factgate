package retrieval

import (
	"reflect"
	"strings"
	"testing"
)

func TestAdmittedOperatorsReadTheNormalizerTable(t *testing.T) {
	for attributeType, want := range map[string][]string{
		"enum":     {"<>", "=", "in"},
		"text":     {"<>", "=", "in"},
		"text_set": {"has"},
		"date":     {"<", "<=", "<>", "=", ">", ">=", "in"},
		"numeric":  {"<", "<=", "<>", "=", ">", ">=", "in"},
	} {
		if got := AdmittedOperators(attributeType); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s operators %v, want %v", attributeType, got, want)
		}
	}
	if AdmittedOperators("geometry") != nil {
		t.Fatal("an unknown attribute type admitted operators")
	}
}

func TestQueryTextBoundIsTheCommitmentBound(t *testing.T) {
	if _, err := commitQuery(Query{Text: strings.Repeat("a", MaxQueryTextBytes)}, 0, false); err != nil {
		t.Fatalf("a text of exactly MaxQueryTextBytes was refused: %v", err)
	}
	if _, err := commitQuery(Query{Text: strings.Repeat("a", MaxQueryTextBytes+1)}, 0, false); err == nil {
		t.Fatal("a text one byte over MaxQueryTextBytes was committed")
	}
	// 1366 three-byte characters are 4098 bytes: within a 4096-character
	// schema bound, outside the byte bound.
	if _, err := commitQuery(Query{Text: strings.Repeat("制", 1366)}, 0, false); err == nil {
		t.Fatal("the byte bound counted characters")
	}
}
