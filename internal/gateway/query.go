package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/semanticcache"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

type storedQueryResult struct {
	Columns            []dataconnector.Column      `json:"columns"`
	Rows               [][]any                     `json:"rows"`
	RowCount           int64                       `json:"row_count"`
	DatabaseMS         int64                       `json:"database_ms"`
	ComponentMS        map[string]float64          `json:"component_ms,omitempty"`
	PipelineMS         map[string]float64          `json:"pipeline_ms,omitempty"`
	DiagnosticMS       map[string]float64          `json:"diagnostic_ms,omitempty"`
	Limited            bool                        `json:"limited"`
	QueryPlan          *queryplan.QueryPlan        `json:"query_plan,omitempty"`
	SQLProfile         string                      `json:"sql_profile,omitempty"`
	PlanDigest         string                      `json:"plan_digest,omitempty"`
	OutputFormat       string                      `json:"output_format,omitempty"`
	DisplayColumns     []string                    `json:"display_columns,omitempty"`
	ResultOrder        []int                       `json:"result_order,omitempty"`
	SemanticColumns    []string                    `json:"semantic_columns,omitempty"`
	PredicateFootprint *predicateFootprintResponse `json:"predicate_footprint,omitempty"`
}

type queryPipelineMeasurement struct {
	requestStarted  time.Time
	prepareFinished time.Time
	executeFinished time.Time
}

type predicateFootprintResponse struct {
	RawLiteralCount int `json:"raw_literal_count"`
	UniqueAtomCount int `json:"unique_atom_count"`
}

// queryResponseMetadata is request-syntax metadata, not exposure identity.
// Canonical connector columns remain encrypted in storage so accounting and
// semantic replay operate on stable field IDs; display labels are applied only
// when a result is released to the caller.
type queryResponseMetadata struct {
	Plan               queryplan.QueryPlan
	SQLProfile         string
	PlanDigest         string
	OutputFormat       string
	DisplayColumns     []string
	ResultOrder        []int
	SemanticColumns    []string
	PredicateFootprint *predicateFootprintResponse
}

func applyQueryResponseMetadata(stored *storedQueryResult, metadata *queryResponseMetadata) error {
	stored.QueryPlan = nil
	stored.SQLProfile = ""
	stored.PlanDigest = ""
	stored.OutputFormat = ""
	stored.DisplayColumns = nil
	stored.ResultOrder = nil
	stored.SemanticColumns = nil
	stored.PredicateFootprint = nil
	if metadata == nil {
		return nil
	}
	if len(metadata.DisplayColumns) > 0 && len(metadata.DisplayColumns) != len(stored.Columns) {
		return fmt.Errorf("display-column metadata disagrees with canonical result: display=%v stored=%v",
			metadata.DisplayColumns, stored.Columns)
	}
	if err := validateResultOrder(metadata.ResultOrder, len(stored.Columns)); err != nil {
		return err
	}
	if err := validateSemanticColumns(metadata.SemanticColumns, len(stored.Columns)); err != nil {
		return err
	}
	plan := cloneQueryPlan(metadata.Plan)
	stored.QueryPlan = &plan
	stored.SQLProfile = metadata.SQLProfile
	stored.PlanDigest = metadata.PlanDigest
	stored.OutputFormat = metadata.OutputFormat
	stored.DisplayColumns = append([]string(nil), metadata.DisplayColumns...)
	stored.ResultOrder = append([]int(nil), metadata.ResultOrder...)
	stored.SemanticColumns = append([]string(nil), metadata.SemanticColumns...)
	if metadata.PredicateFootprint != nil {
		value := *metadata.PredicateFootprint
		stored.PredicateFootprint = &value
	}
	return nil
}

func validateSemanticColumns(columns []string, width int) error {
	if len(columns) == 0 {
		return nil
	}
	if len(columns) != width {
		return errors.New("semantic-column metadata disagrees with canonical result")
	}
	seen := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		if column == "" {
			return errors.New("semantic-column metadata contains an empty identity")
		}
		if _, duplicate := seen[column]; duplicate {
			return errors.New("semantic-column metadata repeats an identity")
		}
		seen[column] = struct{}{}
	}
	return nil
}

func alignStoredSemanticColumns(stored *storedQueryResult, desired []string) error {
	if len(desired) == 0 {
		return nil
	}
	if err := validateSemanticColumns(desired, len(stored.Columns)); err != nil {
		return err
	}
	if err := validateSemanticColumns(stored.SemanticColumns, len(stored.Columns)); err != nil || len(stored.SemanticColumns) == 0 {
		return errors.New("stored semantic-column metadata is unavailable")
	}
	positions := make(map[string]int, len(stored.SemanticColumns))
	for index, column := range stored.SemanticColumns {
		positions[column] = index
	}
	order := make([]int, len(desired))
	for index, column := range desired {
		position, present := positions[column]
		if !present {
			return errors.New("stored result has a different semantic projection")
		}
		order[index] = position
	}
	columns := make([]dataconnector.Column, len(order))
	for index, position := range order {
		columns[index] = stored.Columns[position]
	}
	rows := make([][]any, len(stored.Rows))
	for rowIndex, row := range stored.Rows {
		if len(row) < len(stored.Columns) {
			return errors.New("stored result row is shorter than its metadata")
		}
		rows[rowIndex] = make([]any, len(order))
		for index, position := range order {
			rows[rowIndex][index] = row[position]
		}
	}
	stored.Columns = columns
	stored.Rows = rows
	stored.SemanticColumns = append([]string(nil), desired...)
	return nil
}

func queryPlanSemanticColumns(plan queryplan.QueryPlan) []string {
	columns := make([]string, 0, len(plan.Columns)+len(plan.Derived)+len(plan.Aggregates))
	for _, column := range plan.Columns {
		columns = append(columns, "column:"+column)
	}
	// Derived arithmetic projections sit between plain columns and
	// aggregates, matching the SQL emission and visible-field order.
	for _, derived := range plan.Derived {
		expression, err := queryplan.NormalizeArithmetic(derived.Expr)
		if err != nil {
			expression = derived.Alias
		}
		columns = append(columns, "derived:"+expression)
	}
	occurrences := make(map[string]int, len(plan.Aggregates))
	for _, aggregate := range plan.Aggregates {
		expression := aggregateExpressionID(aggregate)
		if aggregate.ResultEncoding != "" {
			expression += "@" + aggregate.ResultEncoding
		}
		occurrence := occurrences[expression]
		occurrences[expression] = occurrence + 1
		columns = append(columns, "aggregate:"+expression+"#"+strconv.Itoa(occurrence))
	}
	// HAVING identity deliberately does not appear here. Semantic columns are
	// a positionally aligned per-result-column identity list (the width check
	// in validateSemanticColumns and the ResultOrder reordering both assume
	// it), and appending HAVING entries made every HAVING query fail closed
	// with a width mismatch (pilot-benign-01). The HAVING predicate is part
	// of the plan's typed normal form and plan digest, which the semantic
	// replay key already binds.
	return columns
}

func queryPlanResultNames(plan queryplan.QueryPlan) []string {
	columns := append([]string(nil), plan.Columns...)
	for _, aggregate := range plan.Aggregates {
		columns = append(columns, aggregate.Alias)
	}
	return columns
}

func identityResultOrder(width int) []int {
	order := make([]int, width)
	for index := range order {
		order[index] = index
	}
	return order
}

func validateResultOrder(order []int, width int) error {
	if len(order) == 0 {
		return nil
	}
	if len(order) != width {
		return errors.New("result-order metadata disagrees with canonical result")
	}
	seen := make([]bool, width)
	for _, position := range order {
		if position < 0 || position >= width || seen[position] {
			return errors.New("result-order metadata is not a permutation")
		}
		seen[position] = true
	}
	return nil
}

func publicStoredResult(stored storedQueryResult) ([]dataconnector.Column, [][]any, error) {
	if len(stored.DisplayColumns) > 0 && len(stored.DisplayColumns) != len(stored.Columns) {
		return nil, nil, errors.New("stored display-column metadata is invalid")
	}
	if err := validateResultOrder(stored.ResultOrder, len(stored.Columns)); err != nil {
		return nil, nil, err
	}
	order := stored.ResultOrder
	if len(order) == 0 {
		order = make([]int, len(stored.Columns))
		for index := range order {
			order[index] = index
		}
	}
	columns := make([]dataconnector.Column, len(order))
	for publicIndex, canonicalIndex := range order {
		columns[publicIndex] = stored.Columns[canonicalIndex]
		if len(stored.DisplayColumns) > 0 {
			columns[publicIndex].Name = stored.DisplayColumns[publicIndex]
		}
	}
	rows := make([][]any, len(stored.Rows))
	for rowIndex, row := range stored.Rows {
		if len(row) < len(stored.Columns) {
			return nil, nil, errors.New("stored result row is shorter than its metadata")
		}
		rows[rowIndex] = make([]any, len(order))
		for publicIndex, canonicalIndex := range order {
			rows[rowIndex][publicIndex] = row[canonicalIndex]
		}
	}
	return columns, rows, nil
}

func addStoredResponseMetadata(result map[string]any, stored storedQueryResult) error {
	columns, rows, err := publicStoredResult(stored)
	if err != nil {
		return err
	}
	result["columns"] = columns
	result["rows"] = rows
	if stored.QueryPlan != nil {
		result["query_plan"] = cloneQueryPlan(*stored.QueryPlan)
	}
	if stored.SQLProfile != "" {
		result["sql_profile"] = stored.SQLProfile
	}
	if stored.PlanDigest != "" {
		result["plan_digest"] = stored.PlanDigest
	}
	if stored.OutputFormat != "" {
		result["output_format"] = stored.OutputFormat
	}
	if stored.PredicateFootprint != nil {
		result["predicate_footprint"] = *stored.PredicateFootprint
	}
	return nil
}

// decodeStoredQueryResult preserves the exact JSON number lexeme written by
// the connector (including NUMERIC scale and integers above 2^53). Decoding
// interface-valued rows through float64 would silently change a committed
// result before idempotent or semantic replay. Like json.Unmarshal, this
// helper accepts trailing whitespace but rejects every other trailing byte.
func decodeStoredQueryResult(encoded []byte) (storedQueryResult, error) {
	var result storedQueryResult
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return storedQueryResult{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return storedQueryResult{}, errors.New("stored query result contains trailing JSON")
		}
		return storedQueryResult{}, fmt.Errorf("stored query result has invalid trailing JSON: %w", err)
	}
	return result, nil
}

// ordinalSemanticAuthorizationDigest binds the canonical authorized SQL, not
// the per-request resource ceilings injected into its executable form. Those
// ceilings shrink as a task consumes its row budget and therefore are not part
// of semantic identity. A replay remains fail-closed because the committed
// row count is checked against the fresh reservation's AllowedRows before the
// result can be finalized or released.
func ordinalSemanticAuthorizationDigest(visible, provenance sqlpolicy.Decision, manifestDigest string) string {
	return digest(strings.Join([]string{
		visible.Fingerprint,
		provenance.Fingerprint,
		manifestDigest,
	}, "\x00"))
}

// ordinalSemanticAuthorizationDigestV5 binds authorization to the canonical
// V5 query and predicate identities. Policy fingerprints retain harmless SQL
// syntax such as IN-member order and duplicates, so using them here would
// partition semantic replay for queries that NormalizeV4 proves equivalent.
// The enclosing replay binding separately pins the grant, Catalog, schema,
// dictionary set, compiler, ordering, pagination, and result encoding.
func ordinalSemanticAuthorizationDigestV5(planDigest, manifestDigest string,
	footprint *queryplan.PredicateFootprint) string {
	parts := []string{queryplan.NormalFormVersionV4, planDigest, manifestDigest}
	if footprint != nil {
		parts = append(parts,
			footprint.Version,
			footprint.ContextSHA256,
			footprint.AtomSetSHA256,
			strconv.Itoa(footprint.UniqueAtomCount),
		)
	}
	return digest(strings.Join(parts, "\x00"))
}

func ordinalSemanticReplayBinding(taskID, grantDigest, authorizationDigest, planDigest, catalogDigest,
	schemaDigest, dictionarySetDigest string) semanticcache.Binding {
	return semanticcache.Binding{
		TaskID: taskID, GrantDigest: grantDigest, AuthorizationDigest: authorizationDigest,
		TypedNormalForm: queryplan.NormalFormVersion + ":" + planDigest,
		PlanDigest:      planDigest, CatalogDigest: catalogDigest,
		SchemaDigest: schemaDigest, DictionarySetDigest: dictionarySetDigest,
		ExposureProfile: exposure.ProfileV4, CompilerVersion: queryplan.OrdinalProgramVersion,
		OrderingVersion: semanticcache.OrderingV1, PaginationVersion: semanticcache.PaginationV1,
		ResultEncoding: "stored-query-result-v1",
	}
}

