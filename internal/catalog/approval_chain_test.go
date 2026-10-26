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

// testLayeredChain is the delegated-authorization demo's shape (KB1-LOOP-147):
// department administrators approve, then the department directors for
// confidential or leadership data, then the leaders for leadership data.
func testLayeredChain() ApprovalChain {
	return ApprovalChain{
		RequesterAttribute: "department", DepartmentScope: "kb_department",
		Directors: map[string]string{"finance": "finance-admin", "sales": "sales-admin",
			"marketing": "marketing-admin", "legal": "legal-admin"},
		DirectorAuthority: ApprovalAuthority{Attribute: "department", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{"dept_admin"}}},
		Escalations: []ApprovalEscalation{
			{Role: "director", Label: "部门主任", Scope: "classification", Values: []string{"confidential", "leadership"},
				Leaders: map[string]string{"finance": "finance-director", "legal": "legal-director",
					"sales": "sales-director", "marketing": "marketing-director"},
				LeaderAuthority: ApprovalAuthority{Attribute: "department", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{"director"}}}},
			{Role: "leader", Label: "分管领导", Scope: "classification", Values: []string{"leadership"},
				Leaders: map[string]string{"finance": "leader-admin", "legal": "leader-admin",
					"sales": "leader-business", "marketing": "leader-business"},
				LeaderAuthority: ApprovalAuthority{Attribute: "oversees", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{"leader"}}}},
		},
	}
}

func TestApprovalChainWalksEscalationLayersInOrder(t *testing.T) {
	chain := testLayeredChain()
	step := func(role, label, approver string, departments ...string) ApprovalStep {
		return ApprovalStep{Role: role, Departments: departments, Approver: approver, Label: label}
	}
	cases := []struct {
		name  string
		scope map[string][]string
		want  []ApprovalStep
	}{
		{"internal needs the administrators only", map[string][]string{"kb_department": {"sales"}, "classification": {"internal"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "", "finance-admin", "finance"),
				step(ApprovalStepDepartmentDirector, "", "sales-admin", "sales")}},
		{"confidential adds the directors", map[string][]string{"kb_department": {"finance", "sales"}, "classification": {"confidential"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "", "finance-admin", "finance"),
				step(ApprovalStepDepartmentDirector, "", "sales-admin", "sales"),
				step("director", "部门主任", "finance-director", "finance"),
				step("director", "部门主任", "sales-director", "sales")}},
		{"leadership adds the directors and then the leaders", map[string][]string{"kb_department": {"legal", "marketing"}, "classification": {"internal", "leadership"}},
			[]ApprovalStep{step(ApprovalStepRequesterDirector, "", "finance-admin", "finance"),
				step(ApprovalStepDepartmentDirector, "", "legal-admin", "legal"),
				step(ApprovalStepDepartmentDirector, "", "marketing-admin", "marketing"),
				step("director", "部门主任", "legal-director", "legal"),
				step("director", "部门主任", "marketing-director", "marketing"),
				step("leader", "分管领导", "leader-admin", "legal"),
				step("leader", "分管领导", "leader-business", "marketing")}},
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
	for _, step := range []ApprovalStep{{Role: "director"}, {Role: "leader"}} {
		if authority, ok := chain.Authority(step); !ok || authority.Role.AnyOf[0] != step.Role {
			t.Fatalf("authority of layer %s = %+v %v", step.Role, authority, ok)
		}
	}
	if _, ok := chain.Authority(ApprovalStep{Role: "executive"}); ok {
		t.Fatal("an undeclared layer role has an authority")
	}
	// The single-layer form is one layer whose role defaults to leader.
	legacy := testOrgChain().Layers()
	if len(legacy) != 1 || legacy[0].Role != ApprovalStepLeader || legacy[0].Leaders["sales"] != "leader-business" {
		t.Fatalf("legacy layers %+v", legacy)
	}
}

func TestApprovalChainValidationOfEscalationLayers(t *testing.T) {
	scopes := map[string]Scope{
		"kb_department":  {Name: "kb_department", Type: ScopeTypeEnum, AllowedValues: []string{"finance", "legal", "marketing", "sales"}},
		"classification": {Name: "classification", Type: ScopeTypeEnum, AllowedValues: []string{"confidential", "internal", "leadership", "public"}},
	}
	if problems := validateApprovalChain("chain", testLayeredChain(), scopes); len(problems) != 0 {
		t.Fatalf("a complete layered chain was refused: %v", problems)
	}
	both := testLayeredChain()
	legacy := testOrgChain()
	both.Escalation = legacy.Escalation
	if problems := validateApprovalChain("chain", both, scopes).Error(); !strings.Contains(problems, "not both") {
		t.Fatalf("escalation and escalations together were accepted: %q", problems)
	}
	duplicate := testLayeredChain()
	duplicate.Escalations[1].Role = "director"
	if problems := validateApprovalChain("chain", duplicate, scopes).Error(); !strings.Contains(problems, `"director" is declared twice`) {
		t.Fatalf("a duplicate layer role was accepted: %q", problems)
	}
	director := testLayeredChain()
	director.Escalations[0].Role = ApprovalStepDepartmentDirector
	if problems := validateApprovalChain("chain", director, scopes).Error(); !strings.Contains(problems, "must not be a director role") {
		t.Fatalf("a layer named after a director role was accepted: %q", problems)
	}
	unnamed := testLayeredChain()
	unnamed.Escalations[0].Role = ""
	if problems := validateApprovalChain("chain", unnamed, scopes).Error(); !strings.Contains(problems, "lowercase identifier") {
		t.Fatalf("a layer without a role was accepted: %q", problems)
	}
	incomplete := testLayeredChain()
	delete(incomplete.Escalations[1].Leaders, "legal")
	if problems := validateApprovalChain("chain", incomplete, scopes).Error(); !strings.Contains(problems, `escalations[1].leaders: department "legal" has no approver`) {
		t.Fatalf("an incomplete layer org chart was accepted: %q", problems)
	}
	chain := testLayeredChain()
	original := ApprovalRoute{Chain: &chain}
	copied := cloneApprovalRoute(original)
	copied.Chain.Escalations[0].Leaders["sales"] = "mutated"
	copied.Chain.Escalations[1].Values[0] = "mutated"
	if original.Chain.Escalations[0].Leaders["sales"] != "sales-director" || original.Chain.Escalations[1].Values[0] != "leadership" {
		t.Fatal("cloning a route shares its escalation layers")
	}
}
