package sqlpolicy

import (
	"reflect"
	"testing"
)

func TestDefaultOperatorsIsACopyOfTheDefaultSet(t *testing.T) {
	operators := DefaultOperators()
	if !reflect.DeepEqual(operators, defaultOperators) {
		t.Fatalf("DefaultOperators %v, default set %v", operators, defaultOperators)
	}
	operators[0] = "mutated"
	if defaultOperators[0] == "mutated" {
		t.Fatal("DefaultOperators exposes the shared default set")
	}
}