func ordinalSemanticReplayBindingV5(taskID, grantDigest, authorizationDigest, planDigest, catalogDigest,
	schemaDigest, dictionarySetDigest string, footprint *queryplan.PredicateFootprint) semanticcache.Binding {
	binding := semanticcache.Binding{TaskID: taskID, GrantDigest: grantDigest, AuthorizationDigest: authorizationDigest,
		TypedNormalForm: queryplan.NormalFormVersionV4 + ":" + planDigest, PlanDigest: planDigest,
		CatalogDigest: catalogDigest, SchemaDigest: schemaDigest, DictionarySetDigest: dictionarySetDigest,
		ExposureProfile: exposure.ProfileV5, CompilerVersion: queryplan.OrdinalProgramVersion,
		OrderingVersion: semanticcache.OrderingV1, PaginationVersion: semanticcache.PaginationV1,
		ResultEncoding: "stored-query-result-v1"}
	if footprint != nil {
		binding.PredicateProfile = footprint.Version
		binding.PredicateContext = footprint.ContextSHA256
		binding.PredicateSet = footprint.AtomSetSHA256
		binding.PredicateAtomCount = int64(footprint.UniqueAtomCount)
	}
	return binding
}

const (
	resultEncodingFailed     = "RESULT_ENCODING_FAILED"
	resultFinalizationFailed = "RESULT_FINALIZATION_FAILED"
	defaultSettlementTimeout = 5 * time.Second
	settlementRetryDelay     = 100 * time.Millisecond
)

func (s *Service) querySQL(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	requestStarted := time.Now()
	var args struct {
		TaskID    string `json:"task_id"`
		RequestID string `json:"request_id"`
		SQL       string `json:"sql"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.SQL) == "" || len(args.SQL) > 100000 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "sql 必须为 1 到 100000 个字符"}
	}
	if err := validateRequestID(args.RequestID); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	requestSummary := "query_sql\x00" + task.ID + "\x00" + args.SQL
	// Preserve the original request-id contract before parsing or lowering. An
	// exact retry observes its first durable outcome even if the task, Catalog,
	// or SQL profile has changed since that execution.
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, args.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		return s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
	}
	if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	if task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务目录版本与当前实例不一致；为避免扩权已拒绝查询"}
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if err := requireOrdinaryTask(grant); err != nil {
		return nil, err
	}
	// A task with no exposure grant used to skip everything below and hand the
	// agent's own SQL straight to the authorizer. That was the last path in the
	// Gateway that executed a statement nothing had prepared, and it is why a
	// plain query could complete while describing no execution: there was no
	// canonical plan, no sealed preparation and therefore nothing a finalizer
	// could reproduce from frozen inputs.
	//
	// It now lowers and prepares like every other path. The narrowing is real and
	// deliberate: SQL outside taskgate-reporting-sql-v1 no longer executes on a
	// plain task, because a statement that cannot be lowered to a canonical plan
	// cannot carry the execution evidence every completed query is now required
	// to sign. The exposure paths have been held to this profile all along; what
	// ends here is the exemption, not a capability the plain path was using for a
	// reason.
	//
	// All that remains of the branch is the semantic View refusal, which has to be
	// made here because a View task without an exposure profile is refused for
	// that reason rather than for anything lowering would find.
	if !grant.Exposure.Enabled() {
		for _, name := range grant.ApprovedProducts {
			if product, present := s.catalog.LookupProduct(name); present && product.ViewContract != nil {
				return s.replayPreparationFailureAt(ctx, task.ID, args.RequestID, digest(requestSummary),
					requestStarted,
					viewQueryUnsupported("SEMANTIC_VIEW_EXPOSURE_PROFILE"))
			}
		}
	}

	products := make(map[string]queryplan.Product, len(grant.ApprovedProducts))
	for _, name := range grant.ApprovedProducts {
		product, found := s.catalog.LookupProduct(name)
		if !found {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务授权引用的数据产品不在当前 Catalog 中"}
		}
		products[name] = relationalQueryProduct(product, stringSetFromSlice(grant.ApprovedColumns[name]))
	}
	lowered, err := sqllowering.Lower(args.SQL, products)
	if err != nil {
		return nil, sqlLoweringToolError(err)
	}
	if lowered.Plan.From != nil && grant.Exposure.ProfileVersion != exposure.ProfileV2 &&
		grant.Exposure.ProfileVersion != exposure.ProfileV3 && grant.Exposure.ProfileVersion != exposure.ProfileV4 &&
		grant.Exposure.ProfileVersion != exposure.ProfileV5 {
		return nil, &mcp.ToolError{Code: apierr.CodeSQLNotLowerable, Message: "当前 exposure profile 不支持在线多产品计划", Details: map[string]any{
			"reason": "RELATIONAL_EXPOSURE_PROFILE_UNSUPPORTED", "location": map[string]any{"clause": "FROM"},
			"supported_alternative":   "Query an approved prejoined reporting product or use a task with taskgate-exposure-v2, v3, v4, or v5.",
			"retryable_after_rewrite": true, "sql_profile": sqllowering.Profile,
		}}
	}
	prepared, err := s.prepareTaskPlan(ctx, task, grant, lowered.Plan)
	if err != nil {
		return s.replayPreparationFailureAt(ctx, task.ID, args.RequestID, digest(requestSummary), requestStarted, err)
	}
	metadata := &queryResponseMetadata{Plan: lowered.Plan, SQLProfile: lowered.Profile,
		DisplayColumns: append([]string(nil), lowered.DisplayColumns...), ResultOrder: append([]int(nil), lowered.ResultOrder...),
		SemanticColumns: queryPlanSemanticColumns(lowered.Plan)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	return s.executeSQL(ctx, principal, task, args.RequestID, prepared, requestSummary, metadata, requestStarted)
}

func sqlLoweringToolError(err error) error {
	var rejected *sqllowering.Error
	if !errors.As(err, &rejected) {
		return &mcp.ToolError{Code: apierr.CodeSQLNotLowerable, Message: "SQL 无法无损转换为 TaskGate 规范计划", Details: map[string]any{
			"reason": "LOWERING_FAILED", "retryable_after_rewrite": true, "sql_profile": sqllowering.Profile,
		}}
	}
	details := map[string]any{
		"reason":                  rejected.Reason,
		"retryable_after_rewrite": rejected.Retryable,
		"sql_profile":             sqllowering.Profile,
	}
	location := map[string]any{}
	if rejected.Location.Clause != "" {
		location["clause"] = rejected.Location.Clause
	}
	if rejected.Location.Relation != "" {
		location["relation"] = rejected.Location.Relation
	}
	if rejected.Location.Offset >= 0 {
		location["offset"] = rejected.Location.Offset
	}
	if len(location) > 0 {
		details["location"] = location
	}
	if rejected.Alternative != "" {
		details["supported_alternative"] = rejected.Alternative
	}
	code := rejected.Code
	if code == "" {
		code = apierr.CodeSQLNotLowerable
	}
	message := rejected.Message
	if message == "" {
		message = "SQL 无法无损转换为 TaskGate 规范计划"
	}
	return &mcp.ToolError{Code: code, Message: message, Details: details}
}

func (s *Service) executePlan(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	requestStarted := time.Now()
	var args struct {
		TaskID       string              `json:"task_id"`
		RequestID    string              `json:"request_id"`
		Plan         queryplan.QueryPlan `json:"plan"`
		OutputFormat string              `json:"output_format,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	for _, aggregate := range args.Plan.Aggregates {
		if aggregate.ResultEncoding != "" {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest,
				Message: "result_encoding 仅由 query_sql 的受审 SQL lowering 生成，execute_plan 不接受该字段"}
		}
	}
	if args.OutputFormat != "" && args.OutputFormat != "json" && args.OutputFormat != "table" {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "output_format 仅支持 json 或 table"}
	}
	if err := validateRequestID(args.RequestID); err != nil {
		return nil, err
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return nil, err
	}
	requestJSON, err := json.Marshal(args.Plan)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "QueryPlan 无法编码"}
	}
	requestSummary := "execute_plan\x00" + task.ID + "\x00" + string(requestJSON) + "\x00" + defaultString(args.OutputFormat, "json")
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, args.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		replayed, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
		if err != nil {
			return nil, err
		}
		replayed["query_plan"] = args.Plan
		replayed["output_format"] = defaultString(args.OutputFormat, "json")
		return replayed, nil
	}
	if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if err := requireOrdinaryTask(grant); err != nil {
		return nil, err
	}
	prepared, err := s.prepareTaskPlan(ctx, task, grant, args.Plan)
	if err != nil {
		replayed, replayErr := s.replayPreparationFailureAt(ctx, task.ID, args.RequestID, digest(requestSummary), requestStarted, err)
		if replayErr != nil {
			return nil, replayErr
		}
		response, ok := replayed.(map[string]any)
		if !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久查询重放结果格式无效"}
		}
		response["query_plan"] = args.Plan
		response["output_format"] = defaultString(args.OutputFormat, "json")
		return response, nil
	}
	resultNames := queryPlanResultNames(args.Plan)
	metadata := &queryResponseMetadata{Plan: args.Plan, OutputFormat: defaultString(args.OutputFormat, "json"),
		DisplayColumns: resultNames, ResultOrder: identityResultOrder(len(resultNames)),
		SemanticColumns: queryPlanSemanticColumns(args.Plan)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	return s.executeSQL(ctx, principal, task, args.RequestID, prepared, requestSummary, metadata, requestStarted)
}

func predicateFootprintResponseFor(context *planExposureContext) *predicateFootprintResponse {
	if context == nil || context.predicateFootprint == nil {
		return nil
	}
	return &predicateFootprintResponse{RawLiteralCount: context.predicateFootprint.RawLiteralCount,
		UniqueAtomCount: context.predicateFootprint.UniqueAtomCount}
}

// preparedQueryPlan is one prepared operation as the rest of the Gateway needs
// it: the statement to execute, the authorization to admit it against, and --
// when the profile accounts exposure -- the observation context.
//
// PolicyGrant is preparation's, not the task's. Preparation widens the task
// grant by the metering closure and the ordinal sidecars, and it is the widened
// grant the statements were compiled to be admitted by, so carrying it is what
// keeps the Gateway from widening a second time and reaching a different answer.
type preparedQueryPlan struct {
	SQL         string
	PolicyGrant sqlpolicy.Grant
	Exposure    *planExposureContext
	// Prepared is the sealed preparation the statement was compiled by, and it is
	// here for every operation rather than only for the ones that account
	// exposure.
	//
	// It used to be reachable only through Exposure, which is why a plain query
	// signed no execution binding: the one document a binding is built from was
	// discarded the moment the caller had the SQL. The receipt now requires every
	// completed query to describe its execution, and describing it means carrying
	// the preparation a finalizer reproduces -- so a path that prepares and then
	// forgets is a path that cannot complete.
	Prepared physicalquery.PreparedOperation
	// EnrichResult, when set, completes the canonical result before its
	// response metadata is applied and before settlement: a retrieval adds the
	// chunk text after checking it against the released digests. A failure
	// fails the query unsettled, so nothing is released for a result that
	// cannot be delivered. The exposure observation is already fixed by then
	// and covers only the sealed statement's visible columns.
	EnrichResult func(ctx context.Context, stored *storedQueryResult) error
}

// operationPrepared reports whether this value carries a real preparation.
//
// Asked of the sealed binding's digest rather than of a separate flag: the seal
// is filled by Prepare and by nothing else, so a zero PreparedOperation --
// which is what a caller that skipped preparation would hold -- is exactly the
// value with no digest.
func (prepared preparedQueryPlan) operationPrepared() bool {
	return prepared.Prepared.Binding().SHA256 != ""
}

// preparePlan is the only QueryPlan-to-execution boundary. Both the advanced
// structured entrypoint and exposure-enabled reporting SQL use it so visible
// SQL, provenance, FactIDs, ordinal programs, and semantic replay cannot drift
// between public input syntaxes. It performs no reservation or database I/O.
//
// It derives nothing. The statements, the projections, the policy grant, the
// canonical plan identity, the ordinal program and the V5 predicate footprint
// are all produced by internal/physicalquery.Prepare from immutable inputs, and
// the finalizer reproduces them by calling the same function with values it
// obtains for itself. There is no second path and no fallback: a shape Prepare
// refuses is a shape this Gateway does not execute.
func (s *Service) preparePlan(grant control.TaskGrant, plan queryplan.QueryPlan) (preparedQueryPlan, error) {
	inputs, err := s.preparationInputs(grant, plan)
	if err != nil {
		// Building the inputs is the Gateway's own half -- reading the loaded
		// Catalog and resolving Publications through the verified registry -- so a
		// failure here is server state disagreeing with itself, not a refused plan.
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "查询无法在当前 Catalog 与快照证据下构造准备输入", Cause: err}
	}
	prepared, err := physicalquery.Prepare(inputs)
	if err != nil {
		// The cause names the check that refused; it is logged, not returned.
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied,
			Message: "QueryPlan 不在任务授权可精确计量的数据暴露片段内", Cause: err}
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "本进程无法确定查询编译器身份", Cause: err}
	}
	// Validate proves the object is the one its binding describes; RequireInputs
	// proves the binding names these inputs and this compiler. The Gateway holds
	// the source material, so it makes both checks before executing -- exactly as
	// the finalizer does before comparing.
	if err := prepared.RequireInputs(inputs, compiler); err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "准备结果与其输入证据不一致；查询未执行", Cause: err}
	}
	visibleSQL, _, err := prepared.ExecutableStatements()
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "准备结果拒绝交出可执行语句", Cause: err}
	}
	result := preparedQueryPlan{SQL: visibleSQL, PolicyGrant: prepared.PolicyGrant(), Prepared: prepared}
	if !grant.Exposure.Enabled() {
		// A plain query has no companion, no plan identity and no ledger, so there
		// is nothing for an exposure context to hold. The preparation itself is
		// still carried: the execution binding is built from it, and a plain query
		// describes its execution like every other completed one.
		return result, nil
	}
	context, err := s.planExposureContextFrom(prepared, plan)
	if err != nil {
		return preparedQueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "已准备的查询无法在当前快照索引下建立观测上下文", Cause: err}
	}
	result.Exposure = context
	return result, nil
}

