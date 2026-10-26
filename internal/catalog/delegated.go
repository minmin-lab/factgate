package catalog

// DelegatedAuthorization is the demo's coarse-grained, long-lived
// authorization model (KB1-LOOP-147; a demo feature, not a paper claim). It is
// a top-level Catalog block, so the Catalog digest carried by every manifest
// and receipt binds whether it was on; a Catalog without the block (the formal
// round's) behaves exactly as before.
type DelegatedAuthorization struct {
	// IssuerRole is what a department administrator's attested attributes
	// must show to issue an authorization for an employee of their own
	// department (the issue_data_authorization tool).
	IssuerRole AttributeCondition `yaml:"issuer_role" json:"issuer_role"`
	// ChildSelfConfirm lets the human subject of a task tree confirm its child
	// tasks themselves: a child task's approver is then the requester, while a
	// root task is still approved by someone else.
	ChildSelfConfirm bool `yaml:"child_self_confirm,omitempty" json:"child_self_confirm,omitempty"`
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
	return problems
}
