package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ApprovalChain is an org-chart approval route (KB1-LOOP-141; a demo feature,
// not a paper claim). A task is approved in sequence by the approver of the
// requester's own department, then the approver of every other requested
// department, then -- for every escalation layer whose values the task
// requests (for example the confidential or leadership classification) --
// the person in charge of each requested department on that layer. The
// Catalog decides who approves; each approver's authority is verified against
// the attested principal attributes when they approve, so a misrouted OA
// approval is refused rather than granted.
type ApprovalChain struct {
	// RequesterAttribute names the attested attribute holding the
	// requester's own department (exactly one value).
	RequesterAttribute string `yaml:"requester_attribute" json:"requester_attribute"`
	// DepartmentScope is the Catalog enum scope listing requested departments.
	DepartmentScope string `yaml:"department_scope" json:"department_scope"`
	// Directors maps every value of DepartmentScope to its approver's subject
	// (the department's director, or in a delegated-authorization demo its
	// administrator).
	Directors map[string]string `yaml:"directors" json:"directors"`
	// DirectorAuthority is what a director's attested attributes must show.
	DirectorAuthority ApprovalAuthority `yaml:"director_authority" json:"director_authority"`
	// Escalation is the single escalation layer of the original form; nil
	// means none. It is kept so Catalogs and persisted routes written with it
	// still parse; a Catalog uses either it or Escalations, never both.
	Escalation *ApprovalEscalation `yaml:"escalation,omitempty" json:"escalation,omitempty"`
	// Escalations lists escalation layers in chain order (KB1-LOOP-147), each
	// with its own role name, trigger values and org chart.
	Escalations []ApprovalEscalation `yaml:"escalations,omitempty" json:"escalations,omitempty"`
}

// ApprovalEscalation adds, when the task requests any of Values of Scope,
// one step per leader in charge of a requested department.
type ApprovalEscalation struct {
	// Role names the layer's steps (a lowercase identifier, unique among the
	// layers); the single-layer form defaults to "leader".
	Role string `yaml:"role,omitempty" json:"role,omitempty"`
	// Label is how the layer is shown to people; empty shows Role.
	Label           string            `yaml:"label,omitempty" json:"label,omitempty"`
	Scope           string            `yaml:"scope" json:"scope"`
	Values          []string          `yaml:"values" json:"values"`
	Leaders         map[string]string `yaml:"leaders" json:"leaders"`
	LeaderAuthority ApprovalAuthority `yaml:"leader_authority" json:"leader_authority"`
	// Level is the layer's authority level under the delegated-authorization
	// rules (KB1-LOOP-148): 0 ranks the layer with the department approvers
	// (for example directors equal to department administrators). Nil means
	// the layer's position, index+1. It never changes the chain's order.
	Level *int `yaml:"level,omitempty" json:"level,omitempty"`
}

// ApprovalAuthority is checked against an approver's attested attributes:
// Attribute must hold every department of the step, and Role must hold.
type ApprovalAuthority struct {
	Attribute string             `yaml:"attribute" json:"attribute"`
	Role      AttributeCondition `yaml:"role" json:"role"`
}

// Approval step roles, in chain order. Escalation layers name their own
// roles; ApprovalStepLeader is the single-layer form's default.
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
	// Label is the escalation layer's label; empty for the director steps.
	Label string `json:"label,omitempty"`
}

var approverSubjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.@-]{0,127}$`)

// Layers lists the chain's escalation layers in order: Escalations, or the
// single Escalation as one layer whose role defaults to leader.
func (chain ApprovalChain) Layers() []ApprovalEscalation {
	if len(chain.Escalations) > 0 {
		layers := make([]ApprovalEscalation, 0, len(chain.Escalations))
		for _, layer := range chain.Escalations {
			if layer.Role == "" {
				layer.Role = ApprovalStepLeader
			}
			layers = append(layers, layer)
		}
		return layers
	}
	if chain.Escalation != nil {
		layer := *chain.Escalation
		if layer.Role == "" {
			layer.Role = ApprovalStepLeader
		}
		return []ApprovalEscalation{layer}
	}
	return nil
}

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
	for _, layer := range chain.Layers() {
		if !intersects(scope[layer.Scope], layer.Values) {
			continue
		}
		byLeader := map[string]int{}
		for _, department := range departments {
			leader, ok := layer.Leaders[department]
			if !ok {
				return nil, fmt.Errorf("no %s is declared for department %q", layer.Role, department)
			}
			if index, seen := byLeader[leader]; seen {
				steps[index].Departments = append(steps[index].Departments, department)
				continue
			}
			byLeader[leader] = len(steps)
			steps = append(steps, ApprovalStep{Role: layer.Role, Departments: []string{department}, Approver: leader, Label: layer.Label})
		}
	}
	return steps, nil
}

// Delegated-authorization rules (KB1-LOOP-148; the demo's
// delegated_authorization block, not a paper claim). Every step of a chain has
// a position in the org chart -- the two director roles are position 0, the
// i-th escalation layer (counting from zero) is position i+1 -- and a level:
// 0 for the director roles, the layer's Level for a layer (its position when
// unset). A requester's level is the highest level the org chart names them
// on, their position the highest position; both are -1 when it names them
// nowhere.

// LayerLevel is the level of the index-th escalation layer.
func LayerLevel(layer ApprovalEscalation, index int) int {
	if layer.Level != nil {
		return *layer.Level
	}
	return index + 1
}

// stepPosition is a step's position in the org chart; a role the chain does
// not know ranks after every layer.
func (chain ApprovalChain) stepPosition(step ApprovalStep) int {
	switch step.Role {
	case ApprovalStepRequesterDirector, ApprovalStepDepartmentDirector:
		return 0
	}
	layers := chain.Layers()
	for index, layer := range layers {
		if layer.Role == step.Role {
			return index + 1
		}
	}
	return len(layers) + 1
}

// StepLevel is a step's level; a role the chain does not know ranks above
// every layer.
func (chain ApprovalChain) StepLevel(step ApprovalStep) int {
	switch step.Role {
	case ApprovalStepRequesterDirector, ApprovalStepDepartmentDirector:
		return 0
	}
	layers := chain.Layers()
	highest := 0
	for index, layer := range layers {
		if layer.Role == step.Role {
			return LayerLevel(layer, index)
		}
		highest = max(highest, LayerLevel(layer, index))
	}
	return max(highest, len(layers)) + 1
}

// SubjectLevel is the highest level the org chart names subject on, -1 when
// it names them nowhere.
func (chain ApprovalChain) SubjectLevel(subject string) int {
	level, _ := chain.subjectRank(subject)
	return level
}

// subjectRank is the highest level and the highest position the org chart
// names subject on, -1 and -1 when it names them nowhere.
func (chain ApprovalChain) subjectRank(subject string) (level, position int) {
	level, position = -1, -1
	for _, approver := range chain.Directors {
		if strings.EqualFold(approver, subject) {
			level, position = 0, 0
			break
		}
	}
	for index, layer := range chain.Layers() {
		for _, leader := range layer.Leaders {
			if strings.EqualFold(leader, subject) {
				level, position = max(level, LayerLevel(layer, index)), max(position, index+1)
			}
		}
	}
	return level, position
}

// StepsFor derives a requester's chain under the delegated-authorization
// rules. Starting from Steps:
//
//   - a step the requester holds the authority for -- its level is at most
//     the requester's and every department of the step is in authority -- is
//     dropped: the requester stands in for it;
//   - a step that names the requester without that authority is replaced, per
//     department, by the next layer after the step, in org-chart order, that
//     names someone else;
//   - when the scope requests a department outside authority, the requester's
//     own-department step, if dropped by the first rule, is replaced instead
//     by the next layer after the requester's own position, in org-chart
//     order, that names someone else: someone above the requester signs off
//     the department's side of a cross-department request (an administrator's
//     director, a director's vice president) whatever the layers' levels;
//   - one approver appears once (their first step, departments merged when the
//     role is the same).
//
// An empty result means the requester confirms the request themselves. A step
// that cannot be replaced is an error. With an empty requesterSubject this is
// Steps.
func (chain ApprovalChain) StepsFor(requesterDepartment, requesterSubject string, authority []string,
	scope map[string][]string) ([]ApprovalStep, error) {
	steps, err := chain.Steps(requesterDepartment, scope)
	if err != nil || requesterSubject == "" {
		return steps, err
	}
	held := make(map[string]struct{}, len(authority))
	for _, department := range authority {
		held[department] = struct{}{}
	}
	within := func(departments []string) bool {
		for _, department := range departments {
			if _, ok := held[department]; !ok {
				return false
			}
		}
		return true
	}
	level, position := chain.subjectRank(requesterSubject)
	crossing := !within(scope[chain.DepartmentScope])
	out := make([]ApprovalStep, 0, len(steps))
	for index, step := range steps {
		covered := level >= chain.StepLevel(step) && within(step.Departments)
		switch {
		case covered && index == 0 && crossing:
			// Steps puts the requester's own-department step first.
			replacement, err := chain.above(position, requesterDepartment, requesterSubject)
			if err != nil {
				return nil, err
			}
			out = append(out, replacement)
		case covered:
		case strings.EqualFold(step.Approver, requesterSubject):
			for _, department := range step.Departments {
				replacement, err := chain.above(chain.stepPosition(step), department, requesterSubject)
				if err != nil {
					return nil, err
				}
				out = append(out, replacement)
			}
		default:
			out = append(out, step)
		}
	}
	return mergeApprovers(out), nil
}

// above is the step of the first escalation layer after position, in
// org-chart order, whose leader for department is not subject.
func (chain ApprovalChain) above(position int, department, subject string) (ApprovalStep, error) {
	for index, layer := range chain.Layers() {
		if index+1 <= position {
			continue
		}
		leader, ok := layer.Leaders[department]
		if !ok {
			return ApprovalStep{}, fmt.Errorf("no %s is declared for department %q", layer.Role, department)
		}
		if strings.EqualFold(leader, subject) {
			continue
		}
		return ApprovalStep{Role: layer.Role, Departments: []string{department}, Approver: leader, Label: layer.Label}, nil
	}
	return ApprovalStep{}, fmt.Errorf("no one above %s is declared to approve department %q", subject, department)
}

// mergeApprovers keeps each approver's first step; a later step of the same
// approver and role adds its departments, any other later step of theirs is
// dropped (an approver approves a task once).
func mergeApprovers(steps []ApprovalStep) []ApprovalStep {
	out := make([]ApprovalStep, 0, len(steps))
	first := map[string]int{}
	for _, step := range steps {
		key := strings.ToLower(step.Approver)
		index, seen := first[key]
		if !seen {
			first[key] = len(out)
			step.Departments = append([]string(nil), step.Departments...)
			out = append(out, step)
			continue
		}
		if out[index].Role != step.Role {
			continue
		}
		for _, department := range step.Departments {
			if !containsString(out[index].Departments, department) {
				out[index].Departments = append(out[index].Departments, department)
			}
		}
		sort.Strings(out[index].Departments)
	}
	return out
}

// CenterCovers reports whether a center head confirms a request themselves:
// it requests only their own department, only centers they head (scope
// centerScope, non-empty), and triggers no escalation layer.
func (chain ApprovalChain) CenterCovers(requesterDepartment string, scope map[string][]string, centerScope string, centers []string) bool {
	departments := scope[chain.DepartmentScope]
	requested := scope[centerScope]
	if centerScope == "" || len(departments) == 0 || len(requested) == 0 {
		return false
	}
	for _, department := range departments {
		if department != requesterDepartment {
			return false
		}
	}
	for _, center := range requested {
		if !containsString(centers, center) {
			return false
		}
	}
	for _, layer := range chain.Layers() {
		if intersects(scope[layer.Scope], layer.Values) {
			return false
		}
	}
	return true
}

// StewardScope is the scope a chain is derived from when every requested
// product has a steward department: each requested department that is
// neither in the requester's authority nor the requester's own department is
// replaced by the steward departments. The requester's own department keeps
// its own approver (its step is first in every chain anyway).
func (chain ApprovalChain) StewardScope(requesterDepartment string, authority, stewards []string,
	scope map[string][]string) map[string][]string {
	if len(stewards) == 0 {
		return scope
	}
	out := make(map[string][]string, len(scope))
	for name, values := range scope {
		out[name] = values
	}
	var departments []string
	add := func(department string) {
		if !containsString(departments, department) {
			departments = append(departments, department)
		}
	}
	for _, department := range scope[chain.DepartmentScope] {
		if department == requesterDepartment || containsString(authority, department) {
			add(department)
			continue
		}
		for _, steward := range stewards {
			add(steward)
		}
	}
	sort.Strings(departments)
	out[chain.DepartmentScope] = departments
	return out
}

// Authority returns what a step's approver must show.
func (chain ApprovalChain) Authority(step ApprovalStep) (ApprovalAuthority, bool) {
	switch step.Role {
	case ApprovalStepRequesterDirector, ApprovalStepDepartmentDirector:
		return chain.DirectorAuthority, true
	}
	for _, layer := range chain.Layers() {
		if layer.Role == step.Role {
			return layer.LeaderAuthority, true
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
		escalation := cloneApprovalEscalation(*chain.Escalation)
		out.Escalation = &escalation
	}
	if chain.Escalations != nil {
		out.Escalations = make([]ApprovalEscalation, 0, len(chain.Escalations))
		for _, layer := range chain.Escalations {
			out.Escalations = append(out.Escalations, cloneApprovalEscalation(layer))
		}
	}
	return &out
}

func cloneApprovalEscalation(source ApprovalEscalation) ApprovalEscalation {
	escalation := source
	if source.Level != nil {
		level := *source.Level
		escalation.Level = &level
	}
	escalation.Values = append([]string(nil), source.Values...)
	escalation.Leaders = cloneStringMap(source.Leaders)
	escalation.LeaderAuthority.Role.AnyOf = append([]string(nil), source.LeaderAuthority.Role.AnyOf...)
	return escalation
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

// validateApprovalChain requires a complete org chart: a director (and, on
// every escalation layer, a leader) for every value of the department scope,
// so no request can reach a department nobody is declared to approve.
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
	if chain.Escalation != nil && len(chain.Escalations) > 0 {
		problems = append(problems, fieldError(path+".escalations", "use either escalation or escalations, not both", ErrInvalidApprovalRoute))
	}
	if chain.Escalation != nil {
		problems = append(problems, validateApprovalEscalation(path+".escalation", *chain.Escalation, scopes, departments.AllowedValues)...)
		problems = append(problems, validateLayerLevel(path+".escalation", *chain.Escalation, 1)...)
	}
	seen := map[string]struct{}{}
	for index, layer := range chain.Escalations {
		layerPath := fmt.Sprintf("%s.escalations[%d]", path, index)
		switch {
		case !identifierPattern.MatchString(layer.Role):
			problems = append(problems, fieldError(layerPath+".role", "escalation layer role must be a lowercase identifier", ErrInvalidApprovalRoute))
		case layer.Role == ApprovalStepRequesterDirector || layer.Role == ApprovalStepDepartmentDirector:
			problems = append(problems, fieldError(layerPath+".role", "escalation layer role must not be a director role", ErrInvalidApprovalRoute))
		default:
			if _, duplicate := seen[layer.Role]; duplicate {
				problems = append(problems, fieldError(layerPath+".role", fmt.Sprintf("escalation layer role %q is declared twice", layer.Role), ErrInvalidApprovalRoute))
			}
			seen[layer.Role] = struct{}{}
		}
		problems = append(problems, validateApprovalEscalation(layerPath, layer, scopes, departments.AllowedValues)...)
		problems = append(problems, validateLayerLevel(layerPath, layer, len(chain.Escalations))...)
	}
	return problems
}

// validateLayerLevel bounds an explicit layer level to 0..layers.
func validateLayerLevel(path string, layer ApprovalEscalation, layers int) ValidationErrors {
	if layer.Level != nil && (*layer.Level < 0 || *layer.Level > layers) {
		return ValidationErrors{fieldError(path+".level", fmt.Sprintf("level must be between 0 and %d", layers), ErrInvalidApprovalRoute)}
	}
	return nil
}

func validateApprovalEscalation(path string, escalation ApprovalEscalation, scopes map[string]Scope, departments []string) ValidationErrors {
	var problems ValidationErrors
	scope, ok := scopes[escalation.Scope]
	if !ok || scope.Type != ScopeTypeEnum {
		problems = append(problems, fieldError(path+".scope", "escalation scope must name a Catalog enum scope", ErrInvalidApprovalRoute))
	} else {
		problems = append(problems, validateScopeCondition(path, map[string][]string{escalation.Scope: escalation.Values}, scopes)...)
	}
	problems = append(problems, validateOrgChart(path+".leaders", escalation.Leaders, departments)...)
	problems = append(problems, validateApprovalAuthority(path+".leader_authority", escalation.LeaderAuthority)...)
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
