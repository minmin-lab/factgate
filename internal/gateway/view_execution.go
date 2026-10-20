package gateway

import (
	"context"
	"errors"
	"strings"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// prepareTaskPlan resolves semantic roots after the exact-replay check and
// before any reservation. Ordinary plans retain the legacy pure preparation
// path. A query-time graph that contains a semantic root is deliberately not
// grafted into another graph in Phase B; the root's own compiled graph may
// still contain up to queryplan.MaxJoinSources terminal products.
func (s *Service) prepareTaskPlan(ctx context.Context, task control.Task, grant control.TaskGrant, plan queryplan.QueryPlan) (preparedQueryPlan, error) {
	if plan.From != nil {
		productNames, err := queryplan.RelationalProductNames(plan)
		if err == nil {
			for _, name := range productNames {
				product, present := s.catalog.LookupProduct(name)
				if present && product.ViewContract != nil {
					return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_GRAPH_GRAFT")
				}
			}
		}
		return s.preparePlan(grant, plan)
	}

	root, present := s.catalog.LookupProduct(plan.Product)
	if !present || root.ViewContract == nil {
		return s.preparePlan(grant, plan)
	}
	if !contains(grant.ApprovedProducts, root.Name) {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 请求了任务授权外的数据产品"}
	}
	if !semanticRelationalProfile(grant.Exposure) {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_EXPOSURE_PROFILE")
	}

	// Validate the agent-authored plan against the signed public root before
	// any terminal field is introduced. This is the non-expansion boundary:
	// compilation below cannot grant a root column the task did not approve.
	approvedRoot := stringSetFromSlice(grant.ApprovedColumns[root.Name])
	rootAggregates := stringSetFromSlice(root.AllowedAggregates)
	rootSQL, err := queryplan.Compile(plan, queryplan.Product{
		Name: root.Name, Columns: approvedRoot, AllowedAggregates: rootAggregates,
	})
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 无法在语义 View 的任务授权内编译"}
	}
	publicPolicy, err := s.policyGrant(grant)
	if err != nil {
		return preparedQueryPlan{}, err
	}
	if _, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: rootSQL, Grant: publicPolicy, RowLimit: 1,
	}); err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "QueryPlan 使用了语义 View root 未授权的运算"}
	}

	current, err := s.currentTaskViewBinding(ctx, task, grant)
	if err != nil {
		return preparedQueryPlan{}, err
	}
	artifact, present := current.Artifacts[root.Name]
	if !present {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 缺少可执行计划"}
	}
	composition, err := viewcompiler.ComposeQueryPlan(root.Name, plan, artifact)
	if err != nil {
		return preparedQueryPlan{}, viewQueryUnsupported(viewCompositionReason(err))
	}
	return s.prepareSemanticViewPlan(grant, artifact, composition, current, plan)
}

func semanticRelationalProfile(grant control.ExposureGrant) bool {
	if !grant.Enabled() {
		return false
	}
	switch grant.ProfileVersion {
	case exposure.ProfileV2, exposure.ProfileV3, exposure.ProfileV4, exposure.ProfileV5:
		return true
	default:
		return false
	}
}

func viewQueryUnsupported(reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "SEMANTIC_VIEW_QUERY_FRAGMENT"
	}
	return &mcp.ToolError{Code: apierr.CodeViewQueryUnsupported,
		Message: "查询超出 TaskGate Phase B 语义 View 受限片段",
		Details: map[string]any{"reason": reason, "retryable_after_rewrite": true}}
}

func viewCompositionReason(err error) string {
	var rejected *viewcompiler.Error
	if errors.As(err, &rejected) && rejected.Code != "" {
		return string(rejected.Code)
	}
	return "SEMANTIC_VIEW_QUERY_FRAGMENT"
}

func (s *Service) currentTaskViewBinding(ctx context.Context, task control.Task, grant control.TaskGrant) (*resolvedViewBinding, error) {
	if grant.ViewBindingDigest == "" {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "语义 View 任务缺少签名 binding"}
	}
	persisted, err := decodePersistedPending(task)
	if err != nil || persisted.ViewBinding == nil ||
		!sameSnapshotSHA256(persisted.ViewBinding.Digest, grant.ViewBindingDigest) {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 证据不一致"}
	}
	status, err := s.store.GetTaskViewBindingStatus(ctx, task.ID)
	if err != nil || status.Status != control.TaskViewBindingActive ||
		!sameSnapshotSHA256(status.BoundDigest, grant.ViewBindingDigest) {
		return nil, semanticViewChangedToolError()
	}
	products, err := boundViewProducts(persisted.ViewBinding)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的语义 View binding 无法解码"}
	}
	current, resolveErr := s.resolveViewBinding(ctx, products)
	if resolveErr != nil && !dataconnector.IsCode(resolveErr, dataconnector.CodeViewSemanticChanged) {
		return nil, resolveErr
	}
	if resolveErr == nil && viewBindingMatchesCurrent(persisted.ViewBinding, current) &&
		sameSnapshotSHA256(grant.ViewBindingDigest, current.Digest) {
		return current, nil
	}
	observed := viewSemanticObservationDigest(grant.ViewBindingDigest, resolveErr)
	if current != nil && current.Digest != "" {
		observed = current.Digest
	}
	_, _ = s.store.MarkTaskViewSemanticChanged(ctx, task.ID, observed)
	return nil, semanticViewChangedToolError()
}

