package catalog

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gradedChain is the second demo round's org chart (KB1-LOOP-148): department
// administrators at level 0, then directors (secret and above), vice
// presidents (confidential, top_secret) and the general manager (top_secret).
func gradedChain() ApprovalChain {
	authority := func(role string) ApprovalAuthority {
		return ApprovalAuthority{Attribute: "authority_departments", Role: AttributeCondition{Attribute: "job_family", AnyOf: []string{role}}}
	}
	return ApprovalChain{
		RequesterAttribute: "department", DepartmentScope: "kb_department",
		Directors:         map[string]string{"finance": "finance-admin", "hr": "hr-admin", "sales": "sales-admin"},
		DirectorAuthority: authority("dept_admin"),
		Escalations: []ApprovalEscalation{
			{Role: "director", Label: "部门主任", Scope: "classification", Values: []string{"confidential", "secret", "top_secret"},
				Leaders: map[string]string{"finance": "finance-director", "hr": "hr-director", "sales": "sales-director"}, LeaderAuthority: authority("director")},
			{Role: "vice_president", Label: "分管领导", Scope: "classification", Values: []string{"confidential", "top_secret"},
				Leaders: map[string]string{"finance": "vp-admin", "hr": "vp-admin", "sales": "vp-business"}, LeaderAuthority: authority("vice_president")},
			{Role: "general_manager", Label: "总经理", Scope: "classification", Values: []string{"top_secret"},
				Leaders: map[string]string{"finance": "general-manager", "hr": "general-manager", "sales": "general-manager"}, LeaderAuthority: authority("general_manager")},
		},
	}
}

func approvers(steps []ApprovalStep) string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Approver+"["+strings.Join(step.Departments, "+")+"]")
	}
	return strings.Join(names, ",")
}

func TestStepsForDropsWhatTheRequesterHoldsAuthorityFor(t *testing.T) {
	chain := gradedChain()
	scope := func(classification string, departments ...string) map[string][]string {
		return map[string][]string{"kb_department": departments, "classification": {classification}}
	}
	all := []string{"finance", "hr", "sales"}
	cases := []struct {
		name, department, subject string
		authority                 []string
		scope                     map[string][]string
		want                      string
	}{
		{"administrator, own department, internal", "finance", "finance-admin", []string{"finance"}, scope("internal", "finance"), ""},
		{"administrator, own department, secret", "finance", "finance-admin", []string{"finance"}, scope("secret", "finance"), "finance-director[finance]"},
		{"administrator, own department, confidential", "finance", "finance-admin", []string{"finance"}, scope("confidential", "finance"),
			"finance-director[finance],vp-admin[finance]"},
		{"administrator across departments", "finance", "finance-admin", []string{"finance"}, scope("internal", "finance", "sales"),
			"finance-director[finance],sales-admin[sales]"},
		{"administrator asking only another department", "finance", "finance-admin", []string{"finance"}, scope("internal", "sales"),
			"finance-director[finance],sales-admin[sales]"},
		{"director, own department, secret", "finance", "finance-director", []string{"finance"}, scope("secret", "finance"), ""},
		{"director, own department, confidential", "finance", "finance-director", []string{"finance"}, scope("confidential", "finance"), "vp-admin[finance]"},
		{"director across departments", "finance", "finance-director", []string{"finance"}, scope("internal", "finance", "sales"),
			"vp-admin[finance],sales-admin[sales]"},
		{"vice president, their line, confidential", "finance", "vp-admin", []string{"finance", "hr"}, scope("confidential", "finance", "hr"), ""},
		{"vice president, their line, top secret", "finance", "vp-admin", []string{"finance", "hr"}, scope("top_secret", "finance", "hr"),
			"general-manager[finance+hr]"},
		{"general manager, everything", "finance", "general-manager", all, scope("top_secret", all...), ""},
		{"staff, own department", "finance", "staff", nil, scope("internal", "finance"), "finance-admin[finance]"},
		{"staff, across departments, secret", "finance", "staff", nil, scope("secret", "finance", "sales"),
			"finance-admin[finance],sales-admin[sales],finance-director[finance],sales-director[sales]"},
		// An attribute that does not back the org chart: the administrator is
		// named but holds no authority, so the next layer approves instead.
		{"administrator without the authority attribute", "finance", "finance-admin", nil, scope("internal", "finance"), "finance-director[finance]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chain.StepsFor(tc.department, tc.subject, tc.authority, tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if approvers(got) != tc.want {
				t.Fatalf("chain %q, want %q", approvers(got), tc.want)
			}
		})
	}
	// Without a requester subject it is Steps.
	plain, _ := chain.Steps("finance", scope("secret", "finance"))
	if got, err := chain.StepsFor("finance", "", []string{"finance"}, scope("secret", "finance")); err != nil || !reflect.DeepEqual(got, plain) {
		t.Fatalf("StepsFor without a subject %+v %v, want %+v", got, err, plain)
	}
	// Nobody above the general manager: crossing outside their (here
	// incomplete) authority cannot be replaced.
	if _, err := chain.StepsFor("finance", "general-manager", []string{"finance"}, scope("internal", "finance", "sales")); err == nil {
		t.Fatal("a step with no one above the requester was accepted")
	}
	if level := chain.SubjectLevel("vp-business"); level != 2 {
		t.Fatalf("vice president level %d, want 2", level)
	}
	if level := chain.SubjectLevel("staff"); level != -1 {
		t.Fatalf("staff level %d, want -1", level)
	}
}