func predicateLimitsForGrant(grant control.ExposureGrant) queryplan.PredicateLimits {
	if grant.PredicateFootprint == nil {
		return queryplan.PredicateLimits{}
	}
	return queryplan.PredicateLimits{MaxRawLiteralsPerQuery: int(grant.PredicateFootprint.MaxRawLiteralsPerQuery),
		MaxUniqueAtomsPerQuery:   int(grant.PredicateFootprint.MaxUniqueAtomsPerQuery),
		MaxAtomPayloadBytes:      int(grant.PredicateFootprint.MaxAtomPayloadBytes),
		MaxTotalAtomPayloadBytes: int(grant.PredicateFootprint.MaxTotalAtomPayloadBytes)}
}

// executeSQL runs one prepared operation and settles it.
//
// It takes the whole preparedQueryPlan rather than the statement and an optional
// exposure context. The statement alone was enough while a plain query was
// allowed to complete undescribed; it is not enough now that every completed
// query signs the preparation its statement was compiled by, and passing the
// members separately is how the one that mattered came to be dropped at a call
// site that had it in hand.
func (s *Service) executeSQL(ctx context.Context, principal mcp.Principal, task control.Task,
	requestID string, operation preparedQueryPlan, requestSummary string,
	responseMetadata *queryResponseMetadata, requestStarted time.Time) (any, error) {
	agentSQL := operation.SQL
	exposureContext := operation.Exposure
	if requestStarted.IsZero() {
		requestStarted = time.Now()
	}
	pipelineStarted := requestStarted
	pipeline := &queryPipelineMeasurement{requestStarted: requestStarted}
	requestDigest := digest(requestSummary)
	// An idempotent retry observes the first durable result/status even if the
	// task has since expired, been revoked, or exhausted its budget.
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, requestID)
	if lookupErr == nil {
		if existing.RequestDigest != requestDigest {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		return s.queryReplayResponseAt(ctx, existing, pipelineStarted, time.Now())
	}
	if !errors.Is(lookupErr, control.ErrNotFound) {
		return nil, lookupErr
	}
	if task.State != control.TaskActive {
		return nil, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return nil, toolError(err)
	}
	if task.CatalogVersion != s.catalog.CatalogVersion {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务目录版本与当前实例不一致；为避免扩权已拒绝查询"}
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	if grant.Exposure.Enabled() && exposureContext == nil {
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	// Nothing executes unprepared. A completed query must sign the preparation its
	// statement was compiled by, so an operation that reached here without one
	// could only settle as a receipt the contract refuses -- after the Connector
	// had already run. Refusing before the reservation is what keeps that from
	// being discovered at signing time.
	if !operation.operationPrepared() {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "查询未经共享准备，无法产生可独立重建的执行证据；已关闭式拒绝"}
	}
	protocolGrant, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil || approval.VerifyTaskGrantV1(s.receiptVerifier, protocolGrant) != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久授权证明无效；查询已关闭式拒绝"}
	}
	grantDigest, err := approval.GrantCoreDigest(protocolGrant.Core)
	if err != nil || !storedGrantMatchesProtocol(task, grant, protocolGrant) ||
		protocolGrant.Core.CatalogSHA256 != s.catalog.SHA256 {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权、目录或持久 Grant 不一致；查询已关闭式拒绝"}
	}
	if err := s.validateDelegatedGrant(ctx, task, protocolGrant.Core, s.clock().UTC()); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "委托 Grant 不再受有效父任务约束；查询已关闭式拒绝"}
	}
	grantRemaining := grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining < time.Millisecond {
		return nil, toolError(control.ErrTaskExpired)
	}
	persistedPending, err := decodePersistedPending(task)
	if err != nil {
		return nil, err
	}
	parentCore, err := domain.CoreFromManifest(
		persistedPending.Manifest,
		persistedPending.ManifestDigest,
		protocolGrant.ApprovalReceipt.IssuedAt,
	)
	if err != nil || persistedPending.ManifestDigest != protocolGrant.Core.ManifestDigest ||
		parentCore.CheckNarrowing(protocolGrant.Core) != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务上下文与已签名 Grant 不一致；查询已关闭式拒绝"}
	}
	var viewExpectation *dataconnector.ViewRegistryExpectation
	if protocolGrant.Core.ViewBindingDigest != "" {
		status, statusErr := s.store.GetTaskViewBindingStatus(ctx, task.ID)
		if statusErr != nil || status.Status != control.TaskViewBindingActive ||
			persistedPending.ViewBinding == nil ||
			!sameSnapshotSHA256(persistedPending.ViewBinding.Digest, protocolGrant.Core.ViewBindingDigest) {
			if replayed, found, replayErr := s.replayQueryIfPresentAt(ctx, task.ID, requestID, requestDigest, pipelineStarted); found || replayErr != nil {
				return replayed, replayErr
			}
			return nil, &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged), Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
		}
		boundProducts, bindingErr := boundViewProducts(persistedPending.ViewBinding)
		var current *resolvedViewBinding
		if bindingErr == nil {
			current, bindingErr = s.resolveViewBinding(ctx, boundProducts)
		}
		if bindingErr != nil || !viewBindingMatchesCurrent(persistedPending.ViewBinding, current) {
			if bindingErr != nil && !dataconnector.IsCode(bindingErr, dataconnector.CodeViewSemanticChanged) {
				return nil, bindingErr
			}
			observed := viewSemanticObservationDigest(protocolGrant.Core.ViewBindingDigest, bindingErr)
			if current != nil {
				observed = current.Digest
			}
			if replayed, found, replayErr := s.replayQueryIfPresentAt(ctx, task.ID, requestID, requestDigest, pipelineStarted); found || replayErr != nil {
				return replayed, replayErr
			}
			_, _ = s.store.MarkTaskViewSemanticChanged(ctx, task.ID, observed)
			return nil, &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged), Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
		}
		if exposureContext != nil && exposureContext.viewBindingDigest != "" &&
			(!sameSnapshotSHA256(exposureContext.viewBindingDigest, current.Digest) ||
				exposureContext.viewRegistryRevision != current.Expectation.ExpectedRevisionDigest) {
			observed := current.Digest
			if observed == "" {
				observed = viewSemanticObservationDigest(protocolGrant.Core.ViewBindingDigest, viewSemanticChangedError())
			}
			_, _ = s.store.MarkTaskViewSemanticChanged(ctx, task.ID, observed)
			if replayed, found, replayErr := s.replayQueryIfPresentAt(ctx, task.ID, requestID, requestDigest, pipelineStarted); found || replayErr != nil {
				return replayed, replayErr
			}
			return nil, &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged), Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
		}
		expectation := current.Expectation
		expectation.Roots = append([]viewcompiler.RelationName(nil), current.Expectation.Roots...)
		expectation.BaseProducts = cloneStringMap(current.Expectation.BaseProducts)
		viewExpectation = &expectation
	} else if persistedPending.ViewBinding != nil || grant.ViewBindingDigest != "" {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务携带不一致的 View binding 证据"}
	}
	budget, err := s.store.GetBudget(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	remaining := budget.Remaining()
	if remaining.Queries < 1 || remaining.Rows < 1 || remaining.DBMS < 1 {
		return nil, &mcp.ToolError{Code: apierr.CodeBudgetExhausted, Message: "任务预算已耗尽"}
	}
	componentMS := map[string]float64{}
	policyStarted := time.Now()
	var policyGrant sqlpolicy.Grant
	var exposureLedger control.ExposureLedgerSnapshot
	if exposureContext == nil {
		// Nothing was prepared, or what was prepared accounts no exposure: the
		// statement is admitted against the task grant unwidened. This path also
		// carries the agent's own SQL, which no preparation ever saw.
		if policyGrant, err = s.policyGrant(grant); err != nil {
			return nil, err
		}
	} else {
		// The authorization is preparation's, not a second construction of it.
		// Preparation widened the task grant by the metering closure and then by
		// the ordinal sidecars, and compiled the statements to be admitted by the
		// result; widening a second time here would be a second derivation of one
		// authorization, and the two could disagree.
		//
		// What this read of the grant IS for is the check below. The grant was
		// loaded once for preparation and again here, and an approval that narrowed
		// in between must not be executed against the wider one preparation sealed.
		// Comparing the sealed grant digest is exact: it is the same mapping over
		// the same values, so a difference is a real change rather than a mapping
		// artifact.
		if err := requirePreparedGrantStillCurrent(exposureContext, grant); err != nil {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		policyGrant = exposureContext.policyGrant
		exposureLedger, err = s.store.GetExposureLedger(ctx, task.ID)
		if err != nil || exposureLedger.ProfileVersion != grant.Exposure.ProfileVersion {
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
	}
	engine := sqlpolicy.New(sqlpolicy.Config{})
	// The row-limit derivation lives in internal/physicalquery so that the
	// evaluation and the finalizer reproduce the statements this executes rather
	// than reimplementing the arithmetic. sqlpolicy renders the limit into the
	// SQL, so a second implementation would change the executed bytes.
	//
	// This first derivation is PREPARATION ONLY. Its purpose is to learn what to
	// reserve: the reservation needs a requested row count and, under an exposure
	// profile, fact estimates that depend on the derived limits. The budget it
	// reads was fetched above, outside any lock, and the exposure ledger likewise;
	// between here and ReserveBudget another request on the same task can settle a
	// query and move both. The statements actually executed are re-derived below
	// from the pre-state the reservation itself observed under the task lock.
	companionSQL := ""
	if exposureContext != nil {
		companionSQL = exposureContext.provenanceSQL
	}
	preparationState := physicalquery.LedgerPreState{
		RemainingRows: remaining.Rows, HasExposureContext: exposureContext != nil,
	}
	if exposureContext != nil {
		preparationState.InfluenceFacts = exposureLedger.Limits.InfluenceFacts
		preparationState.UsesExpandedEvidence = exposureContext.usesExpandedEvidence()
	}
	prepared, err := s.derivePhysicalQuery(engine, agentSQL, companionSQL, policyGrant, preparationState,
		exposureContext != nil)
	if err != nil {
		return nil, err
	}
	componentMS["parse_policy"] = durationMS(time.Since(policyStarted))
	componentMS["authorization"] = durationMS(policyStarted.Sub(pipelineStarted))
	evidence, err := s.datasourceEvidence(ctx, protocolGrant.Core.ApprovedProducts)
	if err != nil {
		return nil, err
	}
	if protocolGrant.Core.DatasourceID != evidence.DatasourceID ||
		protocolGrant.Core.SchemaDigest != evidence.SchemaDigest ||
		grant.DatasourceID != evidence.DatasourceID || grant.SchemaDigest != evidence.SchemaDigest {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "授权数据源与当前实例不一致；查询已关闭式拒绝"}
	}
	if exposureContext != nil && exposureContext.ordinal != nil {
		if err := s.store.PutOrdinalDictionarySet(ctx, exposureContext.ordinal.DictionarySet); err != nil {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "V4 dictionary set 无法按 Catalog 证据发布"}
		}
	}
	queryID := randomID("query")
	approvedPerQueryTimeout := time.Duration(protocolGrant.Core.Budget.PerQueryTimeoutMS) * time.Millisecond
	requestedTimeout := approvedPerQueryTimeout
	if requestedTimeout > grantRemaining {
		requestedTimeout = grantRemaining
	}
	requestedDBMS := min64(remaining.DBMS, requestedTimeout.Milliseconds())
	if requestedDBMS < 1 {
		return nil, toolError(control.ErrTaskExpired)
	}
	if s.beforeReserveBudget != nil {
		s.beforeReserveBudget(ctx, task.ID)
	}
	reserveStarted := time.Now()
	reserveRequest := control.ReserveRequest{
		QueryID: queryID, TaskID: task.ID, RequestID: requestID, Actor: principal.Subject,
		RequestDigest: requestDigest, SQLFingerprint: prepared.visible.Fingerprint,
		CatalogVersion: task.CatalogVersion, CatalogDigest: protocolGrant.Core.CatalogSHA256,
		DatasourceID: evidence.DatasourceID, SchemaDigest: evidence.SchemaDigest,
		ViewBindingDigest: protocolGrant.Core.ViewBindingDigest,
		ManifestDigest:    protocolGrant.Core.ManifestDigest, GrantDigest: grantDigest, PolicyDecision: "ALLOW",
		RequestedRows: prepared.visible.RowLimit, RequestedDBMS: requestedDBMS,
	}
	if exposureContext != nil {
		reserveRequest.Exposure = &control.ExposureReservationRequest{
			ProfileVersion:          exposureLedger.ProfileVersion,
			EstimatedReleaseFacts:   saturatedProduct(prepared.visible.RowLimit, int64(len(exposureContext.visibleFields))),
			EstimatedInfluenceFacts: saturatedProduct(prepared.companionEvidenceRows, int64(len(exposureContext.provenanceFields)+1)),
		}
		if exposureLedger.ProfileVersion == exposure.ProfileV3 || exposureLedger.ProfileVersion == exposure.ProfileV4 {
			reserveRequest.Exposure.EstimatedOutcomeFacts = 1
		} else if exposureLedger.ProfileVersion == exposure.ProfileV5 {
			if exposureContext.predicateFootprint == nil {
				return nil, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "V5 查询缺少执行前谓词足迹"}
			}
			reserveRequest.Exposure.EstimatedOutcomeFacts = int64(exposureContext.predicateFootprint.UniqueAtomCount) + 1
		}
	}
	reservation, err := s.store.ReserveBudget(ctx, reserveRequest)
	if err != nil {
		return nil, err
	}
	componentMS["reserve"] = durationMS(time.Since(reserveStarted))
	if reservation.Replay && reservation.Record != nil {
		return s.queryReplayResponseAt(ctx, *reservation.Record, pipelineStarted, time.Now())
	}
	// THE AUTHORITATIVE PRE-STATE. reservation.Before and
	// reservation.ExposureLedgerBefore were read under one task lock, in the same
	// transaction that took the reservation. Everything below -- the executed
	// statements, the row limits rendered into them, the semantic replay key, the
	// signed execution binding -- derives from this one state, so the receipt does
	// not assemble a pre-state out of three separate observations and sign it as
	// though it were atomic.
	executionState := physicalquery.LedgerPreState{
		RemainingRows: reservation.Before.Remaining().Rows, HasExposureContext: exposureContext != nil,
	}
	if exposureContext != nil {
		if reservation.ExposureLedgerBefore == nil {
			s.releaseQueryBudget(ctx, queryID, "EXPOSURE_PRE_STATE_UNAVAILABLE")
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		exposureLedger = *reservation.ExposureLedgerBefore
		executionState.InfluenceFacts = exposureLedger.Limits.InfluenceFacts
		executionState.UsesExpandedEvidence = exposureContext.usesExpandedEvidence()
	}
	executed, err := s.derivePhysicalQuery(engine, agentSQL, companionSQL, policyGrant, executionState,
		exposureContext != nil)
	if err != nil {
		s.releaseQueryBudget(ctx, queryID, "AUTHORIZATION_PRE_STATE_CHANGED")
		return nil, err
	}
	// The reservation was sized from the preparation derivation. A pre-state that
	// moved in the widening direction -- a raised budget, a raised influence
	// limit -- would leave the authoritative derivation asking for more than was
	// reserved, and nothing downstream would notice. Fail closed rather than
	// execute an under-reserved statement.
	if err := requireNarrowedDerivation(prepared, executed, reservation.AllowedRows); err != nil {
		s.releaseQueryBudget(ctx, queryID, "AUTHORIZATION_PRE_STATE_WIDENED")
		return nil, toolError(control.ErrExposureEvidenceRequired)
	}
	decision := executed.derivation.VisibleDecision
	var provenanceDecision sqlpolicy.Decision
	var provenanceEvidenceRows int64
	if exposureContext != nil {
		provenanceDecision = *executed.derivation.CompanionDecision
		provenanceEvidenceRows = executed.companionEvidenceRows
	}
	var ordinalCacheKey string
	if exposureContext != nil && exposureContext.ordinal != nil &&
		(exposureLedger.ProfileVersion == exposure.ProfileV4 || exposureLedger.ProfileVersion == exposure.ProfileV5) {
		authorizationDigest := ordinalSemanticAuthorizationDigest(decision, provenanceDecision,
			protocolGrant.Core.ManifestDigest)
		if exposureLedger.ProfileVersion == exposure.ProfileV5 {
			authorizationDigest = ordinalSemanticAuthorizationDigestV5(exposureContext.planDigest,
				protocolGrant.Core.ManifestDigest, exposureContext.predicateFootprint)
		}
		semanticBinding := ordinalSemanticReplayBinding(task.ID, grantDigest, authorizationDigest,
			exposureContext.planDigest, s.catalog.SHA256, evidence.SchemaDigest, exposureContext.ordinal.DictionarySetDigest)
		if exposureLedger.ProfileVersion == exposure.ProfileV5 {
			semanticBinding = ordinalSemanticReplayBindingV5(task.ID, grantDigest, authorizationDigest,
				exposureContext.planDigest, s.catalog.SHA256, evidence.SchemaDigest,
				exposureContext.ordinal.DictionarySetDigest, exposureContext.predicateFootprint)
		}
		ordinalCacheKey, err = semanticBinding.Digest()
		if err != nil {
			s.releaseQueryBudget(ctx, queryID, "ORDINAL_REPLAY_KEY_INVALID")
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "ordinal semantic replay key 无法规范化"}
		}
	}
	// The prepared identity of this operation, and the sealed pre-state the
	// limits above were derived from. Both are needed before anything runs: the
	// binding written with the terminal evidence must describe the statements
	// that were actually sent, and it must name the state that authorized them.
	preparedOp, ledgerBefore, err := s.prepareExecutionBinding(operation.Prepared, exposureContext != nil,
		executionState, exposureLedger, reservation.Before)
	if err != nil {
		s.releaseQueryBudget(ctx, queryID, "EXECUTION_BINDING_UNAVAILABLE")
		return nil, &mcp.ToolError{Code: apierr.CodeConflict,
			Message: "执行绑定无法按已签名的前置状态构造；查询未执行"}
	}
	if ordinalCacheKey != "" {
		// The replay's own binding, built before the lookup so a hit settles with
		// it. executed=false on both targets: nothing reaches the Connector.
		built, bindingErr := buildQueryExecutionBinding(queryID, querybinding.PathSemanticReplay, preparedOp,
			executed, ledgerBefore, reservation.Before, false)
		if bindingErr != nil {
			s.releaseQueryBudget(ctx, queryID, "EXECUTION_BINDING_INVALID")
			return nil, &mcp.ToolError{Code: apierr.CodeConflict,
				Message: "语义重放的执行绑定无法构造；查询未执行"}
		}
		semanticBinding := &built
		replayed, replayOutcome, replayErr := s.tryOrdinalSemanticReplayForQuery(ctx, task, requestID, queryID, grantDigest,
			ordinalCacheKey, exposureContext.ordinal.DictionarySetDigest, reservation, componentMS, responseMetadata, pipeline,
			semanticBinding)
		if replayErr != nil {
			return nil, replayErr
		}
		switch replayOutcome {
		case ordinalReplayCompleted:
			return replayed, nil
		case ordinalReplayContinueNovel:
			// tryOrdinalSemanticReplay explicitly returned ownership of the
			// still-live reservation. The novel path below must settle it.
		default:
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "ordinal replay 返回了非规范终态"}
		}
	}
	pipeline.prepareFinished = time.Now()
	var releaseDerivationSlot func()
	if exposureContext != nil && exposureContext.ordinal != nil &&
		exposureContext.ordinal.EstimatedBaseFacts >= 1_000_000 && s.highCardinalityDerivations != nil {
		queueCtx, queueCancel := context.WithDeadline(ctx, grant.ExpiresAt)
		select {
		case s.highCardinalityDerivations <- struct{}{}:
			releaseDerivationSlot = func() { <-s.highCardinalityDerivations }
		case <-queueCtx.Done():
			queueCancel()
			s.releaseQueryBudget(ctx, queryID, "ORDINAL_DERIVATION_QUEUE_EXPIRED")
			return nil, toolError(control.ErrTaskExpired)
		}
		queueCancel()
	}
	if releaseDerivationSlot != nil {
		defer releaseDerivationSlot()
	}
	timeout := time.Duration(reservation.AllowedDBMS) * time.Millisecond
	if timeout > approvedPerQueryTimeout {
		timeout = approvedPerQueryTimeout
	}
	grantRemaining = grant.ExpiresAt.Sub(s.clock().UTC())
	if grantRemaining <= 0 {
		// The connector has not been invoked, so this reservation may still be
		// released without charging query usage.
		s.releaseQueryBudget(ctx, queryID, "AUTHORIZATION_EXPIRED")
		return nil, toolError(control.ErrTaskExpired)
	}
	if timeout > grantRemaining {
		timeout = grantRemaining
	}
	queryTimeout := timeout + 250*time.Millisecond
	if queryTimeout > grantRemaining {
		queryTimeout = grantRemaining
	}
	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	connectorStarted := time.Now()
	var data dataconnector.Result
	var provenanceData dataconnector.Result
	var ordinalSink *ordinalDerivationSink
	var ordinalStreamDuration time.Duration
	var ordinalConsumerDuration time.Duration
	var ordinalPreparationDuration time.Duration
	var queryErr error
	if exposureContext == nil {
		data, queryErr = s.connector.Query(queryCtx, dataconnector.QueryRequest{
			SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows, ViewRegistry: viewExpectation,
		})
	} else if exposureContext.ordinal != nil {
		streaming, ok := s.connector.(interface {
			QueryPairStream(context.Context, dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error)
		})
		if !ok {
			s.releaseQueryBudget(ctx, queryID, "EXPOSURE_SNAPSHOT_UNAVAILABLE")
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		ordinalSink = &ordinalDerivationSink{program: exposureContext.ordinal.Program,
			indexes: exposureContext.ordinal.Indexes, planDigest: exposureContext.planDigest,
			predicateFootprint: exposureContext.predicateFootprint}
		pair, pairErr := streaming.QueryPairStream(queryCtx, dataconnector.QueryPairStreamRequest{
			Visible: dataconnector.QueryRequest{SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows, ViewRegistry: viewExpectation},
			Provenance: dataconnector.QueryRequest{SQL: provenanceDecision.SQL, StatementTimeout: timeout,
				MaxRows: provenanceEvidenceRows, ViewRegistry: viewExpectation},
			ProvenanceSink: ordinalSink,
		})
		data, queryErr = pair.Visible, pairErr
		provenanceData = dataconnector.Result{Columns: pair.Provenance.Columns, RowCount: pair.Provenance.RowCount,
			DatabaseTime: pair.Provenance.DatabaseTime, Truncated: pair.Provenance.Truncated}
		ordinalStreamDuration = pair.Provenance.DatabaseTime
		ordinalConsumerDuration = pair.Provenance.ConsumerTime
		ordinalPreparationDuration = pair.VisibleSinkTime
	} else {
		paired, ok := s.connector.(interface {
			QueryPair(context.Context, dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error)
		})
		if !ok {
			s.releaseQueryBudget(ctx, queryID, "EXPOSURE_SNAPSHOT_UNAVAILABLE")
			return nil, toolError(control.ErrExposureEvidenceRequired)
		}
		pair, pairErr := paired.QueryPair(queryCtx, dataconnector.QueryPairRequest{
			Visible:    dataconnector.QueryRequest{SQL: decision.SQL, StatementTimeout: timeout, MaxRows: reservation.AllowedRows, ViewRegistry: viewExpectation},
			Provenance: dataconnector.QueryRequest{SQL: provenanceDecision.SQL, StatementTimeout: timeout, MaxRows: provenanceEvidenceRows, ViewRegistry: viewExpectation},
		})
		data, provenanceData, queryErr = pair.Visible, pair.Provenance, pairErr
	}
	connectorFinished := time.Now()
	businessDatabaseDuration := data.DatabaseTime
	provenanceDatabaseDuration := provenanceData.DatabaseTime
	if exposureContext != nil {
		data.DatabaseTime = businessDatabaseDuration + provenanceDatabaseDuration
	}
	settlement := querySettlement(queryID, data, connectorStarted, reservation)
	// The binding is built here, after the Connector returned, because
	// TargetRecordV1.Executed must record what was sent rather than what was
	// planned. It is attached to the settlement so it is written inside the same
	// transaction that commits the terminal query record, the budget settlement,
	// the exposure settlement and the receipt: a settled query has both rows or
	// neither.
	//
	// It is NOT attached when the connector failed. A failed execution cannot
	// prove which of its targets ran, and a binding that claimed paired-novel
	// semantics for a sequence that may have stopped after the visible statement
	// would be a false description rather than a missing one.
	if queryErr == nil {
		path := querybinding.PathPairedNovel
		if executed.companion == nil {
			path = querybinding.PathSingleQuery
		}
		binding, bindingErr := buildQueryExecutionBinding(queryID, path, preparedOp, executed,
			ledgerBefore, reservation.Before, true)
		if bindingErr != nil {
			settlement.ErrorCode = "EXECUTION_BINDING_INVALID"
			s.failQueryBudget(ctx, settlement)
			return nil, &mcp.ToolError{Code: apierr.CodeConflict,
				Message: "执行绑定无法按已签名的前置状态构造；结果未释放"}
		}
		settlement.ExecutionBinding = &binding
	}
	if queryErr != nil {
		code := string(dataconnector.CodeQueryFailed)
		var connectorErr *dataconnector.Error
		if errors.As(queryErr, &connectorErr) {
			code = string(connectorErr.Code)
		}
		settlement.ErrorCode = code
		if code == string(dataconnector.CodeViewSemanticChanged) {
			s.releaseQueryBudget(ctx, queryID, code)
			if protocolGrant.Core.ViewBindingDigest != "" {
				observed := viewSemanticObservationDigest(protocolGrant.Core.ViewBindingDigest, queryErr)
				_, _ = s.store.MarkTaskViewSemanticChanged(ctx, task.ID, observed)
			}
		} else if code == string(dataconnector.CodeConnection) || code == string(dataconnector.CodeInvalidQuery) {
			s.releaseQueryBudget(ctx, queryID, code)
		} else {
			s.markQueryIndeterminate(ctx, queryID, code)
		}
		return nil, queryErr
	}
	if exposureContext != nil {
		derivationStarted := time.Now()
		var deriveErr error
		if exposureContext.ordinal != nil {
			if provenanceData.Truncated || ordinalSink == nil {
				deriveErr = errProvenanceTruncated
			} else {
				var effect ordinalEffect
				effect, deriveErr = ordinalSink.Finish()
				if deriveErr == nil {
					var observation control.OrdinalExposureObservation
					observation, deriveErr = ordinalControlObservation(effect, exposureContext.ordinal.DictionarySetDigest)
					if deriveErr == nil {
						settlement.OrdinalExposure = &observation
					}
				}
			}
		} else {
			var observation exposure.Observation
			observation, deriveErr = exposureContext.deriveObservation(data, provenanceData, exposureLedger.ProfileVersion)
			if deriveErr == nil {
				settlement.Exposure = &observation
			}
		}
		finishDuration := time.Since(derivationStarted)
		componentMS["exposure_derivation"] = durationMS(finishDuration)
		if exposureContext.ordinal != nil {
			recordOrdinalTimingComponents(componentMS, ordinalStreamDuration, ordinalConsumerDuration,
				ordinalPreparationDuration, finishDuration)
		}
		if deriveErr != nil {
			settlement.ErrorCode = "EXPOSURE_PROVENANCE_INVALID"
			s.failQueryBudget(ctx, settlement)
			return nil, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "查询的来源证据不完整，因此结果未释放"}
		}
		data, err = exposureContext.visibleResult(data)
		if err != nil {
			settlement.ErrorCode = "EXPOSURE_RESULT_INVALID"
			s.failQueryBudget(ctx, settlement)
			return nil, err
		}
	}
	totalDatabaseDuration := data.DatabaseTime
	if totalDatabaseDuration <= 0 {
		totalDatabaseDuration = connectorFinished.Sub(connectorStarted)
	}
	if exposureContext == nil && businessDatabaseDuration <= 0 {
		businessDatabaseDuration = totalDatabaseDuration
	}
	ordinalConnectorConsumer := time.Duration(0)
	if exposureContext != nil && exposureContext.ordinal != nil {
		ordinalConnectorConsumer = ordinalPreparationDuration
	}
	// VisibleResult is invoked between the two statements, so it is inside the
	// connector wall clock but outside both query-to-drain database timers. V4
	// reports it as bitmap work; remove it here to keep the leaf components
	// disjoint. ordinal_stream remains an explicitly overlapping aggregate.
	connectorOverhead := connectorOverheadDuration(connectorFinished.Sub(connectorStarted), totalDatabaseDuration,
		ordinalConnectorConsumer)
	componentMS["business_postgresql"] = durationMS(businessDatabaseDuration)
	if exposureContext != nil {
		measuredProvenance := provenanceDatabaseDuration
		if exposureContext.ordinal != nil {
			measuredProvenance -= ordinalConsumerDuration
			if measuredProvenance < 0 {
				measuredProvenance = 0
			}
		}
		componentMS["provenance_postgresql"] = durationMS(measuredProvenance)
	}
	componentMS["connector_overhead"] = durationMS(connectorOverhead)
	stored := storedQueryResult{
		Columns: data.Columns, Rows: data.Rows, RowCount: data.RowCount,
		DatabaseMS: settlement.DBMS, ComponentMS: componentMS,
		Limited: data.Truncated || data.RowCount == reservation.AllowedRows,
	}
	if operation.EnrichResult != nil {
		deliveryStarted := time.Now()
		enrichErr := operation.EnrichResult(ctx, &stored)
		componentMS["result_delivery"] = durationMS(time.Since(deliveryStarted))
		if enrichErr != nil {
			settlement.ErrorCode = "RESULT_DELIVERY_REFUSED"
			// A download's whole-document refusal keeps its own code in the
			// terminal record, so a retry under the same request id answers
			// with the same refusal (docs/kb-design.md §12).
			var refusal *mcp.ToolError
			if errors.As(enrichErr, &refusal) && refusal.Code == apierr.CodeDocumentNotDownloadable {
				settlement.ErrorCode = refusal.Code
			}
			s.failQueryBudget(ctx, settlement)
			return nil, enrichErr
		}
	}
	if err := applyQueryResponseMetadata(&stored, responseMetadata); err != nil {
		settlement.ErrorCode = resultEncodingFailed
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	if ordinalCacheKey != "" {
		expires := grant.ExpiresAt.UTC()
		settlement.OrdinalMaterialization = &control.OrdinalMaterializationPublish{CacheKeySHA256: ordinalCacheKey,
			ExpiresAt: &expires, ProfileVersion: exposureLedger.ProfileVersion}
	}
	if s.resultArtifacts != nil {
		pipeline.executeFinished = time.Now()
		return s.finalizeArtifactQuery(ctx, task, requestID, settlement, stored, componentMS, pipeline)
	}
	encodingStarted := time.Now()
	resultSpool, err := newEncryptedQuerySpool(s.spoolDirectory, task.ID, queryID, s.spoolThreshold)
	if err == nil {
		err = writeStoredQueryResult(resultSpool, stored)
	}
	if err == nil {
		err = resultSpool.Seal()
	}
	componentMS["result_encoding"] = durationMS(time.Since(encodingStarted))
	if err != nil {
		if resultSpool != nil {
			_ = resultSpool.Close()
		}
		settlement.ErrorCode = resultEncodingFailed
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	defer resultSpool.Close()
	finalizeCtx, finalizeCancel := s.detachedContext(ctx)
	var record control.QueryRecord
	var persistedReceipt control.PersistedQueryReceipt
	var finalizeMetrics control.FinalizeQueryMetrics
	if resultSpool.Spilled() {
		var plaintextReader io.ReadCloser
		plaintextReader, err = resultSpool.Open()
		if err == nil {
			if settlement.OrdinalExposure != nil || settlement.OrdinalObservationRef != nil {
				record, persistedReceipt, finalizeMetrics, err = s.store.FinalizeOrdinalQueryStreamMeasuredWithReceipt(finalizeCtx,
					settlement, plaintextReader, settlement.OrdinalMaterialization, s.terminalReceiptBuilder())
			} else {
				record, persistedReceipt, finalizeMetrics, err = s.store.FinalizeQueryStreamMeasuredWithReceipt(finalizeCtx,
					settlement, plaintextReader, s.terminalReceiptBuilder())
			}
			_ = plaintextReader.Close()
		}
	} else {
		var plaintext []byte
		plaintext, err = resultSpool.Bytes()
		if err == nil {
			if settlement.OrdinalExposure != nil || settlement.OrdinalObservationRef != nil {
				record, persistedReceipt, finalizeMetrics, err = s.store.FinalizeOrdinalQueryMeasuredWithReceipt(finalizeCtx, settlement, plaintext,
					settlement.OrdinalMaterialization, s.terminalReceiptBuilder())
			} else {
				record, persistedReceipt, finalizeMetrics, err = s.store.FinalizeQueryMeasuredWithReceipt(finalizeCtx, settlement, plaintext, s.terminalReceiptBuilder())
			}
		}
	}
	finalizeCancel()
	if err != nil {
		settlement.ErrorCode = resultFinalizationFailed
		if errors.Is(err, control.ErrExposureBudgetExhausted) {
			settlement.ErrorCode = string(control.CodeExposureBudgetExhausted)
		}
		s.failQueryBudget(ctx, settlement)
		return nil, err
	}
	componentMS["encryption"] = durationMS(finalizeMetrics.Encryption)
	componentMS["settle_persist"] = durationMS(finalizeMetrics.SettlementStore)
	componentMS["receipt_signing"] = durationMS(finalizeMetrics.ReceiptSigning)
	if exposureContext != nil {
		componentMS["exposure_reservation_lock"] = durationMS(finalizeMetrics.ExposureReservationLock)
		componentMS["exposure_ledger_lock"] = durationMS(finalizeMetrics.ExposureLedgerLock)
		componentMS["exposure_fact_store"] = durationMS(finalizeMetrics.ExposureFactStore)
	}
	receipt, err := decodeReceiptJSON(persistedReceipt.ReceiptJSON)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"task_id": task.ID, "query_id": queryID, "request_id": requestID, "status": record.Status,
		"rows": stored.Rows, "row_count": stored.RowCount, "database_ms": stored.DatabaseMS,
		"component_ms": componentMS, "limited": stored.Limited, "receipt": receipt,
	}
	finalizePipelineResponse(result, stored, pipeline, time.Now(), time.Now(), time.Now())
	if finalizeMetrics.OutcomeRadix.CandidateCardinality > 0 {
		result["outcome_radix"] = finalizeMetrics.OutcomeRadix
	}
	if err := addStoredResponseMetadata(result, stored); err != nil {
		return nil, err
	}
	if charge, exposureErr := s.store.GetExposureCharge(ctx, record.ID); exposureErr == nil {
		result["exposure"] = charge
		if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
			result["exposure_budget"] = ledger
		}
	}
	return result, nil
}