func semanticViewChangedToolError() error {
	return &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged),
		Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
}

type semanticViewGovernance struct {
	products          map[string]catalog.Product
	rootScopeByColumn map[string]map[string]string
}

// semanticViewGovernanceFor proves that every terminal source remains inside
// the root's source, sensitivity, stable-role, and dynamic-scope envelope.
// Scope propagation is explicit: a public root scope output must resolve to
// one concrete terminal FieldID, and every terminal mandatory scope must be
// covered. Join equalities and constants are never treated as scope proofs.
func (s *Service) semanticViewGovernanceFor(root catalog.Product, artifact viewcompiler.Artifact) (semanticViewGovernance, error) {
	result := semanticViewGovernance{
		products:          make(map[string]catalog.Product, len(artifact.BaseProducts)),
		rootScopeByColumn: make(map[string]map[string]string, len(artifact.BaseProducts)),
	}
	scans, err := semanticArtifactScans(artifact.Plan)
	if err != nil {
		return semanticViewGovernance{}, err
	}
	if len(scans) != len(artifact.BaseProducts) {
		return semanticViewGovernance{}, errors.New("semantic View base-product closure is inconsistent")
	}
	artifactBases := make(map[string]struct{}, len(artifact.BaseProducts))
	for _, name := range artifact.BaseProducts {
		if _, duplicate := artifactBases[name]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a base-product binding")
		}
		artifactBases[name] = struct{}{}
	}
	rootSensitivity, err := root.EffectiveSensitivity()
	if err != nil {
		return semanticViewGovernance{}, err
	}
	roleProduct := make(map[string]string, len(scans))
	seenProducts := make(map[string]struct{}, len(scans))
	for _, scan := range scans {
		if _, duplicate := roleProduct[scan.Role]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a stable role")
		}
		if _, duplicate := seenProducts[scan.Product]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a terminal product")
		}
		terminal, present := s.catalog.LookupProduct(scan.Product)
		if !present || terminal.ViewContract != nil || terminal.Source != root.Source ||
			terminal.StableRelationRole != scan.Role {
			return semanticViewGovernance{}, errors.New("semantic View terminal product is not governed by the root source")
		}
		terminalSensitivity, sensitivityErr := terminal.EffectiveSensitivity()
		if sensitivityErr != nil || !terminalSensitivity.AtMost(rootSensitivity) {
			return semanticViewGovernance{}, errors.New("semantic View root sensitivity is below a terminal product")
		}
		roleProduct[scan.Role] = scan.Product
		seenProducts[scan.Product] = struct{}{}
		result.products[scan.Product] = terminal
		result.rootScopeByColumn[scan.Product] = make(map[string]string)
	}
	for name := range artifactBases {
		if _, present := seenProducts[name]; !present {
			return semanticViewGovernance{}, errors.New("semantic View artifact names an unreachable terminal product")
		}
	}
	for name := range seenProducts {
		if _, present := artifactBases[name]; !present {
			return semanticViewGovernance{}, errors.New("semantic View artifact omits a reachable terminal product")
		}
	}

	outputs := make(map[string]viewcompiler.Output, len(artifact.Outputs))
	for _, output := range artifact.Outputs {
		if _, duplicate := outputs[output.Name]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View repeats a public output")
		}
		outputs[output.Name] = output
	}
	for _, rootScope := range root.Scopes {
		output, present := outputs[rootScope]
		if !present || output.Kind != viewcompiler.OutputField {
			return semanticViewGovernance{}, errors.New("semantic View scope is not a public base-field output")
		}
		role, terminalColumn, valid := splitRelationalField(output.FieldID)
		productName, present := roleProduct[role]
		if !valid || !present {
			return semanticViewGovernance{}, errors.New("semantic View scope has no terminal field binding")
		}
		terminal := result.products[productName]
		if !contains(terminal.Scopes, terminalColumn) || !s.equivalentScopePolicies(rootScope, terminalColumn) {
			return semanticViewGovernance{}, errors.New("semantic View scope does not match a terminal mandatory scope")
		}
		if _, duplicate := result.rootScopeByColumn[productName][terminalColumn]; duplicate {
			return semanticViewGovernance{}, errors.New("semantic View maps multiple root scopes to one terminal scope")
		}
		result.rootScopeByColumn[productName][terminalColumn] = rootScope
	}
	for productName, terminal := range result.products {
		for _, terminalScope := range terminal.Scopes {
			if _, covered := result.rootScopeByColumn[productName][terminalScope]; !covered {
				return semanticViewGovernance{}, errors.New("semantic View does not expose every terminal mandatory scope")
			}
		}
	}
	return result, nil
}

