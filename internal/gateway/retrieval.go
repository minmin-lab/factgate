package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
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
var retrievalDisplayColumns = []string{"doc_id", "version", "chunk_index", retrieval.ChunkTextField, chunkTextDeliveryColumn}

// chunkTextDeliveryColumn is the delivered text the result carries after the
// four sealed columns; its semantic identity marks it as delivery, not a
// projection of the sealed statement (kb-design §9.8).
const chunkTextDeliveryColumn = "text"

var searchTool = mcp.Tool{Name: "search", Description: "在知识产品内按过滤原子与查询检索，返回至多 k 个分块：文本或向量做精确最近邻检索；关键词（知识产品声明 keyword 时）返回正文同时包含每个关键词的分块，区分大小写，按命中字符数排序。" +
	"knowledge_product 填知识产品名（如 policy_kb），不是它的关系名（如 policy_kb_chunks）；k 不超过该知识产品的 k_max；query 三选一：text（归一化后至多 4096 字节）、vector、keywords。" +
	"任务须同时授权该知识产品的 chunks 与 documents 关系及其必需列，另加每个过滤属性在 chunks 上的列；知识产品、属性值域、k_max 与必需列见 list_data_products 的 knowledge_products[].task_requirements。" +
	"结果按相似度（或命中字符数）排序，名次即返回顺序，按设计不返回分数。命中分块按任务账本的三维结算记账。过滤只接受知识产品声明的属性，不接受 doc_id：要某份已知文档的全文或原文件，请用 request_download_task 申请下载任务，再用 download_document 一次取回。",
	InputSchema: objectSchema(map[string]any{
		"task_id":           map[string]any{"type": "string"},
		"request_id":        requestIDSchema(),
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"filters": map[string]any{"type": "array", "items": objectSchema(map[string]any{
			"attribute": map[string]any{"type": "string", "minLength": 1},
			"op":        map[string]any{"type": "string", "enum": []string{"=", "<>", "<", "<=", ">", ">=", "in"}},
			"value":     map[string]any{},
			"values":    map[string]any{"type": "array"},
		}, "attribute", "op")},
		"query": objectSchema(map[string]any{
			"text":   map[string]any{"type": "string", "minLength": 1, "maxLength": 4096},
			"vector": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
			"keywords": map[string]any{"type": "array", "minItems": 1, "maxItems": retrieval.MaxKeywords,
				"items": map[string]any{"type": "string", "minLength": 1, "maxLength": retrieval.MaxKeywordRunes}},
		}),
		"k": map[string]any{"type": "integer", "minimum": 1},
	}, "task_id", "request_id", "knowledge_product", "query", "k")}

var fetchTool = mcp.Tool{Name: "fetch", Description: "按 (doc_id, version, chunk_index) 取回先前检索命中的分块正文；取回同样按任务账本结算，已结算过的分块不再重复计费。只能取本任务族此前检索返回过的块；任务授权要求与 search 相同（见 list_data_products 的 task_requirements）；要整份文档请用 request_download_task 与 download_document。",
	InputSchema: objectSchema(map[string]any{
		"task_id":           map[string]any{"type": "string"},
		"request_id":        requestIDSchema(),
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"chunks": map[string]any{"type": "array", "minItems": 1, "items": objectSchema(map[string]any{
			"doc_id":      map[string]any{"type": "string", "minLength": 1},
			"version":     map[string]any{"type": "integer", "minimum": 0},
			"chunk_index": map[string]any{"type": "integer", "minimum": 0},
		}, "doc_id", "version", "chunk_index")},
	}, "task_id", "request_id", "knowledge_product", "chunks")}

// retrievalTools are listed to query principals alongside queryTools.
var retrievalTools = []mcp.Tool{searchTool, fetchTool, aggregateTool}

