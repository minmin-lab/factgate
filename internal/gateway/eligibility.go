package gateway

import (
	"fmt"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/principalattr"
)

// EligibilityVerdictV1 names the private evidence record a task keeps about
// how its request was found inside the Catalog eligibility envelope.
const EligibilityVerdictV1 = "taskgate-eligibility-v1"

// eligibilityVerdict is persisted with the pending context, never in the
// signed authorization manifest (whose version is unchanged). An auditor can
// re-derive the decision from the attribute snapshot digest, the Catalog rule,
// and the evaluation instant.
type eligibilityVerdict struct {
	Version        string                 `json:"version"`
	SnapshotSHA256 string                 `json:"snapshot_sha256"`
	Subject        string                 `json:"subject"`
	EvaluatedAt    time.Time              `json:"evaluated_at"`
	Rules          []eligibilityRuleTrace `json:"rules"`
}

type eligibilityRuleTrace struct {
	Product  string                              `json:"product"`
	Require  []eligibilityConditionTrace         `json:"require,omitempty"`
	Envelope map[string]eligibilityEnvelopeTrace `json:"envelope,omitempty"`
}

type eligibilityConditionTrace struct {
	Attribute string   `json:"attribute"`
	Matched   []string `json:"matched"`
}

const (
	envelopeSourceAttribute = "attribute"
	envelopeSourceWidened   = "widened"
)

type eligibilityEnvelopeTrace struct {
	// Source is "attribute" when the admissible set is the subject's own values
	// of from_attribute at the evaluation instant, or "widened" when a widen_if
	// condition held and the Catalog allowed_values applied.
	Source     string   `json:"source"`
	Admissible []string `json:"admissible"`
	Requested  []string `json:"requested"`
}

func notEligible(message string) error {
	return &mcp.ToolError{Code: apierr.CodeNotEligible, Message: message}
}

// catalogDeclaresEligibility reports whether any Product carries an
// eligibility predicate, in which case a deployment must supply the attribute
// snapshot that the predicate is evaluated against.
func catalogDeclaresEligibility(loaded *catalog.Catalog) bool {
	if loaded == nil {
		return false
	}
	for _, product := range loaded.Products {
		if product.Eligibility != nil {
			return true
		}
	}
	return false
}

