package retrieval

import "sort"

// MaxQueryTextBytes bounds a text query after NormalizeText; the bound is in
// bytes, so a CJK query reaches it at about a third as many characters.
// commitQuery enforces it (TestQueryTextBoundIsTheCommitmentBound).
const MaxQueryTextBytes = 4096

// AdmittedOperators lists, sorted, the filter operators the closed language
// admits on an attribute of the given type, and nil for an unknown type. It
// reads the table Normalize enforces, so what discovery publishes cannot drift
// from what a request is checked against.
func AdmittedOperators(attributeType string) []string {
	admitted, known := operatorsByType[attributeType]
	if !known {
		return nil
	}
	operators := make([]string, 0, len(admitted))
	for operator := range admitted {
		operators = append(operators, operator)
	}
	sort.Strings(operators)
	return operators
}
