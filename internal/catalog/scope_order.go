package catalog

// Ordered scope dimensions (KB1-LOOP-152; the author's decision of
// 2026-10-28). An enum scope that declares order lists its values lowest
// first; asking for one value means asking for every value below it as well.
// The widening happens once, when a task is requested, so the manifest the
// approver sees and the grant they sign hold the whole set; enforcement never
// reads the order.

// ScopeOrder returns the order the named scope declares, lowest first, or nil
// when the scope is unknown or unordered.
func (c *Catalog) ScopeOrder(name string) []string {
	if c == nil {
		return nil
	}
	for _, scope := range c.Scopes {
		if scope.Name == name {
			if len(scope.Order) == 0 {
				return nil
			}
			return append([]string(nil), scope.Order...)
		}
	}
	return nil
}

// DownClosure returns every value of the scope's order not above the highest
// of values, lowest first and without duplicates. A value the order does not
// list is kept after them, in the order given (validation rules such values
// out; this is defensive). A scope without an order returns a copy of
// values unchanged.
func (s Scope) DownClosure(values []string) []string {
	if len(s.Order) == 0 {
		return append([]string(nil), values...)
	}
	rank := make(map[string]int, len(s.Order))
	for index, value := range s.Order {
		if _, seen := rank[value]; !seen {
			rank[value] = index
		}
	}
	top := -1
	var unlisted []string
	seenUnlisted := map[string]bool{}
	for _, value := range values {
		if position, listed := rank[value]; listed {
			if position > top {
				top = position
			}
		} else if !seenUnlisted[value] {
			seenUnlisted[value] = true
			unlisted = append(unlisted, value)
		}
	}
	closure := make([]string, 0, top+1+len(unlisted))
	seen := make(map[string]bool, top+1)
	for _, value := range s.Order[:top+1] {
		if !seen[value] {
			seen[value] = true
			closure = append(closure, value)
		}
	}
	return append(closure, unlisted...)
}