func TestCenterAndStewardRules(t *testing.T) {
	chain := gradedChain()
	center := func(classification string, centers []string, departments ...string) map[string][]string {
		return map[string][]string{"kb_department": departments, "classification": {classification}, "center": centers}
	}
	heads := []string{"finance-a"}
	if !chain.CenterCovers("finance", center("internal", []string{"finance-a"}, "finance"), "center", heads) {
		t.Fatal("a center head's own center was not covered")
	}
	for name, scope := range map[string]map[string][]string{
		"whole department": center("internal", []string{"finance-a", "finance-b"}, "finance"),
		"other department": center("internal", []string{"finance-a"}, "finance", "sales"),
		"escalated":        center("secret", []string{"finance-a"}, "finance"),
		"no center":        {"kb_department": {"finance"}, "classification": {"internal"}},
	} {
		if chain.CenterCovers("finance", scope, "center", heads) {
			t.Fatalf("%s was covered", name)
		}
	}
	if chain.CenterCovers("finance", center("internal", []string{"finance-a"}, "finance"), "", heads) {
		t.Fatal("the center rule applied without a center scope")
	}

	// The steward replaces departments outside authority, never the
	// requester's own.
	scope := map[string][]string{"kb_department": {"finance", "hr", "sales"}, "classification": {"internal"}}
	if got := chain.StewardScope("hr", []string{"hr"}, []string{"hr"}, scope)["kb_department"]; !reflect.DeepEqual(got, []string{"hr"}) {
		t.Fatalf("hr administrator's steward scope %v", got)
	}
	if got := chain.StewardScope("finance", nil, []string{"hr"}, scope)["kb_department"]; !reflect.DeepEqual(got, []string{"finance", "hr"}) {
		t.Fatalf("finance staff's steward scope %v", got)
	}
	if got := chain.StewardScope("finance", nil, nil, scope); !reflect.DeepEqual(got, scope) {
		t.Fatalf("no steward changed the scope: %v", got)
	}
	if !reflect.DeepEqual(scope["kb_department"], []string{"finance", "hr", "sales"}) {
		t.Fatalf("the steward rule changed its input: %v", scope)
	}
	steps, err := chain.StepsFor("hr", "hr-admin", []string{"hr"}, chain.StewardScope("hr", []string{"hr"}, []string{"hr"}, scope))
	if err != nil || len(steps) != 0 {
		t.Fatalf("hr administrator's company-wide personal records: %s %v", approvers(steps), err)
	}
}

func TestDelegatedRuleFieldsValidate(t *testing.T) {
	block := DelegatedAuthorization{IssuerRole: AttributeCondition{Attribute: "job_family", AnyOf: []string{"dept_admin"}},
		AuthorityAttribute: "authority_departments", CenterScope: "center", PersonalScope: "employee_id"}
	if problems := validateDelegatedAuthorization("delegated_authorization", block); len(problems) != 0 {
		t.Fatalf("a complete block was refused: %v", problems)
	}
	if block.AuthorityAttributeName() != "authority_departments" || (DelegatedAuthorization{}).CenterAttributeName() != DefaultCenterAttribute {
		t.Fatal("attribute name defaults")
	}
	bad := block
	bad.PersonalScope = "Employee ID"
	if problems := validateDelegatedAuthorization("delegated_authorization", bad).Error(); !strings.Contains(problems, "personal_scope") {
		t.Fatalf("a malformed personal scope was accepted: %q", problems)
	}
	scopes := map[string]Scope{"center": {Name: "center", Type: ScopeTypeEnum, AllowedValues: []string{"finance-a"}},
		"expense_date": {Name: "expense_date", Type: ScopeTypeDateRange}}
	if problems := validateDelegatedScopes("delegated_authorization", block, scopes).Error(); !strings.Contains(problems, "personal_scope") ||
		strings.Contains(problems, "center_scope") {
		t.Fatalf("scope references: %q", problems)
	}
	dated := block
	dated.CenterScope, dated.PersonalScope = "expense_date", ""
	if problems := validateDelegatedScopes("delegated_authorization", dated, scopes).Error(); !strings.Contains(problems, "center_scope") {
		t.Fatalf("a date-range center scope was accepted: %q", problems)
	}

	departments := map[string]Scope{"kb_department": {Name: "kb_department", Type: ScopeTypeEnum, AllowedValues: []string{"finance", "hr", "sales"}}}
	chain := gradedChain()
	catalog := &Catalog{Products: []Product{{Name: "attendance", StewardDepartment: "hr"}},
		ApprovalRoutes: []ApprovalRoute{{Chain: &chain}}}
	if problems := catalog.validateStewardDepartments(departments); len(problems) != 0 {
		t.Fatalf("a known steward was refused: %v", problems)
	}
	catalog.Products[0].StewardDepartment = "legal"
	if problems := catalog.validateStewardDepartments(departments).Error(); !strings.Contains(problems, "steward_department") {
		t.Fatalf("an unknown steward was accepted: %q", problems)
	}
	catalog.Products[0].StewardDepartment, catalog.ApprovalRoutes = "hr", []ApprovalRoute{{Approver: "bob"}}
	if problems := catalog.validateStewardDepartments(departments).Error(); !strings.Contains(problems, "needs an approval chain") {
		t.Fatalf("a steward without a chain was accepted: %q", problems)
	}
}