func semanticArtifactScans(plan queryplan.QueryPlan) ([]queryplan.Scan, error) {
	if plan.From == nil || plan.Product != "" {
		return nil, errors.New("semantic View artifact is not relational")
	}
	switch {
	case plan.From.Scan != nil && plan.From.Join == nil && plan.From.JoinMany == nil && plan.From.UnionDistinct == nil:
		return []queryplan.Scan{*plan.From.Scan}, nil
	case plan.From.JoinMany != nil && plan.From.Scan == nil && plan.From.Join == nil && plan.From.UnionDistinct == nil:
		return append([]queryplan.Scan(nil), plan.From.JoinMany.Sources...), nil
	default:
		return nil, errors.New("semantic View artifact has an unsupported source operator")
	}
}

func (s *Service) equivalentScopePolicies(rootName, terminalName string) bool {
	root, rootOK := catalogScopeByName(s.catalog.Scopes, rootName)
	terminal, terminalOK := catalogScopeByName(s.catalog.Scopes, terminalName)
	if !rootOK || !terminalOK || root.Type != terminal.Type || root.Min != terminal.Min || root.Max != terminal.Max {
		return false
	}
	return sameStringSet(root.AllowedValues, terminal.AllowedValues)
}

func catalogScopeByName(scopes []catalog.Scope, name string) (catalog.Scope, bool) {
	for _, scope := range scopes {
		if scope.Name == name {
			return scope, true
		}
	}
	return catalog.Scope{}, false
}

// prepareSemanticViewPlan prepares one composed semantic View operation.
//
// It derives nothing. The governance envelope, the terminal column closure, the
// compiled statements, the internal terminal grant, the ordinal binding and the
// V5 predicate footprint are all produced by
// internal/physicalquery.PrepareSemanticView from immutable values, so the
// finalizer reaches the same statements from retained frozen evidence rather
// than from the Gateway's word. What stays here is resolving the binding (done
// by the caller), holding the live snapshot handles, and the registry revision
// the Gateway re-checks before executing.
//
// outer is the agent's own plan against the public View relation, and it is
// required rather than optional: preparation identifies a View operation by the
// outer plan -- that is what PlanSHA256 names -- so an operation prepared
// without one would be identified by nothing the agent submitted.
func (s *Service) prepareSemanticViewPlan(grant control.TaskGrant, artifact viewcompiler.Artifact,
	composition viewcompiler.Composition, binding *resolvedViewBinding,
	outer queryplan.QueryPlan) (preparedQueryPlan, error) {
	// The one shape check kept ahead of preparation. Preparation refuses it too,
	// but only this call site can say WHY in the vocabulary an agent can act on:
	// a composition whose projection and declared outputs disagree is a query
	// outside the Phase B fragment, which is retryable after a rewrite rather
	// than a policy denial.
	if len(composition.VisibleFields) == 0 ||
		len(composition.VisibleFields) != len(composition.Plan.Columns)+len(composition.Plan.Aggregates) {
		return preparedQueryPlan{}, viewQueryUnsupported("SEMANTIC_VIEW_OUTPUT_MAPPING")
	}
	inputs, err := s.semanticViewPreparationInputs(grant, artifact, composition, binding, outer)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "语义 View 查询无法在当前 Catalog 与快照证据下构造准备输入", Cause: err}
	}
	prepared, err := physicalquery.PrepareSemanticView(inputs)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied,
			Message: "语义 View 查询不在其治理闭包与任务授权内", Cause: err}
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "本进程无法确定查询编译器身份", Cause: err}
	}
	if err := prepared.RequireSemanticViewInputs(inputs, compiler); err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "语义 View 准备结果与其输入证据不一致；查询未执行", Cause: err}
	}
	// The observation is expressed in the COMPOSED plan, not the agent's outer
	// one: the statements project and group by composed FieldIDs, so an
	// observation built against the outer plan would be looking for columns the
	// result does not carry. The outer plan is what the binding identifies, and
	// that identity is already sealed.
	context, err := s.planExposureContextFrom(prepared, composition.Plan)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "已准备的语义 View 查询无法在当前快照索引下建立观测上下文", Cause: err}
	}
	// The raw revision, which the binding carries only as a digest. executeSQL
	// compares it against the revision the registry reports at execution time.
	context.viewRegistryRevision = binding.Expectation.ExpectedRevisionDigest
	return preparedQueryPlan{SQL: context.mainSQL, PolicyGrant: prepared.PolicyGrant(),
		Exposure: context, Prepared: prepared}, nil
}
