package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ApprovalChain is an org-chart approval route (KB1-LOOP-141; a demo feature,
// not a paper claim). A task is approved in sequence by the director of the
// requester's own department, then the director of every other requested
// department, then -- when the task requests an escalation value such as the
// leadership classification -- the leader in charge of each requested
// department. The Catalog decides who approves; each approver's authority is
// verified against the attested principal attributes when they approve, so a
// misrouted OA approval is refused rather than granted.
type ApprovalChain struct {
	// RequesterAttribute names the attested attribute holding the
	// requester's own department (exactly one value).
	RequesterAttribute string `yaml:"requester_attribute" json:"requester_attribute"`
	// DepartmentScope is the Catalog enum scope listing requested departments.
	DepartmentScope string `yaml:"department_scope" json:"department_scope"`
	// Directors maps every value of DepartmentScope to its director's subject.
	Directors map[string]string `yaml:"directors" json:"directors"`
	// DirectorAuthority is what a director's attested attributes must show.
	DirectorAuthority ApprovalAuthority `yaml:"director_authority" json:"director_authority"`
	// Escalation adds leader steps; nil means directors only.
	Escalation *ApprovalEscalation `yaml:"escalation,omitempty" json:"escalation,omitempty"`
}

// ApprovalEscalation adds, when the task requests any of Values of Scope,
// one step per leader in charge of a requested department.
type ApprovalEscalation struct {
	Scope           string            `yaml:"scope" json:"scope"`
	Values          []string          `yaml:"values" json:"values"`
	Leaders         map[string]string `yaml:"leaders" json:"leaders"`
	LeaderAuthority ApprovalAuthority `yaml:"leader_authority" json:"leader_authority"`
}

// ApprovalAuthority is checked against an approver's attested attributes:
// Attribute must hold every department of the step, and Role must hold.
type ApprovalAuthority struct {
	Attribute string             `yaml:"attribute" json:"attribute"`
	Role      AttributeCondition `yaml:"role" json:"role"`
}

// Approval step roles, in chain order.
const (
	ApprovalStepRequesterDirector  = "requester_director"
	ApprovalStepDepartmentDirector = "department_director"
	ApprovalStepLeader             = "leader"
)

// ApprovalStep is one approval in a chain.
type ApprovalStep struct {
	Role        string   `json:"role"`
	Departments []string `json:"departments"`
	Approver    string   `json:"approver"`
}

var approverSubjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.@-]{0,127}$`)

// Steps derives the chain for a requester's department and a task's scope.
// It is a function of the Catalog and the scope alone, so a narrowed scope
// yields the chain that scope still needs.
func (chain ApprovalChain) Steps(requesterDepartment string, scope map[string][]string) ([]ApprovalStep, error) {
	departments := append([]string(nil), scope[chain.DepartmentScope]...)
	sort.Strings(departments)
	if len(departments) == 0 {
		return nil, fmt.Errorf("the task requests no %s", chain.DepartmentScope)
	}
	own, ok := chain.Directors[requesterDepartment]
	if !ok {
		return nil, fmt.Errorf("no director is declared for the requester's department %q", requesterDepartment)
	}
	steps := []ApprovalStep{{Role: ApprovalStepRequesterDirector, Departments: []string{requesterDepartment}, Approver: own}}
	for _, department := range departments {
		if department == requesterDepartment {
			continue
		}
		director, ok := chain.Directors[department]
		if !ok {
			return nil, fmt.Errorf("no director is declared for department %q", department)
		}
		steps = append(steps, ApprovalStep{Role: ApprovalStepDepartmentDirector, Departments: []string{department}, Approver: director})
	}
	if escalation := chain.Escalation; escalation != nil && intersects(scope[escalation.Scope], escalation.Values) {
		byLeader := map[string]int{}
		for _, department := range departments {
			leader, ok := escalation.Leaders[department]
			if !ok {
				return nil, fmt.Errorf("no leader is declared for department %q", department)
			}
			if index, seen := byLeader[leader]; seen {
				steps[index].Departments = append(steps[index].Departments, department)
				continue
			}
			byLeader[leader] = len(steps)
			steps = append(steps, ApprovalStep{Role: ApprovalStepLeader, Departments: []string{department}, Approver: leader})
		}
	}
	return steps, nil
}

// Authority returns what a step's approver must show.
func (chain ApprovalChain) Authority(step ApprovalStep) (ApprovalAuthority, bool) {
	switch step.Role {
	case ApprovalStepRequesterDirector, ApprovalStepDepartmentDirector:
		return chain.DirectorAuthority, true
	case ApprovalStepLeader:
		if chain.Escalation != nil {
			return chain.Escalation.LeaderAuthority, true
		}
	}
	return ApprovalAuthority{}, false
}

func intersects(values, wanted []string) bool {
	for _, value := range values {
		for _, target := range wanted {
			if value == target {
				return true
			}
		}
	}
	return false
}

func cloneApprovalChain(chain *ApprovalChain) *ApprovalChain {
	if chain == nil {
		return nil
	}
	out := *chain
	out.Directors = cloneStringMap(chain.Directors)
	out.DirectorAuthority.Role.AnyOf = append([]string(nil), chain.DirectorAuthority.Role.AnyOf...)
	if chain.Escalation != nil {
		escalation := *chain.Escalation
		escalation.Values = append([]string(nil), chain.Escalation.Values...)
		escalation.Leaders = cloneStringMap(chain.Escalation.Leaders)
		escalation.LeaderAuthority.Role.AnyOf = append([]string(nil), chain.Escalation.LeaderAuthority.Role.AnyOf...)
		out.Escalation = &escalation
	}
	return &out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// validateApprovalChain requires a complete org chart: a director (and, with
// an escalation, a leader) for every value of the department scope, so no
// request can reach a department nobody is declared to approve.
func validateApprovalChain(path string, chain ApprovalChain, scopes map[string]Scope) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(chain.RequesterAttribute) {
		problems = append(problems, fieldError(path+".requester_attribute", "requester attribute must be a lowercase identifier", ErrInvalidApprovalRoute))
	}
	departments, ok := scopes[chain.DepartmentScope]
	if !ok || departments.Type != ScopeTypeEnum {
		return append(problems, fieldError(path+".department_scope", "department scope must name a Catalog enum scope", ErrInvalidApprovalRoute))
	}
	problems = append(problems, validateOrgChart(path+".directors", chain.Directors, departments.AllowedValues)...)
	problems = append(problems, validateApprovalAuthority(path+".director_authority", chain.DirectorAuthority)...)
	if escalation := chain.Escalation; escalation != nil {
		scope, ok := scopes[escalation.Scope]
		if !ok || scope.Type != ScopeTypeEnum {
			problems = append(problems, fieldError(path+".escalation.scope", "escalation scope must name a Catalog enum scope", ErrInvalidApprovalRoute))
		} else {
			problems = append(problems, validateScopeCondition(path+".escalation", map[string][]string{escalation.Scope: escalation.Values}, scopes)...)
		}
		problems = append(problems, validateOrgChart(path+".escalation.leaders", escalation.Leaders, departments.AllowedValues)...)
		problems = append(problems, validateApprovalAuthority(path+".escalation.leader_authority", escalation.LeaderAuthority)...)
	}
	return problems
}

func validateOrgChart(path string, chart map[string]string, departments []string) ValidationErrors {
	var problems ValidationErrors
	allowed := make(map[string]struct{}, len(departments))
	for _, department := range departments {
		allowed[department] = struct{}{}
		if _, ok := chart[department]; !ok {
			problems = append(problems, fieldError(path, fmt.Sprintf("department %q has no approver", department), ErrInvalidApprovalRoute))
		}
	}
	for department, subject := range chart {
		if _, ok := allowed[department]; !ok {
			problems = append(problems, fieldError(path, fmt.Sprintf("%q is not a value of the department scope", department), ErrInvalidApprovalRoute))
		}
		if !approverSubjectPattern.MatchString(subject) {
			problems = append(problems, fieldError(path+"."+department, "approver must be a lowercase principal subject", ErrInvalidApprovalRoute))
		}
	}
	return problems
}

func validateApprovalAuthority(path string, authority ApprovalAuthority) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(authority.Attribute) {
		problems = append(problems, fieldError(path+".attribute", "authority attribute must be a lowercase identifier", ErrInvalidApprovalRoute))
	}
	if !identifierPattern.MatchString(authority.Role.Attribute) || len(authority.Role.AnyOf) == 0 {
		problems = append(problems, fieldError(path+".role", "authority role needs an attribute and at least one value", ErrInvalidApprovalRoute))
	}
	for _, value := range authority.Role.AnyOf {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, fieldError(path+".role", "authority role values must be non-empty", ErrInvalidApprovalRoute))
		}
	}
	return problems
}