func (s *Service) replayQueryIfPresent(ctx context.Context, taskID, requestID, requestDigest string) (any, bool, error) {
	return s.replayQueryIfPresentAt(ctx, taskID, requestID, requestDigest, time.Now())
}

func (s *Service) replayQueryIfPresentAt(ctx context.Context, taskID, requestID, requestDigest string, requestStarted time.Time) (any, bool, error) {
	existing, err := s.store.GetQueryByRequestID(ctx, taskID, requestID)
	if errors.Is(err, control.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if existing.RequestDigest != requestDigest {
		return nil, true, toolError(control.ErrIdempotencyConflict)
	}
	replayed, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
	return replayed, true, err
}

// replayPreparationFailure closes the idempotency window between the public
// entrypoint's first request-id lookup and semantic View preparation. A
// concurrent exact request may have committed a durable terminal outcome just
// before preparation observes REQUIRE_REBIND or a new registry revision; that
// outcome takes precedence over the newly observed preparation failure.
func (s *Service) replayPreparationFailure(ctx context.Context, taskID, requestID, requestDigest string, preparationErr error) (any, error) {
	return s.replayPreparationFailureAt(ctx, taskID, requestID, requestDigest, time.Now(), preparationErr)
}

func (s *Service) replayPreparationFailureAt(ctx context.Context, taskID, requestID, requestDigest string, requestStarted time.Time, preparationErr error) (any, error) {
	replayed, found, err := s.replayQueryIfPresentAt(ctx, taskID, requestID, requestDigest, requestStarted)
	if found || err != nil {
		return replayed, err
	}
	return nil, preparationErr
}

func validateRequestID(requestID string) error {
	if requestID == "" || len(requestID) > 128 || strings.TrimSpace(requestID) != requestID {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "request_id 必须为 1 到 128 个非空白边界字符"}
	}
	for _, value := range requestID {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || strings.ContainsRune("._:-", value) {
			continue
		}
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "request_id 仅支持字母、数字、点、下划线、冒号和连字符"}
	}
	return nil
}

