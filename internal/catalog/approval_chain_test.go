package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func testOrgChain() ApprovalChain {
	return ApprovalChain{
		RequesterAttribute: "department", DepartmentScope: "kb_department",
		Directors: map[string]string{"finance": "finance-director", "sales": "sales-director",
			"marketing": "marketing-director", "legal": "legal-director"},
		DirectorAuthority: ApprovalAuthority{Attribute: "department", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{"director"}}},
		Escalation: &ApprovalEscalation{Scope: "classification", Values: []string{"leadership"},
			Leaders: map[string]string{"finance": "leader-admin", "legal": "leader-admin",
				"sales": "leader-business", "marketing": "leader-business"},
			LeaderAuthority: ApprovalAuthority{Attribute: "oversees", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{"leader"}}}},
	}
}

func TestApprovalChainStepsFollowTheOrgChart(t *testing.T) {
	chain := testOrgChain()
	step := func(role, approver string, departments ...string) ApprovalStep {
		return ApprovalStep{Role: role, Departments: departments, Approver: approver}
	}
	cases := []struct {
		name  string
		scope map[string][]string
		want  []ApprovalStep
	}{
		{"own department", map[string][]string{"kb_department": {"finance"}, "classification": {"internal"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "finance-director", "finance")}},
		{"another department", map[string][]string{"kb_department": {"sales"}, "classification": {"confidential", "internal"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "finance-director", "finance"),
				step(ApprovalStepDepartmentDirector, "sales-director", "sales")}},
		{"leadership across two lines", map[string][]string{"kb_department": {"finance", "legal", "marketing", "sales"}, "classification": {"leadership"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "finance-director", "finance"),
				step(ApprovalStepDepartmentDirector, "legal-director", "legal"),
				step(ApprovalStepDepartmentDirector, "marketing-director", "marketing"),
				step(ApprovalStepDepartmentDirector, "sales-director", "sales"),
				step(ApprovalStepLeader, "leader-admin", "finance", "legal"),
				step(ApprovalStepLeader, "leader-business", "marketing", "sales")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chain.Steps("finance", tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("steps\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
	// A narrowed scope needs only what it still requests.
	narrowed, err := chain.Steps("finance", map[string][]string{"kb_department": {"sales"}, "classification": {"internal"}})
	if err != nil || len(narrowed) != 2 || narrowed[1].Approver != "sales-director" {
		t.Fatalf("narrowed steps %+v %v", narrowed, err)
	}
	if _, err := chain.Steps("hr", map[string][]string{"kb_department": {"sales"}}); err == nil {
		t.Fatal("a requester department without a director produced a chain")
	}
	if _, err := chain.Steps("finance", map[string][]string{"kb_department": {"research"}}); err == nil {
		t.Fatal("a department without a director produced a chain")
	}
}

func TestApprovalChainValidationRequiresACompleteOrgChart(t *testing.T) {
	scopes := map[string]Scope{
		"kb_department":  {Name: "kb_department", Type: ScopeTypeEnum, AllowedValues: []string{"finance", "legal", "marketing", "sales"}},
		"classification": {Name: "classification", Type: ScopeTypeEnum, AllowedValues: []string{"confidential", "internal", "leadership", "public"}},
	}
	if problems := validateApprovalChain("chain", testOrgChain(), scopes); len(problems) != 0 {
		t.Fatalf("a complete chain was refused: %v", problems)
	}
	incomplete := testOrgChain()
	delete(incomplete.Directors, "legal")
	incomplete.Escalation.Leaders["research"] = "leader-rd"
	problems := validateApprovalChain("chain", incomplete, scopes).Error()
	for _, want := range []string{`department "legal" has no approver`, `"research" is not a value of the department scope`} {
		if !strings.Contains(problems, want) {
			t.Fatalf("validation %q lacks %q", problems, want)
		}
	}
	route := ApprovalRoute{Sensitivity: "high", Mode: "manual", BudgetProfile: "p", Chain: &ApprovalChain{}}
	if problems := validateApprovalRoute("route", route, map[string]BudgetProfile{"p": {}}); len(problems) != 0 {
		t.Fatalf("a chain route without an approver was refused: %v", problems)
	}
	route.Approver = "bob"
	if problems := validateApprovalRoute("route", route, map[string]BudgetProfile{"p": {}}); len(problems) == 0 {
		t.Fatal("a chain route naming an approver was accepted")
	}
	chain := testOrgChain()
	original := ApprovalRoute{Chain: &chain}
	copied := cloneApprovalRoute(original)
	copied.Chain.Directors["finance"] = "mutated"
	copied.Chain.Escalation.Leaders["sales"] = "mutated"
	if original.Chain.Directors["finance"] != "finance-director" || original.Chain.Escalation.Leaders["sales"] != "leader-business" {
		t.Fatal("cloning a route shares its org chart")
	}
}