// compileEligibility evaluates every requested Product's eligibility predicate
// against the human subject's attested attributes at now. It returns nil when
// no requested Product declares eligibility. Any failure is NOT_ELIGIBLE: the
// request never reaches an approver and no task is created.
//
// The subject is the human the task is accountable to: the authenticated
// principal for a root task and the parent grant's human subject for a
// delegated task, so delegation is bounded by the delegator's attributes.
func (s *Service) compileEligibility(products []catalog.Product, subject string, scope map[string]any, now time.Time) (*eligibilityVerdict, error) {
	governed := make([]catalog.Product, 0, len(products))
	for _, product := range products {
		if product.Eligibility != nil {
			governed = append(governed, product)
			continue
		}
		// A relation of a knowledge product inherits the knowledge product's
		// eligibility (docs/kb-design.md §6): the envelope is declared once on
		// the knowledge product and applies to its chunks, documents and
		// values alike, under the same mandatory scopes.
		if owner, owned := s.knowledgeProductOwning(product.Name); owned && owner.Eligibility != nil {
			inherited := product
			inherited.Eligibility = owner.Eligibility
			governed = append(governed, inherited)
		}
	}
	if len(governed) == 0 {
		return nil, nil
	}
	sort.Slice(governed, func(i, j int) bool { return governed[i].Name < governed[j].Name })
	snapshot := s.principalAttributes
	if snapshot == nil {
		return nil, notEligible("部署未配置主体属性快照，无法编译资格包络")
	}
	if !snapshot.Known(subject) {
		return nil, notEligible("申请人不在主体属性快照中，不具备申请资格")
	}
	definitions := make(map[string]catalog.Scope, len(s.catalog.Scopes))
	for _, definition := range s.catalog.Scopes {
		definitions[definition.Name] = definition
	}
	verdict := &eligibilityVerdict{
		Version: EligibilityVerdictV1, SnapshotSHA256: snapshot.SHA256,
		Subject: subject, EvaluatedAt: now.UTC(),
	}
	for _, product := range governed {
		trace := eligibilityRuleTrace{Product: product.Name}
		for _, condition := range product.Eligibility.Require {
			matched := intersectSorted(snapshot.Values(subject, condition.Attribute, now), condition.AnyOf)
			if len(matched) == 0 {
				return nil, notEligible(fmt.Sprintf("申请人不满足数据产品 %s 的资格条件", product.Name))
			}
			trace.Require = append(trace.Require, eligibilityConditionTrace{Attribute: condition.Attribute, Matched: matched})
		}
		names := make([]string, 0, len(product.Eligibility.ScopeEnvelope))
		for name := range product.Eligibility.ScopeEnvelope {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			rule := product.Eligibility.ScopeEnvelope[name]
			requested, ok := scopeStringValues(scope[name])
			if !ok || len(requested) == 0 {
				return nil, notEligible(fmt.Sprintf("数据产品 %s 的 %s 范围缺失，无法编译资格包络", product.Name, name))
			}
			source := envelopeSourceAttribute
			admissible := snapshot.Values(subject, rule.FromAttribute, now)
			if anyConditionHolds(snapshot, subject, rule.WidenIf, now) {
				source = envelopeSourceWidened
				admissible = append([]string(nil), definitions[name].AllowedValues...)
				sort.Strings(admissible)
			}
			if !subsetOfSorted(requested, admissible) {
				return nil, notEligible(fmt.Sprintf("申请的 %s 范围超出申请人对数据产品 %s 的资格包络", name, product.Name))
			}
			if trace.Envelope == nil {
				trace.Envelope = make(map[string]eligibilityEnvelopeTrace)
			}
			trace.Envelope[name] = eligibilityEnvelopeTrace{
				Source: source, Admissible: append([]string(nil), admissible...), Requested: requested,
			}
		}
		verdict.Rules = append(verdict.Rules, trace)
	}
	return verdict, nil
}

func anyConditionHolds(snapshot *principalattr.Snapshot, subject string, conditions []catalog.AttributeCondition, now time.Time) bool {
	for _, condition := range conditions {
		if len(intersectSorted(snapshot.Values(subject, condition.Attribute, now), condition.AnyOf)) > 0 {
			return true
		}
	}
	return false
}

// scopeStringValues reads a normalized enum scope. normalizeScopes stores
// []string; persisted contexts decode as []any.
func scopeStringValues(raw any) ([]string, bool) {
	switch typed := raw.(type) {
	case []string:
		values := append([]string(nil), typed...)
		sort.Strings(values)
		return values, true
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			value, ok := item.(string)
			if !ok {
				return nil, false
			}
			values = append(values, value)
		}
		sort.Strings(values)
		return values, true
	default:
		return nil, false
	}
}

func intersectSorted(values, candidates []string) []string {
	set := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		set[candidate] = struct{}{}
	}
	var matched []string
	for _, value := range values {
		if _, ok := set[value]; ok {
			matched = append(matched, value)
		}
	}
	sort.Strings(matched)
	return matched
}

func subsetOfSorted(requested, admissible []string) bool {
	set := make(map[string]struct{}, len(admissible))
	for _, value := range admissible {
		set[value] = struct{}{}
	}
	for _, value := range requested {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

// knowledgeProductOwning returns the knowledge product whose chunks,
// documents or values relation is the named Product.
func (s *Service) knowledgeProductOwning(product string) (catalog.KnowledgeProduct, bool) {
	if s.catalog == nil {
		return catalog.KnowledgeProduct{}, false
	}
	for _, kp := range s.catalog.KnowledgeProducts {
		// Every relation inherits: the optional collection_values and files
		// relations as much as the three every knowledge product declares.
		for _, relation := range kp.Products.Relations() {
			if relation == product {
				return kp, true
			}
		}
	}
	return catalog.KnowledgeProduct{}, false
}