func (s *Service) queryReplayResponse(ctx context.Context, record control.QueryRecord) (map[string]any, error) {
	now := time.Now()
	return s.queryReplayResponseAt(ctx, record, now, now)
}

func (s *Service) queryReplayResponseAt(ctx context.Context, record control.QueryRecord, requestStarted, prepareFinished time.Time) (map[string]any, error) {
	if requestStarted.IsZero() {
		requestStarted = time.Now()
	}
	if prepareFinished.Before(requestStarted) {
		prepareFinished = requestStarted
	}
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"task_id": record.TaskID, "query_id": record.ID, "request_id": record.RequestID,
		"status": record.Status, "receipt": receipt, "idempotent_replay": true,
	}
	defer func() {
		responseFinished := time.Now()
		result["pipeline_ms"] = map[string]float64{"prepare": durationMS(prepareFinished.Sub(requestStarted)), "execute_and_derive": 0, "artifact_stage": 0,
			"control_settlement": 0, "artifact_publication": 0, "response_finalize": durationMS(responseFinished.Sub(prepareFinished)), "server_total": durationMS(responseFinished.Sub(requestStarted))}
	}()
	if charge, exposureErr := s.store.GetExposureCharge(ctx, record.ID); exposureErr == nil {
		result["exposure"] = charge
		if ledger, ledgerErr := s.store.GetExposureLedger(ctx, record.TaskID); ledgerErr == nil {
			result["exposure_budget"] = ledger
		}
	}
	if record.Status != control.QueryCompleted || record.ResultSHA256 == "" {
		return result, nil
	}
	if s.resultArtifacts != nil {
		artifact, artifactErr := s.store.GetResultArtifactByQuery(ctx, record.ID)
		if artifactErr == nil {
			if artifact.ExpiresAt != nil && !s.clock().UTC().Before(*artifact.ExpiresAt) {
				return result, nil
			}
			if artifact.Status == control.ResultArtifactPending {
				promoteCtx, cancel := s.artifactOperationContext(ctx)
				artifact, artifactErr = s.promoteResultArtifact(promoteCtx, artifact, "gateway-replay")
				cancel()
			}
			if artifactErr != nil {
				return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "结果已结算，规范 Parquet 正在恢复；请稍后重试"}
			}
			if artifact.Status != control.ResultArtifactAvailable && artifact.Status != control.ResultArtifactDeleting &&
				artifact.Status != control.ResultArtifactDeleted {
				return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "结果已结算，规范 Parquet 正在恢复；请稍后重试"}
			}
			stored, _, decodeErr := decodeArtifactMetadata(artifact)
			if decodeErr != nil {
				return nil, decodeErr
			}
			for key, value := range publicArtifact(artifact, stored) {
				result[key] = value
			}
			return result, nil
		}
		if !errors.Is(artifactErr, control.ErrNotFound) {
			return nil, artifactErr
		}
		// Historical PostgreSQL-ciphertext rows predate object artifacts. Once
		// object-backed mode is enabled they remain auditable, but ordinary Agent
		// replay returns metadata only instead of re-releasing the full row set.
		result["row_count"] = record.ResultRows
		result["database_ms"] = record.ResultObservedDBMS
		result["legacy_result"] = true
		return result, nil
	}
	_, plaintext, err := s.store.GetEncryptedResult(ctx, record.TaskID, record.ID)
	if err != nil {
		// The query status remains the durable answer for a result that could not
		// be encoded or atomically stored after execution.
		return result, nil
	}
	stored, err := decodeStoredQueryResult(plaintext)
	if err != nil {
		return nil, err
	}
	result["rows"] = stored.Rows
	result["row_count"] = stored.RowCount
	result["database_ms"] = stored.DatabaseMS
	result["component_ms"] = stored.ComponentMS
	if stored.DiagnosticMS != nil {
		result["diagnostic_ms"] = stored.DiagnosticMS
	}
	result["limited"] = stored.Limited
	if err := addStoredResponseMetadata(result, stored); err != nil {
		return nil, err
	}
	return result, nil
}