// retrievalToolsFor is retrievalTools with search's k bounded by the largest
// k_max the loaded Catalog declares. Each knowledge product's own k_max is
// published by list_data_products and enforced on the request; the schema
// bound only spares a client the round trip.
func retrievalToolsFor(loaded *catalog.Catalog) []mcp.Tool {
	kMax := 0
	if loaded != nil {
		for _, kp := range loaded.KnowledgeProducts {
			kMax = max(kMax, kp.Retrieval.KMax)
		}
	}
	tools := append([]mcp.Tool(nil), retrievalTools...)
	if kMax == 0 {
		return tools
	}
	properties, _ := searchTool.InputSchema["properties"].(map[string]any)
	bounded := make(map[string]any, len(properties))
	for name, property := range properties {
		bounded[name] = property
	}
	bounded["k"] = map[string]any{"type": "integer", "minimum": 1, "maximum": kMax}
	schema := make(map[string]any, len(searchTool.InputSchema))
	for key, value := range searchTool.InputSchema {
		schema[key] = value
	}
	schema["properties"] = bounded
	for index := range tools {
		if tools[index].Name == searchTool.Name {
			tools[index].InputSchema = schema
		}
	}
	return tools
}

func (s *Service) search(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	return s.retrieve(ctx, principal, raw, retrieval.ToolSearch)
}

func (s *Service) fetch(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	return s.retrieve(ctx, principal, raw, retrieval.ToolFetch)
}

// retrieve is the shared flow of search and fetch: decode, idempotency,
// task and grant checks, preparation, then executeSQL.
func (s *Service) retrieve(ctx context.Context, principal mcp.Principal, raw json.RawMessage, tool string) (any, error) {
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
	request, err := retrieval.DecodeRequest(tool, body)
	if err != nil {
		return nil, retrievalToolError(err)
	}
	task, err := s.ownedTask(ctx, principal, envelope.TaskID)
	if err != nil {
		return nil, err
	}
	requestSummary := tool + "\x00" + task.ID + "\x00" + string(body)
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, envelope.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		replayed, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
		if err != nil {
			return nil, err
		}
		replayed["tool"] = tool
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
	// preparationFailure follows the query path's rule for a refusal before
	// any reservation: nothing is persisted, but if a concurrent attempt with
	// the same request id has meanwhile completed, that durable result is
	// replayed (its stored result already carries the delivered text) instead
	// of the refusal.
	preparationFailure := func(cause error) (any, error) {
		replayed, replayErr := s.replayPreparationFailureAt(ctx, task.ID, envelope.RequestID, digest(requestSummary), requestStarted, cause)
		if replayErr != nil {
			return nil, replayErr
		}
		response, ok := replayed.(map[string]any)
		if !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久查询重放结果格式无效"}
		}
		response["tool"] = tool
		return response, nil
	}
	if tool == retrieval.ToolFetch {
		// A fetch names chunks already returned to this root family (kb-design
		// §9.6 a): anything else, including a chunk outside the task's scope,
		// is refused closed before any reservation.
		if err := s.requireChunksReturned(ctx, task, request); err != nil {
			return preparationFailure(err)
		}
	}
	prepared, plan, err := s.prepareRetrieval(grant, request)
	if err != nil {
		return preparationFailure(err)
	}
	product, _ := s.catalog.LookupKnowledgeProduct(request.KnowledgeProduct)
	prepared.EnrichResult = func(ctx context.Context, stored *storedQueryResult) error {
		return s.enrichChunkText(ctx, product, stored)
	}
	metadata := &queryResponseMetadata{Plan: plan, OutputFormat: "json",
		DisplayColumns: append([]string(nil), retrievalDisplayColumns...),
		ResultOrder:    identityResultOrder(len(retrievalDisplayColumns)),
		SemanticColumns: append(queryPlanSemanticColumns(plan),
			"delivery:"+product.Products.Chunks+"."+chunkTextDeliveryColumn)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	return s.executeSQL(ctx, principal, task, envelope.RequestID, prepared, requestSummary, metadata, requestStarted)
}

