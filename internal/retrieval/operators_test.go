package retrieval

import (
	"reflect"
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