// levelledChain is the demo's chain once the author ranked directors with
// department administrators (KB1-LOOP-148): the director layer is level 0.
func levelledChain() ApprovalChain {
	chain := gradedChain()
	zero := 0
	chain.Escalations[0].Level = &zero
	return chain
}

func TestStepsForWithDirectorsLevelledWithAdministrators(t *testing.T) {
	chain := levelledChain()
	scope := func(classification string, departments ...string) map[string][]string {
		return map[string][]string{"kb_department": departments, "classification": {classification}}
	}
	finance := []string{"finance"}
	cases := []struct {
		name, subject string
		scope         map[string][]string
		want          string
	}{
		{"administrator, own department, secret", "finance-admin", scope("secret", "finance"), ""},
		{"administrator, own department, confidential", "finance-admin", scope("confidential", "finance"), "vp-admin[finance]"},
		{"administrator, own department, top secret", "finance-admin", scope("top_secret", "finance"), "vp-admin[finance],general-manager[finance]"},
		// Crossing goes to the next layer in org-chart order, whatever its level.
		{"administrator across departments", "finance-admin", scope("internal", "finance", "sales"), "finance-director[finance],sales-admin[sales]"},
		{"director across departments", "finance-director", scope("internal", "finance", "sales"), "vp-admin[finance],sales-admin[sales]"},
		{"director, own department, secret", "finance-director", scope("secret", "finance"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chain.StepsFor("finance", tc.subject, finance, tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if approvers(got) != tc.want {
				t.Fatalf("chain %q, want %q", approvers(got), tc.want)
			}
		})
	}
	if level := chain.SubjectLevel("finance-director"); level != 0 {
		t.Fatalf("levelled director level %d, want 0", level)
	}
	if level := chain.StepLevel(ApprovalStep{Role: "vice_president"}); level != 2 {
		t.Fatalf("vice president layer level %d, want its position 2", level)
	}
	// The level never changes who is on the chain without the rules.
	plain, _ := gradedChain().Steps("finance", scope("secret", "finance"))
	levelled, _ := chain.Steps("finance", scope("secret", "finance"))
	if approvers(plain) != approvers(levelled) {
		t.Fatalf("a level changed Steps: %q vs %q", approvers(levelled), approvers(plain))
	}
	// A cloned chain keeps its own level.
	clone := cloneApprovalChain(&chain)
	*clone.Escalations[0].Level = 3
	if *chain.Escalations[0].Level != 0 {
		t.Fatal("a clone shares its layer level")
	}
}

func TestLayerLevelValidation(t *testing.T) {
	for _, level := range []int{-1, 4} {
		chain := gradedChain()
		chain.Escalations[1].Level = &level
		if problems := validateLayerLevel("chain.escalations[1]", chain.Escalations[1], len(chain.Escalations)).Error(); !strings.Contains(problems, ".level") {
			t.Fatalf("level %d was accepted: %q", level, problems)
		}
	}
	for _, level := range []int{0, 3} {
		chain := gradedChain()
		chain.Escalations[1].Level = &level
		if problems := validateLayerLevel("chain.escalations[1]", chain.Escalations[1], len(chain.Escalations)); len(problems) != 0 {
			t.Fatalf("level %d was refused: %v", level, problems)
		}
	}
	single := testOrgChain()
	two := 2
	single.Escalation.Level = &two
	scopes := map[string]Scope{
		"kb_department":  {Name: "kb_department", Type: ScopeTypeEnum, AllowedValues: []string{"finance", "legal", "marketing", "sales"}},
		"classification": {Name: "classification", Type: ScopeTypeEnum, AllowedValues: []string{"confidential", "internal", "leadership", "public"}},
	}
	if problems := validateApprovalChain("chain", single, scopes).Error(); !strings.Contains(problems, "chain.escalation.level") {
		t.Fatalf("a single layer at level 2 was accepted: %q", problems)
	}
	var parsed ApprovalEscalation
	if err := yaml.Unmarshal([]byte("role: director\nlevel: 0\n"), &parsed); err != nil || parsed.Level == nil || *parsed.Level != 0 {
		t.Fatalf("level 0 did not parse: %v %v", err, parsed.Level)
	}
}
