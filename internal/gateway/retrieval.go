package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// Knowledge retrieval tools (kb-design §4, §5, §10a).
//
// search is the second kind of prepared operation. Its statements come from
// physicalquery.PrepareRetrieval and then take exactly the path every
// execute_plan takes: executeSQL reserves against the task ledger, runs the
// visible and companion statements through the Connector, derives the
// Release / Influence / Outcome sets with the ordinal sink, settles, signs the
// receipt and the execution binding. Nothing retrieval-specific happens after
// preparation, which is the point of §9.7 A.

// retrievalDisplayColumns names the visible projection in order; the compiled
// statement carries opaque aliases.
var retrievalDisplayColumns = []string{"doc_id", "version", "chunk_index", "text"}

var searchTool = mcp.Tool{Name: "search", Description: "在知识产品内按过滤原子与查询（文本或向量）做精确最近邻检索，返回至多 k 个分块；命中分块按任务账本的三维结算记账。",
	InputSchema: objectSchema(map[string]any{
		"task_id":           map[string]any{"type": "string"},
		"request_id":        requestIDSchema(),
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"filters": map[string]any{"type": "array", "items": objectSchema(map[string]any{
			"attribute": map[string]any{"type": "string", "minLength": 1},
			"op":        map[string]any{"type": "string", "enum": []string{"=", "<>", "<", "<=", ">", ">=", "in", "has"}},
			"value":     map[string]any{},
			"values":    map[string]any{"type": "array"},
		}, "attribute", "op")},
		"query": objectSchema(map[string]any{
			"text":   map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			"vector": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
		}),
		"k": map[string]any{"type": "integer", "minimum": 1},
	}, "task_id", "request_id", "knowledge_product", "query", "k")}

// retrievalTools are listed to query principals alongside queryTools.
var retrievalTools = []mcp.Tool{searchTool}

