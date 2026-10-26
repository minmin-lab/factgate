package catalog

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDelegatedAuthorizationParsesAndValidates(t *testing.T) {
	var block struct {
		Delegated *DelegatedAuthorization `yaml:"delegated_authorization"`
	}
	text := "delegated_authorization:\n  issuer_role: {attribute: job_family, any_of: [dept_admin]}\n  child_self_confirm: true\n"
	if err := yaml.Unmarshal([]byte(text), &block); err != nil || block.Delegated == nil {
		t.Fatalf("parse %v %+v", err, block.Delegated)
	}
	if block.Delegated.IssuerRole.Attribute != "job_family" || block.Delegated.IssuerRole.AnyOf[0] != "dept_admin" || !block.Delegated.ChildSelfConfirm {
		t.Fatalf("parsed %+v", block.Delegated)
	}
	if problems := validateDelegatedAuthorization("delegated_authorization", *block.Delegated); len(problems) != 0 {
		t.Fatalf("a complete block was refused: %v", problems)
	}
	for name, delegated := range map[string]DelegatedAuthorization{
		"no values":       {IssuerRole: AttributeCondition{Attribute: "job_family"}},
		"bad attribute":   {IssuerRole: AttributeCondition{Attribute: "Job Family", AnyOf: []string{"dept_admin"}}},
		"empty value":     {IssuerRole: AttributeCondition{Attribute: "job_family", AnyOf: []string{""}}},
		"self-confirm on": {ChildSelfConfirm: true},
	} {
		if problems := validateDelegatedAuthorization("delegated_authorization", delegated).Error(); !strings.Contains(problems, "issuer_role") {
			t.Fatalf("%s was accepted: %q", name, problems)
		}
	}
}