func durationMS(value time.Duration) float64 {
	if value <= 0 {
		return 0
	}
	return float64(value.Nanoseconds()) / float64(time.Millisecond)
}

func finalizePipelineResponse(response map[string]any, stored storedQueryResult, measurement *queryPipelineMeasurement,
	artifactStageFinished, settlementFinished, publicationFinished time.Time) {
	if response == nil || measurement == nil || measurement.requestStarted.IsZero() {
		return
	}
	if measurement.prepareFinished.IsZero() {
		measurement.prepareFinished = measurement.requestStarted
	}
	if measurement.executeFinished.IsZero() {
		measurement.executeFinished = measurement.prepareFinished
	}
	if artifactStageFinished.Before(measurement.executeFinished) {
		artifactStageFinished = measurement.executeFinished
	}
	if settlementFinished.Before(artifactStageFinished) {
		settlementFinished = artifactStageFinished
	}
	if publicationFinished.Before(settlementFinished) {
		publicationFinished = settlementFinished
	}
	responseFinished := time.Now()
	pipeline := map[string]float64{
		"prepare":              durationMS(measurement.prepareFinished.Sub(measurement.requestStarted)),
		"execute_and_derive":   durationMS(measurement.executeFinished.Sub(measurement.prepareFinished)),
		"artifact_stage":       durationMS(artifactStageFinished.Sub(measurement.executeFinished)),
		"control_settlement":   durationMS(settlementFinished.Sub(artifactStageFinished)),
		"artifact_publication": durationMS(publicationFinished.Sub(settlementFinished)),
		"response_finalize":    durationMS(responseFinished.Sub(publicationFinished)),
		"server_total":         durationMS(responseFinished.Sub(measurement.requestStarted)),
	}
	response["pipeline_ms"] = pipeline
	if stored.DiagnosticMS != nil {
		response["diagnostic_ms"] = stored.DiagnosticMS
	}
}

// recordOrdinalTimingComponents records both the independently measured leaf
// timers and the two useful aggregates. The leaf equations are intentionally
// exposed so an evaluation consumer can reject placeholder or double-counted
// component evidence:
//
//	ordinal_stream = provenance_postgresql + ordinal_stream_consumer
//	bitmap_derivation = ordinal_visible_preparation + ordinal_stream_consumer + ordinal_finish
//
// provenance_postgresql is populated separately after connector execution.
func recordOrdinalTimingComponents(componentMS map[string]float64, stream, consumer, preparation, finish time.Duration) {
	componentMS["ordinal_stream"] = durationMS(stream)
	componentMS["ordinal_stream_consumer"] = durationMS(consumer)
	componentMS["ordinal_visible_preparation"] = durationMS(preparation)
	componentMS["ordinal_finish"] = durationMS(finish)
	componentMS["bitmap_derivation"] = durationMS(preparation + consumer + finish)
}

func connectorOverheadDuration(elapsed, database, separatelyReportedConsumer time.Duration) time.Duration {
	result := elapsed - database - separatelyReportedConsumer
	if result < 0 {
		return 0
	}
	return result
}

func querySettlement(queryID string, data dataconnector.Result, started time.Time, reservation control.BudgetReservation) control.BudgetSettlement {
	rows := data.RowCount
	if rows < 0 {
		rows = 0
	}
	if rows > reservation.AllowedRows {
		rows = reservation.AllowedRows
	}
	databaseMS := data.DatabaseTime.Milliseconds()
	if data.DatabaseTime <= 0 {
		databaseMS = time.Since(started).Milliseconds()
	}
	if databaseMS < 1 {
		databaseMS = 1
	}
	return control.BudgetSettlement{
		QueryID:      queryID,
		Rows:         rows,
		DBMS:         min64(databaseMS, reservation.AllowedDBMS),
		ObservedDBMS: databaseMS,
	}
}

func (s *Service) failQueryBudget(ctx context.Context, settlement control.BudgetSettlement) {
	// OrdinalMaterialization is a proposed success-path publication. A failed
	// query must neither publish it nor let it poison the terminal failure
	// settlement: FailBudgetWithReceipt deliberately uses the ordinary terminal
	// path, which rejects success-only materialization requests. Keep the caller's
	// value untouched and retain the sanitized copy for every background retry.
	failure := settlement
	failure.OrdinalMaterialization = nil
	// The execution binding is dropped for the same reason, and it is not merely
	// tidiness: the Control Store refuses to write one for a query that did not
	// complete, so a failure settlement that carried it would be rejected whole
	// and retried forever. The refusal is right -- a FAILED query cannot prove
	// which of its targets ran -- and the caller's binding was built before the
	// failure was known.
	failure.ExecutionBinding = nil
	failCtx, cancel := s.detachedContext(ctx)
	_, _, err := s.store.FailBudgetWithReceipt(failCtx, failure, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	// The finalize transaction may have committed even when COMMIT returned a
	// transport error. A durable terminal record wins and must not create an
	// infinite FAILED-settlement retry that holds readiness down forever.
	stateCtx, stateCancel := s.detachedContext(ctx)
	durable, stateErr := s.store.GetQuery(stateCtx, settlement.QueryID)
	stateCancel()
	if stateErr == nil && durable.Status != control.QueryReserved {
		return
	}
	s.logger.Error("fail query budget", "trace_id", mcp.TraceID(ctx), "query_id", settlement.QueryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryFailedQuerySettlement(failure)
}

func (s *Service) releaseQueryBudget(ctx context.Context, queryID, errorCode string) {
	releaseCtx, cancel := s.detachedContext(ctx)
	_, _, err := s.store.ReleaseBudgetWithReceipt(releaseCtx, queryID, errorCode, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	s.logger.Error("release query budget", "trace_id", mcp.TraceID(ctx), "query_id", queryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryTerminalQuery(queryID, errorCode, false)
}

func (s *Service) markQueryIndeterminate(ctx context.Context, queryID, errorCode string) {
	markCtx, cancel := s.detachedContext(ctx)
	_, _, err := s.store.MarkIndeterminateWithReceipt(markCtx, queryID, errorCode, s.terminalReceiptBuilder())
	cancel()
	if err == nil {
		return
	}
	s.logger.Error("mark query indeterminate", "trace_id", mcp.TraceID(ctx), "query_id", queryID, "error", err)
	if errors.Is(err, control.ErrClosed) || s.background.Err() != nil {
		return
	}
	s.pendingSettles.Add(1)
	go s.retryTerminalQuery(queryID, errorCode, true)
}

func (s *Service) retryTerminalQuery(queryID, errorCode string, indeterminate bool) {
	defer s.pendingSettles.Add(-1)
	delay := settlementRetryDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.background.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		attemptCtx, cancel := context.WithTimeout(s.background, s.settlementTimeout)
		var err error
		if indeterminate {
			_, _, err = s.store.MarkIndeterminateWithReceipt(attemptCtx, queryID, errorCode, s.terminalReceiptBuilder())
		} else {
			_, _, err = s.store.ReleaseBudgetWithReceipt(attemptCtx, queryID, errorCode, s.terminalReceiptBuilder())
		}
		cancel()
		if err == nil || errors.Is(err, control.ErrClosed) {
			return
		}
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (s *Service) retryFailedQuerySettlement(settlement control.BudgetSettlement) {
	defer s.pendingSettles.Add(-1)
	delay := settlementRetryDelay
	for {
		timer := time.NewTimer(delay)
		select {
		case <-s.background.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		attemptCtx, cancel := context.WithTimeout(s.background, s.settlementTimeout)
		durable, stateErr := s.store.GetQuery(attemptCtx, settlement.QueryID)
		if stateErr == nil && durable.Status != control.QueryReserved {
			cancel()
			return
		}
		_, _, err := s.store.FailBudgetWithReceipt(attemptCtx, settlement, s.terminalReceiptBuilder())
		cancel()
		if err == nil || errors.Is(err, control.ErrClosed) {
			return
		}
		s.logger.Error("retry failed query budget settlement", "query_id", settlement.QueryID, "error", err)
		if delay < time.Second {
			delay *= 2
			if delay > time.Second {
				delay = time.Second
			}
		}
	}
}

func (s *Service) policyGrant(grant control.TaskGrant) (sqlpolicy.Grant, error) {
	var scope map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
		return sqlpolicy.Grant{}, err
	}
	result := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(grant.ApprovedProducts))}
	for _, name := range grant.ApprovedProducts {
		product, ok := s.catalog.LookupProduct(name)
		if !ok {
			return sqlpolicy.Grant{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务引用了当前目录中不存在的数据产品"}
		}
		policyProduct, err := catalogPolicyProductGrant(product, grant.ApprovedColumns[name], scope)
		if err != nil {
			return sqlpolicy.Grant{}, err
		}
		result.Products = append(result.Products, policyProduct)
	}
	return result, nil
}

func catalogPolicyProductGrant(product catalog.Product, approvedColumns []string, scope map[string]any) (sqlpolicy.ProductGrant, error) {
	parts := strings.Split(product.ReportingView, ".")
	if len(parts) != 2 {
		return sqlpolicy.ProductGrant{}, errors.New("validated catalog has invalid reporting view")
	}
	for _, scopeName := range product.Scopes {
		if _, present := scope[scopeName]; !present {
			return sqlpolicy.ProductGrant{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务授权缺少目录要求的强制数据范围"}
		}
	}
	predicates, err := scopePredicates(product, scope)
	if err != nil {
		return sqlpolicy.ProductGrant{}, err
	}
	return sqlpolicy.ProductGrant{
		LogicalName: product.Name, PhysicalSchema: parts[0], PhysicalView: parts[1],
		ApprovedColumns:   append([]string(nil), approvedColumns...),
		AllowedFunctions:  append([]string(nil), product.AllowedFunctions...),
		AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
		AllowedOperators:  append([]string(nil), product.AllowedOperators...),
		MandatoryScope:    predicates,
	}, nil
}

func scopePredicates(product catalog.Product, scope map[string]any) ([]sqlpolicy.ScopePredicate, error) {
	fieldSet := make(map[string]struct{}, len(product.Fields))
	for _, field := range product.Fields {
		fieldSet[field.Name] = struct{}{}
	}
	names := make([]string, 0, len(scope))
	for name := range scope {
		names = append(names, name)
	}
	sort.Strings(names)
	var predicates []sqlpolicy.ScopePredicate
	for _, name := range names {
		if _, relevant := fieldSet[name]; !relevant {
			continue
		}
		switch value := scope[name].(type) {
		case string:
			predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeEqual, Values: []string{value}})
		case []any:
			values := make([]string, 0, len(value))
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					return nil, errors.New("invalid stored enum scope")
				}
				values = append(values, text)
			}
			operator := sqlpolicy.ScopeIn
			if len(values) == 1 {
				operator = sqlpolicy.ScopeEqual
			}
			predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: operator, Values: values})
		case map[string]any:
			if from, _ := value["from"].(string); from != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeGreaterEqual, Values: []string{from}})
			}
			if to, _ := value["to"].(string); to != "" {
				predicates = append(predicates, sqlpolicy.ScopePredicate{Column: name, Operator: sqlpolicy.ScopeLessEqual, Values: []string{to}})
			}
		default:
			return nil, fmt.Errorf("invalid stored scope value")
		}
	}
	return predicates, nil
}