// enrichChunkText completes a search or fetch result with the chunk text. The
// ledger carries a returned chunk's content digest as its Result cell
// (kb-design §9.8); the text is not a published field. It is read from
// kb.chunks by (collection, identity) for exactly the rows the sealed statement
// returned, and a row is admitted only when the SHA-256 of what was read is the
// digest the statement returned. Any missing or drifted row refuses the whole
// result before settlement, so nothing is released for a result that cannot be
// delivered. The text becomes the fifth column of the canonical result, so it
// is what a replay and a result artifact carry, under the receipt that binds
// them.
func (s *Service) enrichChunkText(ctx context.Context, product catalog.KnowledgeProduct, stored *storedQueryResult) error {
	if len(stored.Columns) != len(retrievalDisplayColumns)-1 {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索结果列数与封印语句不符"}
	}
	if len(stored.Rows) == 0 {
		stored.Columns = append(stored.Columns, dataconnector.Column{Name: chunkTextDeliveryColumn, DataTypeOID: 25})
		return nil
	}
	type identity struct {
		doc            string
		version, chunk int64
	}
	keys := make([]identity, len(stored.Rows))
	tuples := make([]string, 0, len(stored.Rows))
	for index, row := range stored.Rows {
		if len(row) != len(stored.Columns) {
			return &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索结果行形状无效"}
		}
		doc, ok := row[0].(string)
		version, okVersion := responseInteger(row[1])
		chunk, okChunk := responseInteger(row[2])
		if !ok || !okVersion || !okChunk {
			return &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索结果身份列无效"}
		}
		literal, err := pgTextLiteral(doc)
		if err != nil {
			return &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索结果文档号无法编码"}
		}
		keys[index] = identity{doc, version, chunk}
		tuples = append(tuples, fmt.Sprintf("(%s, %d, %d)", literal, version, chunk))
	}
	collection, err := pgTextLiteral(product.Collection)
	if err != nil || product.Collection == "" {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "知识产品集合名无法编码"}
	}
	statement := "SELECT doc_id, version, chunk_index, text FROM kb.chunks WHERE collection = " + collection +
		" AND (doc_id, version, chunk_index) IN (" + strings.Join(tuples, ", ") + ")"
	result, err := s.connector.Query(ctx, dataconnector.QueryRequest{SQL: statement, StatementTimeout: chunkTextDeliveryTimeout, MaxRows: int64(len(stored.Rows))})
	if err != nil {
		return &mcp.ToolError{Code: apierr.CodeDataSourceUnavailable, Message: "读取分块正文失败"}
	}
	texts := make(map[identity]string, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) != 4 {
			continue
		}
		doc, _ := row[0].(string)
		version, _ := responseInteger(row[1])
		chunk, _ := responseInteger(row[2])
		text, _ := row[3].(string)
		texts[identity{doc, version, chunk}] = text
	}
	for index, row := range stored.Rows {
		text, present := texts[keys[index]]
		released, _ := row[3].(string)
		if !present || released == "" || sha256Hex(text) != released {
			return &mcp.ToolError{Code: apierr.CodeConflict, Message: fmt.Sprintf("块 %s v%d #%d 的正文与发布期摘要不一致，拒绝交付", keys[index].doc, keys[index].version, keys[index].chunk)}
		}
		stored.Rows[index] = append(row, text)
	}
	stored.Columns = append(stored.Columns, dataconnector.Column{Name: chunkTextDeliveryColumn, DataTypeOID: 25})
	return nil
}

// chunkTextDeliveryTimeout bounds the delivery read of chunk text; it is a
// bounded identity lookup on kb.chunks, not a governed query, so it carries
// its own fixed limit rather than the task's query budget.
const chunkTextDeliveryTimeout = 5 * time.Second

// responseInteger reads an integer cell however the result or replay path
// encoded it.
func responseInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case float64:
		if typed != math.Trunc(typed) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

