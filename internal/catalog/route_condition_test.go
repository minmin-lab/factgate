package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Scope-conditioned approval routes: a route that names an approver for tasks
// whose mandatory scope holds a listed value takes precedence over the
// unconditioned route for the same sensitivity and products, in declared
// order. The condition chooses who approves; it never changes what is granted.

func catalogWithRoutes(t *testing.T, extra ...map[string]any) []byte {
	t.Helper()
	base, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(base, &document); err != nil {
		t.Fatal(err)
	}
	routes, _ := document["approval_routes"].([]any)
	for _, route := range extra {
		routes = append(routes, route)
	}
	document["approval_routes"] = routes
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func conditioned(sensitivity, approver string, condition map[string]any) map[string]any {
	return map[string]any{"sensitivity": sensitivity, "mode": "manual", "approver": approver,
		"budget_profile": "detail-manual-v5", "scope_condition": condition}
}

func TestConditionedRouteChoosesTheApproverByScope(t *testing.T) {
	parsed, err := Parse(catalogWithRoutes(t,
		conditioned("high", "dana", map[string]any{"department": []any{"财务部"}}),
		conditioned("high", "erin", map[string]any{"department": []any{"研发部", "财务部"}})))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// expense_detail is the base catalog's unscoped high-sensitivity product.
	medium := "expense_detail"
	for _, tc := range []struct {
		scope    map[string][]string
		approver string
	}{
		{map[string][]string{"department": {"销售部"}}, "bob"},
		{map[string][]string{"department": {"财务部"}}, "dana"},        // first matching route in declared order
		{map[string][]string{"department": {"研发部"}}, "erin"},        // only the second route matches
		{map[string][]string{"department": {"销售部", "财务部"}}, "dana"}, // any listed value suffices
		{nil, "bob"},
	} {
		policy, err := parsed.ResolveTaskPolicyForScope([]string{medium}, tc.scope)
		if err != nil {
			t.Fatalf("%v: %v", tc.scope, err)
		}
		if policy.ApprovalRoute.Approver != tc.approver {
			t.Fatalf("scope %v routed to %q, want %q", tc.scope, policy.ApprovalRoute.Approver, tc.approver)
		}
	}
	plain, err := parsed.ResolveTaskPolicy([]string{medium})
	if err != nil || plain.ApprovalRoute.Approver != "bob" {
		t.Fatalf("scope-blind resolution %+v %v, want the unconditioned route", plain.ApprovalRoute, err)
	}
}

func TestConditionedRouteValidationFailsClosed(t *testing.T) {
	for name, route := range map[string]map[string]any{
		"unknown scope":      conditioned("medium", "dana", map[string]any{"clearance": []any{"leadership"}}),
		"not an enum":        conditioned("medium", "dana", map[string]any{"expense_date": []any{"2026-01-01"}}),
		"value not admitted": conditioned("medium", "dana", map[string]any{"department": []any{"市场部"}}),
		"unsorted":           conditioned("medium", "dana", map[string]any{"department": []any{"财务部", "研发部"}}),
		"empty":              conditioned("medium", "dana", map[string]any{"department": []any{}}),
		"no fallback":        map[string]any{"sensitivity": "medium", "products": []any{"no_such_product"}, "mode": "manual", "approver": "dana", "budget_profile": "detail-manual-v5", "scope_condition": map[string]any{"department": []any{"财务部"}}},
	} {
		if _, err := Parse(catalogWithRoutes(t, route)); err == nil {
			t.Fatalf("%s: a malformed conditioned route was accepted", name)
		}
	}
	duplicate := conditioned("medium", "dana", map[string]any{"department": []any{"财务部"}})
	if _, err := Parse(catalogWithRoutes(t, duplicate, conditioned("medium", "erin", map[string]any{"department": []any{"财务部"}}))); err == nil ||
		!strings.Contains(err.Error(), "conditioned approval route is duplicated") {
		t.Fatalf("duplicate conditioned routes: %v", err)
	}
}