func (s *Service) getQueryResult(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID  string `json:"task_id"`
		QueryID string `json:"query_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.ownedTask(ctx, principal, args.TaskID); err != nil {
		return nil, err
	}
	record, err := s.store.GetQuery(ctx, args.QueryID)
	if err != nil || record.TaskID != args.TaskID || record.Actor != principal.Subject {
		return nil, notFound()
	}
	if s.resultArtifacts != nil {
		artifact, artifactErr := s.store.GetResultArtifactByQuery(ctx, args.QueryID)
		if artifactErr == nil {
			if artifact.ExpiresAt != nil && !s.clock().UTC().Before(*artifact.ExpiresAt) {
				return nil, notFound()
			}
			if artifact.Status == control.ResultArtifactPending {
				promoteCtx, cancel := s.artifactOperationContext(ctx)
				artifact, artifactErr = s.promoteResultArtifact(promoteCtx, artifact, "gateway-result-read")
				cancel()
			}
			if artifactErr != nil {
				return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "规范 Parquet 正在恢复；请稍后重试"}
			}
			if artifact.Status != control.ResultArtifactAvailable && artifact.Status != control.ResultArtifactDeleting &&
				artifact.Status != control.ResultArtifactDeleted {
				return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "规范 Parquet 正在恢复；请稍后重试"}
			}
			stored, _, decodeErr := decodeArtifactMetadata(artifact)
			if decodeErr != nil {
				return nil, decodeErr
			}
			receipt, receiptErr := s.queryReceipt(ctx, record)
			if receiptErr != nil {
				return nil, receiptErr
			}
			response := publicArtifact(artifact, stored)
			response["status"] = record.Status
			response["receipt"] = receipt
			return response, nil
		}
		if !errors.Is(artifactErr, control.ErrNotFound) {
			return nil, artifactErr
		}
		receipt, receiptErr := s.queryReceipt(ctx, record)
		if receiptErr != nil {
			return nil, receiptErr
		}
		return map[string]any{
			"task_id": args.TaskID, "query_id": args.QueryID, "status": record.Status,
			"row_count": record.ResultRows, "database_ms": record.ResultObservedDBMS,
			"legacy_result": true, "receipt": receipt,
		}, nil
	}
	_, plaintext, err := s.store.GetEncryptedResult(ctx, args.TaskID, args.QueryID)
	if err != nil {
		return nil, err
	}
	result, err := decodeStoredQueryResult(plaintext)
	if err != nil {
		return nil, err
	}
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	response := map[string]any{
		"task_id": args.TaskID, "query_id": args.QueryID,
		"row_count": result.RowCount, "database_ms": result.DatabaseMS,
		"limited": result.Limited, "receipt": receipt,
	}
	if err := addStoredResponseMetadata(response, result); err != nil {
		return nil, err
	}
	return response, nil
}

func (s *Service) listReceipts(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Cursor string `json:"cursor,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if _, err := s.ownedTask(ctx, principal, args.TaskID); err != nil {
		return nil, err
	}
	page, err := s.store.ListQueriesPage(ctx, args.TaskID, args.Cursor, 100)
	if err != nil {
		return nil, err
	}
	receipts := make([]map[string]any, 0, len(page.Records))
	for _, record := range page.Records {
		receipt, err := s.queryReceipt(ctx, record)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return map[string]any{"task_id": args.TaskID, "receipts": receipts, "next_cursor": page.NextCursor}, nil
}

func (s *Service) listAuditEvents(ctx context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		TaskID    string `json:"task_id,omitempty"`
		Actor     string `json:"actor,omitempty"`
		EventType string `json:"event_type,omitempty"`
		Cursor    string `json:"cursor,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	var after int64
	if args.Cursor != "" {
		value, err := strconv.ParseInt(args.Cursor, 10, 64)
		if err != nil || value < 0 {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "cursor 无效"}
		}
		after = value
	}
	events, err := s.store.ListAuditEvents(ctx, control.AuditFilter{TaskID: args.TaskID, Actor: args.Actor, EventType: args.EventType, After: after, Limit: 100})
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(events))
	for _, event := range events {
		items = append(items, publicAuditEvent(event, true))
	}
	next := ""
	if len(events) == 100 {
		next = strconv.FormatInt(events[len(events)-1].Sequence, 10)
	}
	return map[string]any{"events": items, "next_cursor": next}, nil
}

func (s *Service) getAuditReceipt(ctx context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		ReceiptID string `json:"receipt_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	record, err := s.store.GetQuery(ctx, args.ReceiptID)
	if err != nil {
		return nil, err
	}
	evidence, err := s.store.GetQueryReceipt(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	events, err := s.store.ListAuditEventsForQuery(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	chain := make([]map[string]any, 0, len(events))
	for _, event := range events {
		chain = append(chain, publicAuditEvent(event, true))
	}
	receipt, err := s.queryReceipt(ctx, record)
	if err != nil {
		return nil, err
	}
	publicProof, typedProof, err := s.auditInclusionProof(ctx, evidence.Audit)
	if err != nil {
		return nil, err
	}
	signed, err := decodeSignedQueryReceipt(receipt)
	if err != nil {
		return nil, err
	}
	if err := queryreceipt.VerifyAuditInclusion(signed, typedProof); err != nil {
		return nil, err
	}
	result := map[string]any{"receipt": receipt, "audit_chain_events": chain, "audit_inclusion": publicProof}
	// The inclusion proofs belong to receipts that registered a result object.
	// This was a comparison against a receipt version, which was wrong in both
	// directions: an equality made a later receipt skip the proofs even though it
	// carried the same artifact intent, and a range would have demanded a
	// registration proof from an inline operation that registers no result object.
	// The signed delivery mode answers it, and answers it per operation.
	if signed.RequiresArtifactInclusionProofs() {
		if evidence.ArtifactRegistrationAudit == nil {
			return nil, fmt.Errorf("an artifact-delivery receipt is missing artifact registration audit evidence")
		}
		registrationPublicProof, registrationTypedProof, err := s.auditInclusionProof(ctx, *evidence.ArtifactRegistrationAudit)
		if err != nil {
			return nil, err
		}
		if err := queryreceipt.VerifyArtifactIntentInclusion(signed, typedProof, registrationTypedProof); err != nil {
			return nil, err
		}
		result["artifact_intent_inclusion"] = map[string]any{
			"terminal": publicProof, "registration": registrationPublicProof,
		}
		availabilityProof, err := s.artifactAvailabilityInclusion(ctx, signed, events)
		if err != nil {
			return nil, err
		}
		if availabilityProof != nil {
			result["availability_event_inclusion"] = availabilityProof
		}
	}
	return result, nil
}

// artifactAvailabilityInclusion gives auditors a direct, verified inclusion
// proof for the post-settlement event that makes an artifact logically
// AVAILABLE. PENDING crash-window receipts correctly have no such proof yet.
func (s *Service) artifactAvailabilityInclusion(ctx context.Context, receipt queryreceipt.QueryReceiptV1,
	events []control.AuditEvent) (map[string]any, error) {
	var availability *control.AuditEvent
	for index := range events {
		if events[index].EventType != "QUERY_RESULT_CONSUMED" {
			continue
		}
		if availability != nil {
			return nil, fmt.Errorf("query %s has multiple artifact availability events", receipt.QueryID)
		}
		availability = &events[index]
	}
	if availability == nil {
		return nil, nil
	}
	publicProof, typedProof, err := s.auditInclusionProof(ctx, *availability)
	if err != nil {
		return nil, err
	}
	if err := queryreceipt.VerifyArtifactAvailabilityInclusion(receipt, typedProof); err != nil {
		return nil, err
	}
	return publicProof, nil
}

func publicAuditEvent(event control.AuditEvent, includePayload bool) map[string]any {
	result := map[string]any{
		"sequence": event.Sequence, "event_id": event.EventID, "task_id": event.TaskID,
		"query_id": event.QueryID, "actor": event.Actor, "event_type": event.EventType,
		"occurred_at": event.OccurredAt, "previous_hash": event.PreviousHash, "current_hash": event.CurrentHash,
	}
	if includePayload {
		var payload any
		_ = json.Unmarshal(event.Payload, &payload)
		result["payload"] = payload
	}
	return result
}

func (s *Service) auditInclusionProof(ctx context.Context, terminal control.AuditEvent) (map[string]any, auditchain.InclusionProof, error) {
	checkpoint, err := s.store.AuditCheckpoint(ctx)
	if err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	proof := auditchain.InclusionProof{TerminalEvent: terminal, Checkpoint: checkpoint}
	var predecessor any
	if terminal.Sequence > 1 {
		event, err := s.store.GetAuditEvent(ctx, terminal.Sequence-1)
		if err != nil && !errors.Is(err, control.ErrNotFound) {
			return nil, auditchain.InclusionProof{}, err
		}
		if err == nil {
			proof.PredecessorEvent = &event
			predecessor = publicAuditEvent(event, true)
		}
	}
	successors, err := s.store.ListAuditEventsRange(ctx, terminal.Sequence, checkpoint.Sequence)
	if err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	proof.SuccessorEvents = successors
	publicSuccessors := make([]map[string]any, 0, len(successors))
	for _, event := range successors {
		publicSuccessors = append(publicSuccessors, publicAuditEvent(event, true))
	}
	if err := auditchain.VerifyInclusion(proof); err != nil {
		return nil, auditchain.InclusionProof{}, err
	}
	return map[string]any{
		"terminal_event":    publicAuditEvent(terminal, true),
		"predecessor_event": predecessor,
		"successor_events":  publicSuccessors,
		"checkpoint": map[string]any{
			"sequence": checkpoint.Sequence,
			"hash":     checkpoint.Hash,
		},
	}, proof, nil
}

func (s *Service) queryReceipt(ctx context.Context, record control.QueryRecord) (map[string]any, error) {
	if record.Status == control.QueryReserved {
		return unsignedPublicReceipt(record), nil
	}
	evidence, err := s.store.GetQueryReceipt(ctx, record.ID)
	if err != nil {
		return nil, err
	}
	record = evidence.Query
	if evidence.Receipt != nil {
		return decodeReceiptJSON(evidence.Receipt.ReceiptJSON)
	}
	if record.BudgetAfter == nil || record.CompletedAt == nil {
		return nil, fmt.Errorf("terminal query is missing durable budget or timestamp evidence")
	}
	// This is a recovery attestation, not the ordinary co-committed receipt
	// path. It signs already immutable terminal and registration audit evidence;
	// consequently its signed_at may be later than the original settlement.
	request, err := s.buildQueryReceiptRequest(control.QueryReceipt{
		Query: record, Audit: evidence.Audit, Exposure: evidence.Exposure,
		Artifact: evidence.Artifact, ArtifactRegistrationAudit: evidence.ArtifactRegistrationAudit,
		// The execution binding is part of the evidence being recovered, not
		// something the recovery may leave behind. Reassembling the struct member
		// by member is how it came to be dropped: the loaded evidence carried the
		// binding and this literal did not name it, so a recovered receipt
		// described no execution while the settled one it replaced did.
		ExecutionBinding: evidence.ExecutionBinding,
	}, s.clock().UTC())
	if err != nil {
		return nil, err
	}
	persisted, err := s.store.SaveQueryReceipt(ctx, request)
	if err != nil {
		return nil, err
	}
	return decodeReceiptJSON(persisted.ReceiptJSON)
}

func (s *Service) terminalReceiptBuilder() control.TerminalReceiptBuilder {
	return func(evidence control.QueryReceipt) (control.SaveQueryReceiptRequest, error) {
		return BuildQueryReceiptRequest(evidence, s.queryReceiptSigner, s.clock().UTC())
	}
}

func (s *Service) buildQueryReceiptRequest(evidence control.QueryReceipt, signedAt time.Time) (control.SaveQueryReceiptRequest, error) {
	return BuildQueryReceiptRequest(evidence, s.queryReceiptSigner, signedAt)
}

// BuildQueryReceiptRequest signs terminal query evidence and returns an
// immutable Control PG receipt row. Normal settlement callers persist it in
// the evidence transaction; queryReceipt also uses it later to recover a
// missing attestation over already immutable audit evidence.
func BuildQueryReceiptRequest(evidence control.QueryReceipt, signer *queryreceipt.Signer, signedAt time.Time) (control.SaveQueryReceiptRequest, error) {
	record := evidence.Query
	if record.BudgetAfter == nil || record.CompletedAt == nil {
		return control.SaveQueryReceiptRequest{}, fmt.Errorf("terminal query is missing durable budget or timestamp evidence")
	}
	signedAt = signedAt.UTC()
	if signedAt.Before(record.CompletedAt.UTC()) {
		// Preserve causal receipt ordering even if the host wall clock is
		// adjusted backwards between settlement and signing.
		signedAt = record.CompletedAt.UTC()
	}
	// The exposure profile is an experiment condition, not a receipt version.
	//
	// This used to choose a receipt version per profile -- V4 for the profiles
	// without outcomes, V5 for v3, V6 for v4, V7 for v5 -- which made the
	// accounting mechanism under test and the evidence format one variable. They
	// are separate variables: every profile now signs the same receipt, and which
	// one accounted the operation is stated inside the signed exposure block.
	var exposureEvidence *queryreceipt.ExposureEvidenceV1
	var artifactIntent *queryreceipt.ArtifactIntentEvidenceV1
	if evidence.Exposure != nil {
		exposureEvidence = &queryreceipt.ExposureEvidenceV1{
			RootTaskID: evidence.Exposure.RootTaskID, ProfileVersion: evidence.Exposure.ProfileVersion,
			ActualReleaseFacts: evidence.Exposure.ActualReleaseFacts, ActualInfluenceFacts: evidence.Exposure.ActualInfluenceFacts,
			ActualOutcomeFacts:  evidence.Exposure.ActualOutcomeFacts,
			ChargedReleaseFacts: evidence.Exposure.ChargedReleaseFacts, ChargedInfluenceFacts: evidence.Exposure.ChargedInfluenceFacts,
			ChargedOutcomeFacts: evidence.Exposure.ChargedOutcomeFacts,
			ObservationSHA256:   evidence.Exposure.ObservationSHA256,
			DictionarySetSHA256: evidence.Exposure.DictionarySetDigest,
			ReleaseSetSHA256:    evidence.Exposure.ReleaseSetSHA256,
			InfluenceSetSHA256:  evidence.Exposure.InfluenceSetSHA256,
			OutcomeSetSHA256:    evidence.Exposure.OutcomeSetSHA256,
			RootEpoch:           evidence.Exposure.RootEpoch,
		}
		if evidence.Exposure.ProfileVersion == exposure.ProfileV5 {
			exposureEvidence.PredicateProfileVersion = exposure.PredicateFootprintVersion
			exposureEvidence.PredicateContextSHA256 = evidence.Exposure.PredicateContextSHA256
			exposureEvidence.PredicateSetSHA256 = evidence.Exposure.PredicateSetSHA256
			exposureEvidence.ActualPredicateAtomCount = evidence.Exposure.ActualPredicateAtomCount
			exposureEvidence.ChargedPredicateAtomCount = evidence.Exposure.ChargedPredicateAtomCount
			exposureEvidence.CompositeOutcomeSHA256 = evidence.Exposure.CompositeOutcomeSHA256
			exposureEvidence.ActualCompositeCount = 1
			exposureEvidence.ChargedCompositeCount = evidence.Exposure.ChargedOutcomeFacts - evidence.Exposure.ChargedPredicateAtomCount
		}
	}
	if (evidence.Artifact == nil) != (evidence.ArtifactRegistrationAudit == nil) {
		return control.SaveQueryReceiptRequest{}, fmt.Errorf("artifact and registration audit evidence must be provided together")
	}
	// Whether a result object was registered is a property of the operation, not
	// of the accounting profile it settled under. This used to also require
	// exposure v5, which was the same profile/format conflation the receipt ladder
	// was: an artifact-enabled deployment on any other profile registered a result
	// object the receipt then described as an inline delivery.
	if evidence.Artifact != nil {
		artifact := evidence.Artifact
		registration := evidence.ArtifactRegistrationAudit
		intent, err := queryreceipt.BuildArtifactIntent(queryreceipt.ArtifactIntentEvidenceV1{
			Version: queryreceipt.ArtifactIntentVersionV1, ResultID: artifact.ResultID,
			Format: artifact.Format, Encryption: artifact.Encryption, KeyID: artifact.KeyID,
			ParquetSHA256: artifact.ParquetSHA256, ObjectSHA256: artifact.ObjectSHA256,
			ParquetSize: artifact.ParquetSize, ObjectSize: artifact.ObjectSize,
			RowCount: artifact.RowCount, ColumnCount: int64(artifact.ColumnCount),
			SchemaSHA256:         digest(string(artifact.SchemaJSON)),
			ResultMetadataSHA256: digest(string(artifact.ResultMetadataJSON)),
			ACLSHA256:            digest(string(artifact.ACLJSON)),
			ObjectKeySHA256:      digest(artifact.ObjectKey), StagingKeySHA256: digest(artifact.StagingKey),
			ExpiresAt: artifact.ExpiresAt, Status: queryreceipt.ArtifactStatusPending,
			RegistrationAuditSequence:     registration.Sequence,
			RegistrationPreviousAuditHash: registration.PreviousHash,
			RegistrationAuditHash:         registration.CurrentHash,
		})
		if err != nil {
			return control.SaveQueryReceiptRequest{}, err
		}
		artifactIntent = &intent
	}
	// The execution evidence, and the delivery mode that says what came back.
	//
	// The binding comes from the row loaded inside this transaction, never from a
	// caller's memory. A query with no binding carries none: the receipt must not
	// claim to describe an execution nothing recorded, and it does not have to,
	// because the absence is signed.
	var executionBindingV2 *querybinding.QueryExecutionBindingV2
	var exposureLedgerBefore *querybinding.ExposureLedgerBeforeV1
	if evidence.ExecutionBinding != nil {
		if err := evidence.ExecutionBinding.Validate(); err != nil {
			return control.SaveQueryReceiptRequest{}, fmt.Errorf("persisted execution binding: %w", err)
		}
		exposureLedgerBefore = evidence.ExecutionBinding.ExposureLedgerBefore
		executionBindingV2 = evidence.ExecutionBinding.BindingV2
	}
	// The delivery mode is read from what the settlement actually did -- whether a
	// result object was registered -- and is cross-bound to the terminal status by
	// the receipt, so it cannot describe a delivery the settlement did not
	// perform.
	deliveryMode := queryreceipt.DeliveryNone
	if record.Status == control.QueryCompleted {
		deliveryMode = queryreceipt.DeliveryInline
		if artifactIntent != nil {
			deliveryMode = queryreceipt.DeliveryArtifact
		}
	}

	receipt := queryreceipt.QueryReceiptV1{
		Version: queryreceipt.Version, ReceiptID: record.ID,
		TaskID: record.TaskID, QueryID: record.ID, RequestID: record.RequestID,
		ManifestDigest: record.ManifestDigest, GrantDigest: record.GrantDigest,
		CatalogDigest: record.CatalogDigest, CatalogVersion: record.CatalogVersion,
		DatasourceID: record.DatasourceID, SchemaDigest: record.SchemaDigest,
		RequestDigest: record.RequestDigest, SQLFingerprint: record.SQLFingerprint,
		PolicyDecision: record.PolicyDecision, BudgetBefore: queryReceiptBudget(record.BudgetBefore),
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: record.ReservedRows, DBMS: record.ReservedDBMS},
		BudgetCharged: queryreceipt.BudgetVectorV1{
			Queries: record.ChargedQueries, Rows: record.ChargedRows, DBMS: record.ChargedDBMS,
		},
		BudgetAfter: queryReceiptBudget(*record.BudgetAfter), RowCount: record.ResultRows,
		DatabaseMS: record.ResultDBMS, ResultHash: record.ResultSHA256,
		Status: string(record.Status), ErrorCode: record.ErrorCode,
		CreatedAt: record.CreatedAt, CompletedAt: *record.CompletedAt,
		AuditSequence: evidence.Audit.Sequence, PreviousAuditHash: evidence.Audit.PreviousHash,
		AuditHash: evidence.Audit.CurrentHash, SignedAt: &signedAt,
		Exposure: exposureEvidence, ArtifactIntent: artifactIntent,
		ResultDeliveryMode:   deliveryMode,
		ExposureLedgerBefore: exposureLedgerBefore,
		ExecutionBindingV2:   executionBindingV2,
	}
	signed, err := signer.Sign(receipt)
	if err != nil {
		return control.SaveQueryReceiptRequest{}, err
	}
	encoded, err := approval.CanonicalJSON(signed)
	if err != nil {
		return control.SaveQueryReceiptRequest{}, err
	}
	return control.SaveQueryReceiptRequest{
		QueryID: record.ID, Version: signed.Version, GatewayKeyID: signed.GatewayKeyID,
		Signature: signed.Signature, SignedAt: signedAt,
		TerminalAuditSequence: evidence.Audit.Sequence, TerminalAuditHash: evidence.Audit.CurrentHash,
		ReceiptJSON: encoded,
	}, nil
}

func decodeReceiptJSON(encoded []byte) (map[string]any, error) {
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeSignedQueryReceipt(value map[string]any) (queryreceipt.QueryReceiptV1, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return queryreceipt.QueryReceiptV1{}, err
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return queryreceipt.QueryReceiptV1{}, err
	}
	return receipt, nil
}

func queryReceiptBudget(snapshot control.BudgetSnapshot) queryreceipt.BudgetStateV1 {
	return queryreceipt.BudgetStateV1{
		Limits: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Limits.Queries, Rows: snapshot.Limits.Rows, DBMS: snapshot.Limits.DBMS,
		},
		Used: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Usage.UsedQueries, Rows: snapshot.Usage.UsedRows, DBMS: snapshot.Usage.UsedDBMS,
		},
		Reserved: queryreceipt.BudgetVectorV1{
			Queries: snapshot.Usage.ReservedQueries, Rows: snapshot.Usage.ReservedRows, DBMS: snapshot.Usage.ReservedDBMS,
		},
	}
}