// pgTextLiteral renders a standard-conforming single-quoted literal; control
// characters are refused rather than escaped.
func pgTextLiteral(value string) (string, error) {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("control character in literal")
		}
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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
		return preparedQueryPlan{}, queryplan.QueryPlan{}, s.unknownKnowledgeProduct(request.KnowledgeProduct)
	}
	if !contains(grant.ApprovedProducts, product.Products.Chunks) || !contains(grant.ApprovedProducts, product.Products.Documents) {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, knowledgeRelationsNotGranted("任务未同时授权该知识产品的分块与文档关系", product, request.Tool,
			[]string{product.Products.Chunks, product.Products.Documents}, grant.ApprovedProducts)
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
	// Both refusals below are ones the preparation would make; they come
	// first only to say precisely what the request or the task lacks.
	if err := requireRenderedOperators(normalForm); err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, err
	}
	if err := requireRetrievalColumns(product, normalForm, grant.ApprovedColumns); err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, err
	}
	var vector []float32
	var keywords []string
	source := chunkVectorSource(product)
	if request.Tool == retrieval.ToolSearch {
		if normalForm.Query != nil && normalForm.Query.Kind == retrieval.QueryKindKeyword {
			// Keyword search ranks inside the database; no embedding is involved.
			if keywords, err = retrieval.CanonicalKeywords(request.Query.Keywords); err != nil {
				return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeRetrievalQueryUnsupported, Message: err.Error()}
			}
			source.TextColumn = chunkTextDeliveryColumn
		} else if vector, err = s.retrievalQueryVector(product, request); err != nil {
			return preparedQueryPlan{}, queryplan.QueryPlan{}, err
		}
	}
	view, err := physicalquery.CatalogViewFromCatalog(*s.catalog)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索无法在当前 Catalog 下构造准备输入", Cause: err}
	}
	bindings, err := s.snapshotBindings()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索无法在当前快照证据下构造准备输入", Cause: err}
	}
	inputs := physicalquery.RetrievalPreparationInputsV1{
		Grant: preparationGrant(grant), Catalog: view, SnapshotBindings: bindings,
		KnowledgeProduct: product, NormalForm: normalForm, QueryVector: vector, QueryKeywords: keywords,
		VectorSource: source,
	}
	prepared, err := physicalquery.PrepareRetrieval(inputs)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied,
			Message: "检索不在任务授权可精确计量的数据暴露片段内",
			Details: map[string]any{"reason": "RETRIEVAL_NOT_PREPARABLE"}, Cause: err}
	}
	compiler, err := physicalquery.LocalCompilerIdentity()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本进程无法确定查询编译器身份", Cause: err}
	}
	if err := prepared.RequireRetrievalInputs(inputs, compiler); err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "准备结果与其输入证据不一致；检索未执行", Cause: err}
	}
	visibleSQL, _, err := prepared.ExecutableStatements()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "准备结果拒绝交出可执行语句", Cause: err}
	}
	planInputs, err := inputs.PlanInputs()
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "检索的等价计划无法构造", Cause: err}
	}
	exposureContext, err := s.planExposureContextFrom(prepared, planInputs.Plan)
	if err != nil {
		return preparedQueryPlan{}, queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "已准备的检索无法在当前快照索引下建立观测上下文", Cause: err}
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

var aggregateTool = mcp.Tool{Name: "aggregate", Description: "对知识产品抽取值做 count / sum / min / max：按字段与范围过滤后在 values 关系上执行，结算与 query_sql 相同（派生 Fact 带见证集）。",
	InputSchema: objectSchema(map[string]any{
		"task_id":           map[string]any{"type": "string"},
		"request_id":        requestIDSchema(),
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"filters": map[string]any{"type": "array", "items": objectSchema(map[string]any{
			"attribute": map[string]any{"type": "string", "minLength": 1},
			"op":        map[string]any{"type": "string", "enum": []string{"=", "<>", "in"}},
			"value":     map[string]any{},
			"values":    map[string]any{"type": "array"},
		}, "attribute", "op")},
		"field":     map[string]any{"type": "string", "minLength": 1},
		"aggregate": map[string]any{"type": "string", "enum": []string{"count", "sum", "min", "max"}},
	}, "task_id", "request_id", "knowledge_product", "field", "aggregate")}

