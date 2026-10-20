package retrieval

import "sort"

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