func unsignedPublicReceipt(record control.QueryRecord) map[string]any {
	result := map[string]any{
		"receipt_id": record.ID, "task_id": record.TaskID, "query_id": record.ID,
		"request_id": record.RequestID, "actor": record.Actor,
		"request_digest": record.RequestDigest, "sql_fingerprint": record.SQLFingerprint,
		"catalog_version": record.CatalogVersion, "catalog_digest": record.CatalogDigest,
		"datasource_id": record.DatasourceID, "schema_digest": record.SchemaDigest,
		"manifest_digest": record.ManifestDigest, "grant_digest": record.GrantDigest,
		"policy_decision": record.PolicyDecision,
		"status":          record.Status, "result_rows": record.ResultRows, "database_ms": record.ResultDBMS,
		"budget_reserved": map[string]any{"queries": int64(1), "rows": record.ReservedRows, "db_ms": record.ReservedDBMS},
		"charged_queries": record.ChargedQueries, "charged_rows": record.ChargedRows,
		"charged_database_ms": record.ChargedDBMS, "result_sha256": record.ResultSHA256,
		"error_code": record.ErrorCode, "created_at": record.CreatedAt, "completed_at": record.CompletedAt,
		"budget_before": publicBudget(record.BudgetBefore),
	}
	if record.ViewBindingDigest != "" {
		result["view_binding_digest"] = record.ViewBindingDigest
	}
	if record.BudgetAfter != nil {
		result["budget_after"] = publicBudget(*record.BudgetAfter)
	}
	return result
}

func storedGrantMatchesProtocol(task control.Task, stored control.TaskGrant, protocol approval.TaskGrantV1) bool {
	core := protocol.Core
	if core.TaskID != task.ID || core.TaskID != stored.TaskID || core.AgentID != task.PrincipalID ||
		core.RootTaskID != lineageValue(task.RootTaskID, task.ParentTaskID) || core.ParentTaskID != task.ParentTaskID ||
		core.HumanSubject != stored.Subject || core.DeclaredObjective != task.Objective ||
		core.DeclaredObjective != stored.Purpose || string(core.SensitivityCeiling) != stored.SensitivityCeiling ||
		core.CatalogVersion != task.CatalogVersion || core.CatalogVersion != stored.CatalogVersion ||
		core.CatalogSHA256 != stored.CatalogDigest ||
		core.DatasourceID != stored.DatasourceID || core.SchemaDigest != stored.SchemaDigest ||
		core.ViewBindingDigest != stored.ViewBindingDigest ||
		!stored.ExpiresAt.Equal(core.ExpiresAt.UTC().Truncate(time.Microsecond)) ||
		stored.Budget != (control.BudgetLimits{Queries: core.Budget.MaxQueries, Rows: core.Budget.MaxResultRows, DBMS: core.Budget.MaxDBMS}) ||
		!reflect.DeepEqual(stored.Exposure, control.ExposureGrant{Limits: control.ExposureLimits{ReleaseFacts: core.Budget.MaxReleaseFacts, InfluenceFacts: core.Budget.MaxInfluenceFacts, OutcomeFacts: core.Budget.MaxOutcomeFacts}, ProfileVersion: core.Budget.ExposureProfileVersion, PredicateFootprint: controlPredicateFootprint(core.Budget.PredicateFootprint)}) ||
		!sameStringSet(stored.ApprovedProducts, core.ApprovedProducts) ||
		!sameColumnSets(stored.ApprovedColumns, core.ApprovedColumns) {
		return false
	}
	var storedScope map[string]any
	if err := json.Unmarshal(stored.MandatoryScope, &storedScope); err != nil {
		return false
	}
	storedCanonical, err := approval.CanonicalJSON(storedScope)
	if err != nil {
		return false
	}
	coreCanonical, err := approval.CanonicalJSON(core.MandatoryScope)
	return err == nil && string(storedCanonical) == string(coreCanonical)
}

func (s *Service) ensureActiveTaskFamily(ctx context.Context, task control.Task) error {
	if task.ParentTaskID == "" {
		return nil
	}
	expectedRoot := task.RootTaskID
	seen := map[string]struct{}{task.ID: {}}
	parentID := task.ParentTaskID
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := seen[parentID]; duplicate {
			return control.ErrInvalidStateChange
		}
		seen[parentID] = struct{}{}
		parent, err := s.store.GetTask(ctx, parentID)
		if err != nil {
			return err
		}
		if parent.State != control.TaskActive || parent.RootTaskID != expectedRoot {
			return control.ErrTaskNotActive
		}
		if parent.ParentTaskID == "" {
			if parent.ID != expectedRoot {
				return control.ErrInvalidStateChange
			}
			return nil
		}
		parentID = parent.ParentTaskID
	}
	return control.ErrInvalidStateChange
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func sameColumnSets(left, right map[string][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for product, leftColumns := range left {
		rightColumns, ok := right[product]
		if !ok || !sameStringSet(leftColumns, rightColumns) {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func saturatedProduct(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if left > maxInt64/right {
		return maxInt64
	}
	return left * right
}

func (s *Service) detachedContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), s.settlementTimeout)
}

func (s *Service) artifactOperationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), s.artifactOperationTimeout)
}