// aggregate is kb-design §5's third tool: the request is normalized by the
// closed retrieval language, then executed as an ordinary plan over the
// knowledge product's values relation, so settlement, replay and receipts are
// exactly execute_plan's. Nothing retrieval-specific is settled.
func (s *Service) aggregate(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
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
	request, err := retrieval.DecodeRequest(retrieval.ToolAggregate, body)
	if err != nil {
		return nil, retrievalToolError(err)
	}
	task, err := s.ownedTask(ctx, principal, envelope.TaskID)
	if err != nil {
		return nil, err
	}
	requestSummary := retrieval.ToolAggregate + "\x00" + task.ID + "\x00" + string(body)
	existing, lookupErr := s.store.GetQueryByRequestID(ctx, task.ID, envelope.RequestID)
	if lookupErr == nil {
		if existing.RequestDigest != digest(requestSummary) {
			return nil, toolError(control.ErrIdempotencyConflict)
		}
		replayed, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
		if err != nil {
			return nil, err
		}
		replayed["tool"] = retrieval.ToolAggregate
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
	plan, err := s.aggregatePlan(grant, request)
	if err != nil {
		return nil, err
	}
	if missing := missingPlanColumns(singleProductPlanColumns(plan), grant.ApprovedColumns); len(missing) > 0 {
		product, _ := s.catalog.LookupKnowledgeProduct(request.KnowledgeProduct)
		return nil, columnsNotGranted(product, retrieval.ToolAggregate, missing)
	}
	prepared, err := s.prepareTaskPlan(ctx, task, grant, plan)
	if err != nil {
		replayed, replayErr := s.replayPreparationFailureAt(ctx, task.ID, envelope.RequestID, digest(requestSummary), requestStarted, err)
		if replayErr != nil {
			return nil, replayErr
		}
		response, ok := replayed.(map[string]any)
		if !ok {
			return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "持久查询重放结果格式无效"}
		}
		response["tool"] = retrieval.ToolAggregate
		return response, nil
	}
	resultNames := queryPlanResultNames(plan)
	metadata := &queryResponseMetadata{Plan: plan, OutputFormat: "json", DisplayColumns: resultNames,
		ResultOrder: identityResultOrder(len(resultNames)), SemanticColumns: queryPlanSemanticColumns(plan)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	return s.executeSQL(ctx, principal, task, envelope.RequestID, prepared, requestSummary, metadata, requestStarted)
}

// aggregatePlan maps the normalized aggregate onto the values relation: the
// extracted field and its value type select the rows, caller atoms become
// filters when the values relation carries the attribute, and the function
// reads the typed value column. The task's mandatory scope is applied by the
// policy renderer, as for every plan.
func (s *Service) aggregatePlan(grant control.TaskGrant, request retrieval.Request) (queryplan.QueryPlan, error) {
	if s.catalog == nil {
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未加载 Catalog"}
	}
	product, found := s.catalog.LookupKnowledgeProduct(request.KnowledgeProduct)
	if !found {
		return queryplan.QueryPlan{}, s.unknownKnowledgeProduct(request.KnowledgeProduct)
	}
	// A collection-scoped field aggregates over the deduplicated relation, so
	// the witness set and the dependency are distinct values within the
	// task's scope rather than one row per document.
	values := product.Products.Values
	for _, rule := range product.ExtractionRules {
		if rule.Field == request.Field && rule.Scope == catalog.KBExtractionScopeCollection {
			values = product.Products.CollectionValues
		}
	}
	if values == "" {
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodePolicyDenied, Message: "任务未授权该知识产品的抽取值关系"}
	}
	if !contains(grant.ApprovedProducts, values) {
		return queryplan.QueryPlan{}, knowledgeRelationsNotGranted("任务未授权该知识产品的抽取值关系", product, retrieval.ToolAggregate,
			[]string{values}, grant.ApprovedProducts)
	}
	valuesProduct, found := s.catalog.LookupProduct(values)
	if !found {
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "Catalog 未声明抽取值关系"}
	}
	scope, err := mandatoryScopeValues(grant)
	if err != nil {
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务强制范围无法解析"}
	}
	form, err := retrieval.Normalize(product, scope, request)
	if err != nil {
		return queryplan.QueryPlan{}, retrievalToolError(err)
	}
	if form.Aggregate == nil {
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeRetrievalQueryUnsupported, Message: "aggregate 需要字段与聚合函数"}
	}
	columns := map[string]bool{}
	for _, field := range valuesProduct.Fields {
		columns[field.Name] = true
	}
	plan := queryplan.QueryPlan{Product: values, Filters: []queryplan.Filter{
		{Column: "field", Op: "=", Value: form.Aggregate.Field},
		{Column: "value_type", Op: "=", Value: form.Aggregate.ValueType},
	}}
	filters, err := aggregateAtomFilters(form, columns)
	if err != nil {
		return queryplan.QueryPlan{}, err
	}
	plan.Filters = append(plan.Filters, filters...)
	column := "canonical_value"
	switch form.Aggregate.Function {
	case "count":
	case "sum", "min", "max":
		switch form.Aggregate.ValueType {
		case "numeric":
			column = "value_numeric"
		case "date":
			if form.Aggregate.Function == "sum" {
				return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeAggregateUnsupported, Message: "日期字段不支持 sum"}
			}
			column = "value_date"
		default:
			return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeAggregateUnsupported, Message: "文本字段只支持 count"}
		}
	default:
		return queryplan.QueryPlan{}, &mcp.ToolError{Code: apierr.CodeAggregateUnsupported, Message: "未知聚合函数"}
	}
	plan.Aggregates = []queryplan.Aggregate{{Function: form.Aggregate.Function, Column: column, Alias: form.Aggregate.Function}}
	return plan, nil
}

