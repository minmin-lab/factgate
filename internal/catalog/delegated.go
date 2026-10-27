package catalog

import "fmt"

// DelegatedAuthorization is the demo's coarse-grained, long-lived
// authorization model (KB1-LOOP-147; a demo feature, not a paper claim). It is
// a top-level Catalog block, so the Catalog digest carried by every manifest
// and receipt binds whether it was on; a Catalog without the block (the formal
// round's) behaves exactly as before.
type DelegatedAuthorization struct {
	// IssuerRole is what a department administrator's attested attributes
	// must show to issue an authorization for an employee (the
	// issue_data_authorization tool); the scope stays in the issuer's own
	// department.
	IssuerRole AttributeCondition `yaml:"issuer_role" json:"issuer_role"`
	// ChildSelfConfirm lets the human subject of a task tree confirm its child
	// tasks themselves: a child task's approver is then the requester, while a
	// root task is still approved by someone else unless a rule below makes
	// it self-service.
	ChildSelfConfirm bool `yaml:"child_self_confirm,omitempty" json:"child_self_confirm,omitempty"`
	// AuthorityAttribute names the attested attribute listing the departments
	// a subject has authority over (KB1-LOOP-148); empty means
	// DefaultAuthorityAttribute. An approval chain drops the steps a requester
	// holds the authority for (ApprovalChain.StepsFor).
	AuthorityAttribute string `yaml:"authority_attribute,omitempty" json:"authority_attribute,omitempty"`
	// CenterScope names the enum scope of the centers below a department; a
	// requester whose CenterAttribute holds every requested center of their
	// own department confirms their own request when it triggers no
	// escalation layer and every requested product declares the scope. Empty
	// switches the center rule off.
	CenterScope string `yaml:"center_scope,omitempty" json:"center_scope,omitempty"`
	// CenterAttribute names the attested attribute listing the centers a
	// subject heads; empty means DefaultCenterAttribute.
	CenterAttribute string `yaml:"center_attribute,omitempty" json:"center_attribute,omitempty"`
	// PersonalScope names the enum scope of personal records (for example
	// employee_id). A root task whose every product declares it and whose
	// values are exactly the requester's own values of the same-named
	// attribute is confirmed by the requester. Empty switches the rule off.
	PersonalScope string `yaml:"personal_scope,omitempty" json:"personal_scope,omitempty"`
	// AgentSubmits lets the agent submit a task for its human subject
	// (KB1-LOOP-149, author's decision 2026-10-28): request_data_task,
	// request_download_task and issue_data_authorization then create the
	// task already AWAITING_APPROVAL and its OA draft already pending, so a
	// person only approves. Off, a person submits the draft in the OA first,
	// as in the formal round. The Catalog digest binds the choice.
	AgentSubmits bool `yaml:"agent_submits,omitempty" json:"agent_submits,omitempty"`
}

// Default attribute names of the delegated-authorization rules.
const (
	DefaultAuthorityAttribute = "authority_departments"
	DefaultCenterAttribute    = "authority_center"
)

// AuthorityAttributeName is the attribute listing a subject's departments of
// authority.
func (d DelegatedAuthorization) AuthorityAttributeName() string {
	if d.AuthorityAttribute == "" {
		return DefaultAuthorityAttribute
	}
	return d.AuthorityAttribute
}

// CenterAttributeName is the attribute listing the centers a subject heads.
func (d DelegatedAuthorization) CenterAttributeName() string {
	if d.CenterAttribute == "" {
		return DefaultCenterAttribute
	}
	return d.CenterAttribute
}

func validateDelegatedAuthorization(path string, delegated DelegatedAuthorization) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(delegated.IssuerRole.Attribute) || len(delegated.IssuerRole.AnyOf) == 0 {
		problems = append(problems, fieldError(path+".issuer_role", "issuer role needs an attribute and at least one value", ErrInvalidApprovalRoute))
	}
	for _, value := range delegated.IssuerRole.AnyOf {
		if value == "" {
			problems = append(problems, fieldError(path+".issuer_role", "issuer role values must be non-empty", ErrInvalidApprovalRoute))
		}
	}
	for field, name := range map[string]string{"authority_attribute": delegated.AuthorityAttribute,
		"center_attribute": delegated.CenterAttribute, "center_scope": delegated.CenterScope, "personal_scope": delegated.PersonalScope} {
		if name != "" && !identifierPattern.MatchString(name) {
			problems = append(problems, fieldError(path+"."+field, "must be a lowercase identifier", ErrInvalidApprovalRoute))
		}
	}
	return problems
}

// validateDelegatedScopes requires the center and personal scopes to be
// Catalog enum scopes.
func validateDelegatedScopes(path string, delegated DelegatedAuthorization, scopes map[string]Scope) ValidationErrors {
	var problems ValidationErrors
	for field, name := range map[string]string{"center_scope": delegated.CenterScope, "personal_scope": delegated.PersonalScope} {
		if name == "" {
			continue
		}
		if scope, ok := scopes[name]; !ok || scope.Type != ScopeTypeEnum {
			problems = append(problems, fieldError(path+"."+field, fmt.Sprintf("%q must name a Catalog enum scope", name), ErrInvalidApprovalRoute))
		}
	}
	return problems
}

// validateStewardDepartments requires a product's steward department to be a
// department every approval chain knows: a value of each chain's department
// scope. A steward needs a chain to approve its records.
func (c *Catalog) validateStewardDepartments(scopes map[string]Scope) ValidationErrors {
	var problems ValidationErrors
	for index, product := range c.Products {
		if product.StewardDepartment == "" {
			continue
		}
		path := fmt.Sprintf("products[%d].steward_department", index)
		chains := 0
		for _, route := range c.ApprovalRoutes {
			if route.Chain == nil {
				continue
			}
			chains++
			if !containsString(scopes[route.Chain.DepartmentScope].AllowedValues, product.StewardDepartment) {
				problems = append(problems, fieldError(path, fmt.Sprintf("%q is not a value of the approval chain's department scope %s",
					product.StewardDepartment, route.Chain.DepartmentScope), ErrInvalidApprovalRoute))
			}
		}
		if chains == 0 {
			problems = append(problems, fieldError(path, "a steward department needs an approval chain route", ErrInvalidApprovalRoute))
		}
	}
	return problems
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