func (s *Service) search(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	requestStarted := time.Now()
	var envelope struct {
		TaskID    string `json:"task_id"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "参数不是合法 JSON 对象"}
	}
	if err := validateRequestID(envelope.RequestID); err != nil {
		return nil, err
	}
	body, err := retrievalBody(raw)
	if err != nil {
		return nil, err
	}
	request, err := retrieval.DecodeRequest(retrieval.ToolSearch, body)
	if err != nil {
		return nil, retrievalToolError(err)
	}
	task, err := s.ownedTask(ctx, principal, envelope.TaskID)
	if err != nil {
		return nil, err
	}
	requestSummary := "search\x00" + task.ID + "\x00" + string(body)
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, envelope.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		replayed, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
		if err != nil {
			return nil, err
		}
		replayed["tool"] = retrieval.ToolSearch
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
	prepared, plan, err := s.prepareRetrieval(grant, request)
	if err != nil {
		replayed, replayErr := s.replayPreparationFailureAt(ctx, task.ID, envelope.RequestID, digest(requestSummary), requestStarted, err)
		if replayErr != nil {
			return nil, replayErr
		}
		response, ok := replayed.(map[string]any)
		if !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久查询重放结果格式无效"}
		}
		response["tool"] = retrieval.ToolSearch
		return response, nil
	}
	metadata := &queryResponseMetadata{Plan: plan, OutputFormat: "json",
		DisplayColumns:  append([]string(nil), retrievalDisplayColumns...),
		ResultOrder:     identityResultOrder(len(retrievalDisplayColumns)),
		SemanticColumns: queryPlanSemanticColumns(plan)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	return s.executeSQL(ctx, principal, task, envelope.RequestID, prepared, requestSummary, metadata, requestStarted)
}

// retrievalBody strips the task envelope so the retrieval request can be
// decoded strictly, and re-encodes it canonically so the idempotency digest
// does not depend on the caller's key order.
func retrievalBody(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "参数不是合法 JSON 对象"}
	}
	delete(fields, "task_id")
	delete(fields, "request_id")
	var decoded map[string]any
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "参数无法编码"}
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "参数无法解码"}
	}
	canonical, err := canonicalJSONBytes(decoded)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "参数无法规范化"}
	}
	return canonical, nil
}

func canonicalJSONBytes(value any) ([]byte, error) {
	// encoding/json sorts map keys, which is the only canonicalization the
	// request digest needs; numbers keep their decoded form.
	return json.Marshal(value)
}

// retrievalToolError maps a closed-form rejection from the normalizer onto a
// tool error carrying its code.
func retrievalToolError(err error) error {
	var rejection *retrieval.Error
	if errors.As(err, &rejection) {
		return &mcp.ToolError{Code: rejection.Code, Message: rejection.Message}
	}
	return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: err.Error()}
}

// prepareRetrieval builds the retrieval preparation inputs from live state
// and seals the operation. It performs no reservation or database I/O.
func (s *Service) prepareRetrieval(grant control.TaskGrant, request retrieval.Request) (preparedQueryPlan, queryplan.QueryPlan, error) {
	if s.catalog == nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未加载 Catalog"}
	}
	product, found := s.catalog.LookupKnowledgeProduct(request.KnowledgeProduct)
	if !found {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务授权外的知识产品"}
	}
	if !contains(grant.ApprovedProducts, product.Products.Chunks) || !contains(grant.ApprovedProducts, product.Products.Documents) {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务未同时授权该知识产品的分块与文档关系"}
	}
	if !grant.Exposure.Enabled() || grant.Exposure.ProfileVersion != exposure.ProfileV5 {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "知识检索要求任务携带 V5 暴露 profile"}
	}
	scope, err := mandatoryScopeValues(grant)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务强制范围无法解析"}
	}
	normalForm, err := retrieval.Normalize(product, scope, request)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, retrievalToolError(err)
	}
	vector, err := s.retrievalQueryVector(product, request)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, err
	}
	view, err := physicalquery.CatalogViewFromCatalog(*s.catalog)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索无法在当前 Catalog 下构造准备输入"}
	}
	bindings, err := s.snapshotBindings()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索无法在当前快照证据下构造准备输入"}
	}
	inputs := physicalquery.RetrievalPreparationInputsV1{
		Grant: preparationGrant(grant), Catalog: view, SnapshotBindings: bindings,
		KnowledgeProduct: product, NormalForm: normalForm, QueryVector: vector,
		VectorSource: chunkVectorSource(product),
	}
	prepared, err := physicalquery.PrepareRetrieval(inputs)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied,
			Message: "检索不在任务授权可精确计量的数据暴露片段内"}
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本进程无法确定查询编译器身份"}
	}
	if err := prepared.RequireRetrievalInputs(inputs, compiler); err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "准备结果与其输入证据不一致；检索未执行"}
	}
	visibleSQL, _, err := prepared.ExecutableStatements()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "准备结果拒绝交出可执行语句"}
	}
	planInputs, err := inputs.PlanInputs()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索的等价计划无法构造"}
	}
	exposureContext, err := s.planExposureContextFrom(prepared, planInputs.Plan)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "已准备的检索无法在当前快照索引下建立观测上下文"}
	}
	return preparedQueryPlan{SQL: visibleSQL, PolicyGrant: prepared.PolicyGrant(), Prepared: prepared, Exposure: exposureContext}, planInputs.Plan, nil
}

// retrievalQueryVector resolves the ranking vector. A vector query is taken
// as given; a text query is embedded from its normalized form, so two requests
// with the same commitment h_N rank with the same vector.
func (s *Service) retrievalQueryVector(product catalog.KnowledgeProduct, request retrieval.Request) ([]float32, error) {
	if request.Query == nil {
		return nil, &mcp.ToolError{Code: apierr.CodeRetrievalQueryUnsupported, Message: "search 需要查询文本或向量"}
	}
	dimension := product.Embedding.Dimension
	if len(request.Query.Vector) > 0 {
		if len(request.Query.Vector) != dimension {
			return nil, &mcp.ToolError{Code: apierr.CodeRetrievalQueryUnsupported,
				Message: fmt.Sprintf("查询向量维度 %d 与知识产品声明的 %d 不符", len(request.Query.Vector), dimension)}
		}
		vector := make([]float32, len(request.Query.Vector))
		for i, value := range request.Query.Vector {
			vector[i] = float32(value)
		}
		return vector, nil
	}
	if s.kbEmbedder == nil {
		return nil, &mcp.ToolError{Code: apierr.CodeRetrievalQueryUnsupported, Message: "本实例未配置嵌入服务，文本查询不可用；请提交查询向量"}
	}
	declared := kbpublish.Embedding{Model: product.Embedding.Model, Dimension: dimension, WeightsSHA256: product.Embedding.WeightsSHA256}
	if err := s.kbEmbedder.Check(declared); err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "嵌入服务与知识产品声明的模型不一致；检索已关闭式拒绝"}
	}
	vectors, err := s.kbEmbedder.Embed([]string{retrieval.NormalizeText(request.Query.Text)})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != dimension {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "嵌入服务未能为查询文本返回声明维度的向量"}
	}
	return vectors[0], nil
}

// mandatoryScopeValues reads the task's stored scope as the value lists the
// normalizer injects. Range scopes are not retrieval attributes and are
// skipped; the policy renderer still applies them to every relation.
func mandatoryScopeValues(grant control.TaskGrant) (map[string][]string, error) {
	values := map[string][]string{}
	if len(grant.MandatoryScope) == 0 {
		return values, nil
	}
	var scope map[string]any
	if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
		return nil, err
	}
	for name, raw := range scope {
		switch typed := raw.(type) {
		case string:
			values[name] = []string{typed}
		case []any:
			list := make([]string, 0, len(typed))
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("mandatory scope %q carries a non-string value", name)
				}
				list = append(list, text)
			}
			values[name] = list
		case map[string]any:
			// A date range: not an enum attribute, nothing to inject.
		default:
			return nil, fmt.Errorf("mandatory scope %q carries an unsupported value", name)
		}
	}
	return values, nil
}

// chunkVectorSource describes where a knowledge product's chunk vectors live.
// The business schema is db/init/kb-schema.psql; the publication renames
// department to kb_department (internal/kbpublish relationSelect), which is
// the only attribute whose name differs between the two.
func chunkVectorSource(product catalog.KnowledgeProduct) physicalquery.RetrievalVectorSource {
	return physicalquery.RetrievalVectorSource{
		LogicalName: product.Name + "_chunk_vectors", Schema: "kb", Relation: "chunks",
		Collection: product.Collection, CollectionColumn: "collection", EmbeddingColumn: "embedding",
		EntityColumns:    []string{"doc_id", "version", "chunk_index"},
		AttributeColumns: map[string]string{"kb_department": "department"},
	}
}