// aggregateAtomFilters renders caller atoms (grouped back from IN lists) as
// plan filters on the values relation; an attribute the relation lacks is a
// closed refusal rather than a silently dropped filter.
func aggregateAtomFilters(form retrieval.NormalForm, columns map[string]bool) ([]queryplan.Filter, error) {
	type key struct{ attribute, operator string }
	grouped := map[key][]retrieval.Atom{}
	var order []key
	for _, atom := range form.CallerAtoms() {
		if !columns[atom.Attribute] {
			return nil, &mcp.ToolError{Code: apierr.CodeRetrievalFilterUnsupported,
				Message: fmt.Sprintf("aggregate 的过滤属性 %q 不在抽取值关系上", atom.Attribute)}
		}
		k := key{atom.Attribute, atom.Operator}
		if _, seen := grouped[k]; !seen {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], atom)
	}
	var filters []queryplan.Filter
	for _, k := range order {
		atoms := grouped[k]
		switch k.operator {
		case "in":
			list := make([]any, 0, len(atoms))
			for _, atom := range atoms {
				list = append(list, strings.TrimPrefix(atom.CanonicalLiteral, "s:"))
			}
			filters = append(filters, queryplan.Filter{Column: k.attribute, Op: "IN", Value: list})
		case "=", "<>":
			for _, atom := range atoms {
				filters = append(filters, queryplan.Filter{Column: k.attribute, Op: k.operator, Value: strings.TrimPrefix(atom.CanonicalLiteral, "s:")})
			}
		default:
			return nil, &mcp.ToolError{Code: apierr.CodeRetrievalFilterUnsupported, Message: fmt.Sprintf("aggregate 不支持算子 %q", k.operator)}
		}
	}
	return filters, nil
}

// requireChunksReturned checks every chunk a fetch names against the root
// family's settled Release set: the chunk's published text cell must already
// be released. The lookup goes through the chunks publication's HOT index, so
// a chunk the publication does not hold is "not returned" as well.
func (s *Service) requireChunksReturned(ctx context.Context, task control.Task, request retrieval.Request) error {
	product, found := s.catalog.LookupKnowledgeProduct(request.KnowledgeProduct)
	if !found {
		return s.unknownKnowledgeProduct(request.KnowledgeProduct)
	}
	chunksProduct, found := s.catalog.LookupProduct(product.Products.Chunks)
	if !found || s.snapshotRegistry == nil {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "知识产品的分块发布物未激活"}
	}
	index, err := s.snapshotRegistry.Resolve(ordinal.PublicationKey{CatalogDigest: s.catalog.SHA256, PublicationName: chunksProduct.SnapshotPublication})
	if err != nil {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "知识产品的分块发布物未激活"}
	}
	textField := chunksProduct.Name + "." + retrieval.ChunkTextField
	refs := make([]ordinal.FactRef, 0, len(request.Chunks))
	for _, ref := range request.Chunks {
		entityKey, err := retrieval.ChunkEntityKey(product.SourceNamespace, ref.DocID, ref.Version, ref.ChunkIndex)
		if err != nil {
			return &mcp.ToolError{Code: apierr.CodeChunkNotReturned, Message: "fetch 的块引用无法编码"}
		}
		row, ok := index.LookupEntity(entityKey)
		if !ok {
			return &mcp.ToolError{Code: apierr.CodeChunkNotReturned, Message: fmt.Sprintf("块 %s v%d #%d 不是本任务族此前检索返回的块；要整份文档请用 request_download_task 申请下载任务", ref.DocID, ref.Version, ref.ChunkIndex)}
		}
		cell, ok := row.Cells[textField]
		if !ok {
			return &mcp.ToolError{Code: apierr.CodeConflict, Message: "分块发布物不含正文单元格"}
		}
		refs = append(refs, cell)
	}
	released, err := s.store.RootReleaseContains(ctx, task.ID, refs)
	if err != nil {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "无法读取任务族的已释放集合"}
	}
	for i, ok := range released {
		if !ok {
			ref := request.Chunks[i]
			return &mcp.ToolError{Code: apierr.CodeChunkNotReturned, Message: fmt.Sprintf("块 %s v%d #%d 不是本任务族此前检索返回的块；要整份文档请用 request_download_task 申请下载任务", ref.DocID, ref.Version, ref.ChunkIndex)}
		}
	}
	return nil
}
