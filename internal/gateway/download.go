package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/kbdownload"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// Document download task (docs/kb-design.md §12).
//
// Searching and downloading are separate task kinds. A download task's grant
// carries the approved document list under the reserved scope key
// kbdownload.ScopeKey; the approver sees that list and may narrow it. A
// download reads one approved document through three ordinary prepared
// operations, each reserved, executed, settled and receipted by executeSQL:
//
//	<request_id>:document  the documents row (title, chunk_count), a plan
//	<request_id>:chunks    every chunk of the document, a fetch-form retrieval
//	<request_id>:values    the extracted values, a plan, only when approved
//
// The chunk operation refuses before settlement unless it returns exactly
// chunk_count chunks: a section outside the task's scope (or any other gap)
// refuses the whole document and releases no chunk. Only after all parts have
// settled is the file assembled, from the settled rows alone.

const downloadToolName = "download_document"

// maxDownloadRequestIDLength leaves room for the part suffixes inside the
// 128-character request id contract.
const maxDownloadRequestIDLength = 100

var requestDownloadTaskTool = mcp.Tool{Name: "request_download_task",
	Description: "申请一个文档下载任务：列出要下载的文档（doc_id 与 version，1 到 20 份），审批人在 OA 看到这份清单并可删减。下载任务只能下载清单内的文档，不能检索；检索请另建任务。include_values 为 true 时一并申请抽取值（下载文件带类型化字段）。original_file 为 true 时一并申请原件（files 关系，如 PDF；原件含前置字段，故隐含 include_values），审批人可删去该关系即不批原件。scopes 缺省为申请人的整个可申请范围（常含全部部门与最高密级如 top_secret（绝密），因而要更多人审批：路由到最高密级的审批人，或审批链上每个部门的主任、分管领导乃至总经理）；最小权限做法是按文档已知的部门与密级给出最窄的 scopes，例如 {kb_department: [sales], classification: [internal]}。approver 可选，指定审批人。",
	InputSchema: objectSchema(map[string]any{
		"objective":         map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"documents": map[string]any{"type": "array", "minItems": 1, "maxItems": kbdownload.MaxDocuments, "items": objectSchema(map[string]any{
			"doc_id":  map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"version": map[string]any{"type": "integer", "minimum": 0},
		}, "doc_id", "version")},
		"include_values": map[string]any{"type": "boolean"},
		"original_file":  map[string]any{"type": "boolean"},
		"scopes":         map[string]any{"type": "object"},
		"approver":       map[string]any{"type": "string", "minLength": 1, "maxLength": 64},
	}, "objective", "knowledge_product", "documents")}

var downloadDocumentTool = mcp.Tool{Name: downloadToolName,
	Description: "在下载任务中下载一份已批准的文档。format 缺省为 assembled：由已发布内容组装的 Markdown 文件（标题、批准时的抽取值、按序的分块正文）；format 为 original 时交付原件（如 PDF；任务须批准 files 关系与抽取值），其大小与 SHA-256 等于已发布的 files 行。返回文件 SHA-256、逐块摘要清单与短期下载地址；组装文件另附全文，原件以嵌入资源附二进制。文档有任务范围外的节时整份拒绝。读取按任务账本结算；同一 request_id 重复下载不重复计费。",
	InputSchema: objectSchema(map[string]any{
		"task_id":           map[string]any{"type": "string"},
		"request_id":        map[string]any{"type": "string", "minLength": 1, "maxLength": maxDownloadRequestIDLength, "pattern": "^[A-Za-z0-9._:-]+$"},
		"knowledge_product": map[string]any{"type": "string", "minLength": 1},
		"doc_id":            map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"version":           map[string]any{"type": "integer", "minimum": 0},
		"format":            map[string]any{"type": "string", "enum": []string{kbdownload.FormatAssembled, kbdownload.FormatOriginal}},
	}, "task_id", "request_id", "knowledge_product", "doc_id", "version")}

// downloadPartDigestSource is the idempotency summary of one download part. The
// parts are the same plans in both formats; the original adds the files part.
func downloadPartDigestSource(part, taskID, knowledgeProduct string, ref kbdownload.Ref) string {
	return downloadToolName + "\x00" + part + "\x00" + taskID + "\x00" + knowledgeProduct + "\x00" + ref.String()
}

// KBOriginalStore reads document originals by object key (docs/kb-design.md
// §12.9). It is read-only: the publisher writes the objects.
type KBOriginalStore interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// downloadTools are listed to query principals alongside the retrieval tools.
var downloadTools = []mcp.Tool{requestDownloadTaskTool, downloadDocumentTool}

// downloadListOf reads a grant's approved document list. isDownload is false
// for an ordinary task; a malformed list is an error, never an ordinary task.
func downloadListOf(grant control.TaskGrant) ([]string, bool, error) {
	var scope map[string]any
	if len(grant.MandatoryScope) > 0 {
		if err := json.Unmarshal(grant.MandatoryScope, &scope); err != nil {
			return nil, false, err
		}
	}
	return kbdownload.ApprovedList(scope)
}

// requireOrdinaryTask refuses search, fetch, aggregate and queries in a
// download task.
func requireOrdinaryTask(grant control.TaskGrant) error {
	_, isDownload, err := downloadListOf(grant)
	if err != nil {
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的下载清单无法解析"}
	}
	if isDownload {
		return &mcp.ToolError{Code: apierr.CodeTaskKindMismatch, Message: "下载任务只能下载已批准的文档；检索与查询请另建任务"}
	}
	return nil
}

func (s *Service) requestDownloadTask(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		Objective        string           `json:"objective"`
		KnowledgeProduct string           `json:"knowledge_product"`
		Documents        []kbdownload.Ref `json:"documents"`
		IncludeValues    bool             `json:"include_values,omitempty"`
		OriginalFile     bool             `json:"original_file,omitempty"`
		Scopes           map[string]any   `json:"scopes,omitempty"`
		Approver         string           `json:"approver,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if s.catalog == nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未加载 Catalog"}
	}
	product, found := s.catalog.LookupKnowledgeProduct(args.KnowledgeProduct)
	if !found {
		return nil, s.unknownKnowledgeProduct(args.KnowledgeProduct)
	}
	list, err := kbdownload.CanonicalList(args.Documents)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: err.Error()}
	}
	columns := map[string][]string{
		product.Products.Chunks:    {"doc_id", "version", "chunk_index", retrieval.ChunkTextField, "kb_department", "classification"},
		product.Products.Documents: {"doc_id", "version", "title", "chunk_count", "kb_department", "classification"},
	}
	if args.OriginalFile {
		// The original-file right is the files relation; the original carries
		// the front matter, so the values relation comes with it
		// (docs/kb-design.md §12.9).
		if product.Products.Files == "" {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "该知识产品没有原件（未发布 files 关系）"}
		}
		columns[product.Products.Files] = []string{"doc_id", "version", "media_type", "byte_size", "file_sha256", "kb_department", "classification"}
		args.IncludeValues = true
	}
	if args.IncludeValues {
		if product.Products.Values == "" {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "该知识产品没有抽取值关系"}
		}
		columns[product.Products.Values] = []string{"doc_id", "version", "field", "value_type", "canonical_value", "kb_department", "classification"}
	}
	products := make([]string, 0, len(columns))
	for name := range columns {
		products = append(products, name)
	}
	sort.Strings(products)
	scopes := args.Scopes
	scopesDefaulted := scopes == nil
	if scopes == nil {
		if scopes, err = s.eligibilityEnvelopeScope(product, principal.Subject); err != nil {
			return nil, err
		}
	}
	result, err := s.createTask(ctx, principal, taskRequestArgs{Objective: args.Objective, DataProducts: products,
		Columns: columns, Scopes: scopes, Approver: args.Approver}, map[string]any{kbdownload.ScopeKey: list})
	if err != nil {
		return nil, err
	}
	result["task_kind"] = "download"
	result["documents"] = list
	result["original_file"] = args.OriginalFile
	// A defaulted scope is the requester's whole envelope; say so, because it
	// decides the approval route (often a leadership approver).
	result["scopes_defaulted_to_envelope"] = scopesDefaulted
	return result, nil
}

// eligibilityEnvelopeScope is a download task's default scope: for every scope
// the knowledge product's eligibility envelope declares, the values the human
// subject is admissible for (their own attribute values, or every allowed value
// when a widening condition holds). compileEligibility then admits it as any
// request; nothing here widens the envelope.
func (s *Service) eligibilityEnvelopeScope(product catalog.KnowledgeProduct, subject string) (map[string]any, error) {
	if product.Eligibility == nil || len(product.Eligibility.ScopeEnvelope) == 0 {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "该知识产品未声明资格规则（eligibility）；请显式提供 scopes"}
	}
	snapshot := s.principalAttributes
	if snapshot == nil || !snapshot.Known(subject) {
		return nil, notEligible("申请人不在主体属性快照中，不具备申请资格")
	}
	definitions := make(map[string]catalog.Scope, len(s.catalog.Scopes))
	for _, definition := range s.catalog.Scopes {
		definitions[definition.Name] = definition
	}
	now := s.clock().UTC()
	scope := make(map[string]any, len(product.Scopes))
	for _, name := range product.Scopes {
		rule, declared := product.Eligibility.ScopeEnvelope[name]
		definition, defined := definitions[name]
		if !declared || !defined {
			return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: fmt.Sprintf("按属性推出的可申请范围不含 %s；请显式提供 scopes", name)}
		}
		admissible := snapshot.Values(subject, rule.FromAttribute, now)
		if anyConditionHolds(snapshot, subject, rule.WidenIf, now) {
			admissible = definition.AllowedValues
		}
		values := intersectSorted(sortedStrings(definition.AllowedValues), admissible)
		if len(values) == 0 {
			return nil, notEligible(fmt.Sprintf("申请人在范围 %s 上没有可申请的值", name))
		}
		scope[name] = values
	}
	return scope, nil
}

func sortedStrings(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}

// downloadPart is one settled sub-operation of a download.
type downloadPart struct {
	name     string
	response map[string]any
	rows     [][]any
}

func (s *Service) downloadDocument(ctx context.Context, principal mcp.Principal, raw json.RawMessage) (mcp.ToolResult, error) {
	requestStarted := time.Now()
	var args struct {
		TaskID           string `json:"task_id"`
		RequestID        string `json:"request_id"`
		KnowledgeProduct string `json:"knowledge_product"`
		DocID            string `json:"doc_id"`
		Version          *int64 `json:"version"`
		Format           string `json:"format,omitempty"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return mcp.ToolResult{}, err
	}
	if args.Format == "" {
		args.Format = kbdownload.FormatAssembled
	}
	if args.Format != kbdownload.FormatAssembled && args.Format != kbdownload.FormatOriginal {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "format 只能是 assembled 或 original"}
	}
	if err := validateRequestID(args.RequestID); err != nil {
		return mcp.ToolResult{}, err
	}
	if len(args.RequestID) > maxDownloadRequestIDLength {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: fmt.Sprintf("download_document 的 request_id 最长 %d 个字符", maxDownloadRequestIDLength)}
	}
	if args.Version == nil {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "version 必须显式提供"}
	}
	ref := kbdownload.Ref{DocID: args.DocID, Version: *args.Version}
	if err := ref.Validate(); err != nil {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: err.Error()}
	}
	task, err := s.ownedTask(ctx, principal, args.TaskID)
	if err != nil {
		return mcp.ToolResult{}, err
	}
	grant, err := s.store.GetGrant(ctx, task.ID)
	if err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
		}
		return mcp.ToolResult{}, err
	}
	approved, isDownload, err := downloadListOf(grant)
	if err != nil {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "任务的下载清单无法解析"}
	}
	if !isDownload {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeTaskKindMismatch, Message: "这不是下载任务；请用 request_download_task 申请下载"}
	}
	if !contains(approved, ref.String()) {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeDownloadNotApproved, Message: fmt.Sprintf("文档 %s 不在本任务批准的下载清单内", ref)}
	}
	if s.catalog == nil {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未加载 Catalog"}
	}
	product, found := s.catalog.LookupKnowledgeProduct(args.KnowledgeProduct)
	if !found {
		return mcp.ToolResult{}, s.unknownKnowledgeProduct(args.KnowledgeProduct)
	}
	if !contains(grant.ApprovedProducts, product.Products.Chunks) || !contains(grant.ApprovedProducts, product.Products.Documents) {
		return mcp.ToolResult{}, knowledgeRelationsNotGranted("任务未授权该知识产品的分块与文档关系", product, "download_document",
			[]string{product.Products.Chunks, product.Products.Documents}, grant.ApprovedProducts)
	}
	includeValues := product.Products.Values != "" && contains(grant.ApprovedProducts, product.Products.Values)
	original := args.Format == kbdownload.FormatOriginal
	if original {
		if product.Products.Files == "" || !includeValues || !contains(grant.ApprovedProducts, product.Products.Files) {
			return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeDownloadNotApproved,
				Message: "本任务未批准原件：须批准该知识产品的 files 关系与抽取值关系"}
		}
		if s.kbOriginals == nil {
			return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未配置原件存储"}
		}
	}
	summary := func(part string) string {
		return downloadPartDigestSource(part, task.ID, product.Name, ref)
	}

	// Plan literals take the types a decoded MCP plan carries (queryplan
	// sqlLiteral): an integer is a json.Number.
	versionLiteral := json.Number(strconv.FormatInt(ref.Version, 10))
	documentColumns := []string{"title", "chunk_count"}
	documentPlan := queryplan.QueryPlan{Product: product.Products.Documents, Columns: documentColumns,
		Filters: []queryplan.Filter{{Column: "doc_id", Op: "=", Value: ref.DocID}, {Column: "version", Op: "=", Value: versionLiteral}}}
	documentPart, err := s.runDownloadPlan(ctx, principal, task, grant, args.RequestID+":document", summary("document"), documentPlan, requestStarted)
	if err != nil {
		return mcp.ToolResult{}, err
	}
	if len(documentPart.rows) != 1 || len(documentPart.rows[0]) != len(documentColumns) {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeDocumentNotDownloadable, Message: fmt.Sprintf("文档 %s 不存在或在任务范围外", ref)}
	}
	title, _ := documentPart.rows[0][0].(string)
	chunkCount, ok := responseInteger(documentPart.rows[0][1])
	if !ok || chunkCount < 1 {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "文档行的 chunk_count 无效"}
	}
	chunkPart, err := s.runDownloadChunks(ctx, principal, task, grant, args.RequestID+":chunks", summary("chunks"), product, ref, chunkCount, requestStarted)
	if err != nil {
		return mcp.ToolResult{}, err
	}
	parts := []downloadPart{documentPart, chunkPart}
	file := kbdownload.File{Ref: ref, Title: title}
	for _, row := range chunkPart.rows {
		if len(row) != len(retrievalDisplayColumns) {
			return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "分块结果行形状无效"}
		}
		index, ok := responseInteger(row[2])
		digest, _ := row[3].(string)
		text, textOK := row[4].(string)
		if !ok || !textOK {
			return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "分块结果行无效"}
		}
		file.Chunks = append(file.Chunks, kbdownload.Chunk{Index: index, TextSHA256: digest, Text: text})
	}
	if includeValues {
		valuesPlan := queryplan.QueryPlan{Product: product.Products.Values, Columns: []string{"field", "value_type", "canonical_value"},
			Filters: []queryplan.Filter{{Column: "doc_id", Op: "=", Value: ref.DocID}, {Column: "version", Op: "=", Value: versionLiteral}}}
		valuesPart, err := s.runDownloadPlan(ctx, principal, task, grant, args.RequestID+":values", summary("values"), valuesPlan, requestStarted)
		if err != nil {
			return mcp.ToolResult{}, err
		}
		parts = append(parts, valuesPart)
		file.Values = []kbdownload.Value{}
		for _, row := range valuesPart.rows {
			if len(row) != 3 {
				return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "抽取值结果行形状无效"}
			}
			file.Values = append(file.Values, kbdownload.Value{Field: fmt.Sprint(row[0]), ValueType: fmt.Sprint(row[1]), CanonicalValue: fmt.Sprint(row[2])})
		}
	}
	content, err := kbdownload.Assemble(file)
	if err != nil {
		return mcp.ToolResult{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "已结算的结果无法组装为文件：" + err.Error()}
	}
	mediaType, fileName := kbdownload.FileMediaType, ref.FileName()
	if original {
		// Every published cell the original contains has now been settled:
		// the document cells, every chunk and every value. The files part
		// releases the original's media type, size and digest; the object is
		// then read and delivered only if it matches them.
		filesPlan := queryplan.QueryPlan{Product: product.Products.Files, Columns: []string{"media_type", "byte_size", "file_sha256"},
			Filters: []queryplan.Filter{{Column: "doc_id", Op: "=", Value: ref.DocID}, {Column: "version", Op: "=", Value: versionLiteral}}}
		filesPart, err := s.runDownloadPlan(ctx, principal, task, grant, args.RequestID+":files", summary("files"), filesPlan, requestStarted)
		if err != nil {
			return mcp.ToolResult{}, err
		}
		parts = append(parts, filesPart)
		described, err := describedOriginal(filesPart.rows)
		if err != nil {
			return mcp.ToolResult{}, err
		}
		if content, err = s.readOriginalObject(ctx, product, ref, described); err != nil {
			return mcp.ToolResult{}, err
		}
		mediaType, fileName = described.mediaType, ref.DocID+"-v"+strconv.FormatInt(ref.Version, 10)+kbpublish.FileMediaTypes[described.mediaType]
	}
	fileSHA256 := kbdownload.SHA256(content)
	manifest := make([]map[string]any, 0, len(file.Chunks))
	for _, chunk := range file.Chunks {
		manifest = append(manifest, map[string]any{"chunk_index": chunk.Index, "text_sha256": chunk.TextSHA256})
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i]["chunk_index"].(int64) < manifest[j]["chunk_index"].(int64) })
	queries := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		entry := map[string]any{"part": part.name, "row_count": len(part.rows)}
		for _, key := range []string{"query_id", "request_id", "result_id", "status", "receipt", "exposure", "idempotent_replay"} {
			if value, present := part.response[key]; present {
				entry[key] = value
			}
		}
		queries = append(queries, entry)
	}
	structured := map[string]any{
		"tool": downloadToolName, "task_id": task.ID, "request_id": args.RequestID,
		"knowledge_product": product.Name, "doc_id": ref.DocID, "version": ref.Version,
		"file_name": fileName, "media_type": mediaType, "format": args.Format, "file_format": fileFormat(args.Format),
		"file_sha256": fileSHA256, "file_bytes": len(content),
		"chunks": manifest, "values_included": includeValues, "value_count": len(file.Values), "queries": queries,
	}
	resource := mcp.EmbeddedResource{URI: "taskgate://kb/" + product.Name + "/documents/" + url.PathEscape(ref.String()) + "/" + args.Format, MimeType: mediaType}
	if original {
		resource.Blob = content
	} else {
		structured["content"] = string(content)
		resource.Text = string(content)
	}
	if s.deliveryBaseURL != "" && len(s.deliverySigningKey) > 0 {
		expiresAt := s.clock().UTC().Add(s.deliveryTTL)
		if !grant.ExpiresAt.IsZero() && expiresAt.After(grant.ExpiresAt) {
			expiresAt = grant.ExpiresAt.UTC()
		}
		structured["download_url"] = s.documentDownloadURL(task.ID, args.RequestID, product.Name, ref, args.Format, fileSHA256, expiresAt.Unix())
		structured["download_url_expires_at"] = expiresAt
	}
	return mcp.ToolResult{Structured: structured, Resources: []mcp.EmbeddedResource{resource}}, nil
}

// originalDescription is the released files row of a document version.
type originalDescription struct {
	mediaType string
	byteSize  int64
	sha256    string
}

func describedOriginal(rows [][]any) (originalDescription, error) {
	if len(rows) != 1 || len(rows[0]) != 3 {
		return originalDescription{}, &mcp.ToolError{Code: apierr.CodeDocumentNotDownloadable, Message: "该文档没有在任务范围内可见的原件"}
	}
	mediaType, _ := rows[0][0].(string)
	size, ok := responseInteger(rows[0][1])
	digest, _ := rows[0][2].(string)
	if _, admitted := kbpublish.FileMediaTypes[mediaType]; !admitted || !ok || size <= 0 || len(digest) != 64 {
		return originalDescription{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "原件的 files 行无效"}
	}
	return originalDescription{mediaType: mediaType, byteSize: size, sha256: digest}, nil
}

// runDownloadPlan settles one plan part of a download, or replays it when the
// part's request id already has a durable record. A failed part answers with
// its stored refusal rather than a different one on retry.
func (s *Service) runDownloadPlan(ctx context.Context, principal mcp.Principal, task control.Task, grant control.TaskGrant,
	requestID, summary string, plan queryplan.QueryPlan, requestStarted time.Time) (downloadPart, error) {
	if response, replayed, err := s.replayDownloadPart(ctx, principal, task, requestID, summary, requestStarted); replayed || err != nil {
		return response, err
	}
	if err := s.requireDownloadActive(ctx, task); err != nil {
		return downloadPart{}, err
	}
	prepared, err := s.prepareTaskPlan(ctx, task, grant, plan)
	if err != nil {
		return downloadPart{}, toolError(err)
	}
	resultNames := queryPlanResultNames(plan)
	metadata := &queryResponseMetadata{Plan: plan, OutputFormat: "json", DisplayColumns: resultNames,
		ResultOrder: identityResultOrder(len(resultNames)), SemanticColumns: queryPlanSemanticColumns(plan)}
	if prepared.Exposure != nil {
		metadata.PlanDigest = prepared.Exposure.planDigest
		metadata.PredicateFootprint = predicateFootprintResponseFor(prepared.Exposure)
	}
	response, err := s.executeSQL(ctx, principal, task, requestID, prepared, summary, metadata, requestStarted)
	if err != nil {
		return downloadPart{}, err
	}
	return s.settledDownloadPart(ctx, requestID, response)
}

// runDownloadChunks settles the chunk part: a fetch-form retrieval naming every
// chunk index of the document. Its result hook adds the chunk text (checked
// against the released digests) and then requires all chunk_count chunks:
// anything less refuses the whole document before settlement.
func (s *Service) runDownloadChunks(ctx context.Context, principal mcp.Principal, task control.Task, grant control.TaskGrant,
	requestID, summary string, product catalog.KnowledgeProduct, ref kbdownload.Ref, chunkCount int64, requestStarted time.Time) (downloadPart, error) {
	if response, replayed, err := s.replayDownloadPart(ctx, principal, task, requestID, summary, requestStarted); replayed || err != nil {
		return response, err
	}
	if err := s.requireDownloadActive(ctx, task); err != nil {
		return downloadPart{}, err
	}
	request := retrieval.Request{Tool: retrieval.ToolFetch, KnowledgeProduct: product.Name}
	for index := int64(0); index < chunkCount; index++ {
		request.Chunks = append(request.Chunks, retrieval.ChunkRef{DocID: ref.DocID, Version: int(ref.Version), ChunkIndex: int(index)})
	}
	prepared, plan, err := s.prepareRetrieval(grant, request)
	if err != nil {
		return downloadPart{}, err
	}
	prepared.EnrichResult = func(ctx context.Context, stored *storedQueryResult) error {
		if err := s.enrichChunkText(ctx, product, stored); err != nil {
			return err
		}
		if int64(len(stored.Rows)) != chunkCount {
			return &mcp.ToolError{Code: apierr.CodeDocumentNotDownloadable,
				Message: fmt.Sprintf("文档 %s 有 %d 个分块，任务范围内可见 %d 个；整份拒绝，未释放任何分块", ref, chunkCount, len(stored.Rows))}
		}
		return nil
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
	response, err := s.executeSQL(ctx, principal, task, requestID, prepared, summary, metadata, requestStarted)
	if err != nil {
		return downloadPart{}, err
	}
	return s.settledDownloadPart(ctx, requestID, response)
}

func (s *Service) requireDownloadActive(ctx context.Context, task control.Task) error {
	if task.State != control.TaskActive {
		return &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务尚未批准或已经归档"}
	}
	if err := s.ensureActiveTaskFamily(ctx, task); err != nil {
		return toolError(err)
	}
	return nil
}

// replayDownloadPart answers a part whose request id already has a durable
// record: a completed part replays its stored result; a failed part repeats its
// stored refusal.
func (s *Service) replayDownloadPart(ctx context.Context, principal mcp.Principal, task control.Task,
	requestID, summary string, requestStarted time.Time) (downloadPart, bool, error) {
	existing, err := s.store.GetQueryByRequestID(ctx, task.ID, requestID)
	if errors.Is(err, control.ErrNotFound) {
		return downloadPart{}, false, nil
	}
	if err != nil {
		return downloadPart{}, true, err
	}
	if existing.RequestDigest != digest(summary) {
		return downloadPart{}, true, toolError(control.ErrIdempotencyConflict)
	}
	if existing.Status != control.QueryCompleted {
		if existing.ErrorCode == apierr.CodeDocumentNotDownloadable {
			return downloadPart{}, true, &mcp.ToolError{Code: apierr.CodeDocumentNotDownloadable, Message: "该文档此前已因有任务范围外的节被整份拒绝"}
		}
		return downloadPart{}, true, &mcp.ToolError{Code: apierr.CodeConflict, Message: fmt.Sprintf("下载的子查询 %s 此前未完成（%s）；请换 request_id 重试", requestID, existing.ErrorCode)}
	}
	response, err := s.queryReplayResponseAt(ctx, existing, requestStarted, time.Now())
	if err != nil {
		return downloadPart{}, true, err
	}
	part, err := s.settledDownloadPart(ctx, requestID, response)
	return part, true, err
}

// settledDownloadPart reads the canonical rows of a settled part: inline when
// the deployment returns rows in the response, otherwise from its result
// artifact.
func (s *Service) settledDownloadPart(ctx context.Context, requestID string, response any) (downloadPart, error) {
	mapped, ok := response.(map[string]any)
	if !ok {
		return downloadPart{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "下载子查询的结果格式无效"}
	}
	name := requestID[strings.LastIndex(requestID, ":")+1:]
	part := downloadPart{name: name, response: mapped}
	if rows, present := mapped["rows"]; present {
		encoded, err := json.Marshal(rows)
		if err != nil {
			return downloadPart{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "下载子查询的结果行无法读取"}
		}
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		if err := decoder.Decode(&part.rows); err != nil {
			return downloadPart{}, &mcp.ToolError{Code: apierr.CodeConflict, Message: "下载子查询的结果行无法读取"}
		}
		return part, nil
	}
	queryID, _ := mapped["query_id"].(string)
	rows, err := s.artifactRowsByQuery(ctx, queryID)
	if err != nil {
		return downloadPart{}, err
	}
	part.rows = rows
	return part, nil
}

// artifactRowsByQuery reads every row of a completed query's result artifact,
// the same bounded read preview_result performs.
func (s *Service) artifactRowsByQuery(ctx context.Context, queryID string) ([][]any, error) {
	unavailable := &mcp.ToolError{Code: apierr.CodeConflict, Message: "下载子查询的结果不可读"}
	if s.resultArtifacts == nil || queryID == "" {
		return nil, unavailable
	}
	artifact, err := s.store.GetResultArtifactByQuery(ctx, queryID)
	if err != nil {
		return nil, unavailable
	}
	if artifact.Status == control.ResultArtifactPending {
		promoteCtx, cancel := s.artifactOperationContext(ctx)
		artifact, err = s.promoteResultArtifact(promoteCtx, artifact, "gateway-download")
		cancel()
		if err != nil {
			return nil, unavailable
		}
	}
	if artifact.Status != control.ResultArtifactAvailable || (artifact.ExpiresAt != nil && !s.clock().UTC().Before(*artifact.ExpiresAt)) {
		return nil, unavailable
	}
	if err := s.ensureArtifactKeyActive(ctx, artifact); err != nil {
		return nil, unavailable
	}
	if artifact.RowCount == 0 {
		return [][]any{}, nil
	}
	_, schema, err := decodeArtifactMetadata(artifact)
	if err != nil {
		return nil, unavailable
	}
	encoded, err := s.resultArtifacts.ReadParquet(ctx, artifactReference(artifact), s.previewMaxBytes)
	if err != nil {
		return nil, unavailable
	}
	rows, err := resultartifact.ReadParquet(encoded, artifact.ResultID, schema, 0, artifact.RowCount)
	if err != nil {
		return nil, unavailable
	}
	return rows, nil
}

// documentDownloadSignature authenticates a document download capability. The
// payload is domain-separated from the result delivery signature and binds the
// file digest, so the handler serves only the file the tool returned.
func (s *Service) documentDownloadSignature(taskID, requestID, knowledgeProduct string, ref kbdownload.Ref, format, fileSHA256 string, expires int64) string {
	payload := strings.Join([]string{"TASKGATE-KB-DOCUMENT-DOWNLOAD-V1", taskID, requestID, knowledgeProduct,
		ref.String(), format, fileSHA256, strconv.FormatInt(expires, 10)}, "\n")
	mac := hmac.New(sha256.New, s.deliverySigningKey)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Service) documentDownloadURL(taskID, requestID, knowledgeProduct string, ref kbdownload.Ref, format, fileSHA256 string, expires int64) string {
	query := url.Values{
		"task_id": {taskID}, "request_id": {requestID}, "knowledge_product": {knowledgeProduct},
		"doc": {ref.String()}, "format": {format}, "sha256": {fileSHA256}, "expires": {strconv.FormatInt(expires, 10)},
		"token": {s.documentDownloadSignature(taskID, requestID, knowledgeProduct, ref, format, fileSHA256, expires)},
	}
	return s.deliveryBaseURL + "/api/v1/kb/documents/download?" + query.Encode()
}

// DocumentDownloadHandler serves a document file named by a download
// capability. Like result delivery it changes no budget or receipt: the file is
// reassembled from the parts' settled result artifacts and served only when it
// hashes to the digest the capability signs.
func (s *Service) DocumentDownloadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.URL.Query()
		taskID, requestID, knowledgeProduct := values.Get("task_id"), values.Get("request_id"), values.Get("knowledge_product")
		fileSHA256, format := values.Get("sha256"), values.Get("format")
		ref, refErr := kbdownload.ParseRef(values.Get("doc"))
		expires, err := strconv.ParseInt(values.Get("expires"), 10, 64)
		if err != nil || refErr != nil || expires <= s.clock().UTC().Unix() || taskID == "" || requestID == "" || len(s.deliverySigningKey) == 0 ||
			(format != kbdownload.FormatAssembled && format != kbdownload.FormatOriginal) ||
			!hmac.Equal([]byte(values.Get("token")), []byte(s.documentDownloadSignature(taskID, requestID, knowledgeProduct, ref, format, fileSHA256, expires))) {
			http.Error(w, "download capability is invalid or expired", http.StatusUnauthorized)
			return
		}
		select {
		case s.downloadSlots <- struct{}{}:
			defer func() { <-s.downloadSlots }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "result delivery capacity is temporarily exhausted", http.StatusServiceUnavailable)
			return
		}
		content, mediaType, fileName, err := s.reassembleDocument(r.Context(), taskID, requestID, knowledgeProduct, ref, format)
		if err != nil || kbdownload.SHA256(content) != fileSHA256 {
			http.NotFound(w, r)
			return
		}
		digestBytes, _ := hex.DecodeString(fileSHA256)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digestBytes)+":")
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(content)
	})
}

// reassembleDocument rebuilds a delivered file from the durable records of its
// parts, with its media type and file name. Every part must be a completed
// query of the task; an original is read from the originals store again and
// must match the released files row.
func (s *Service) reassembleDocument(ctx context.Context, taskID, requestID, knowledgeProduct string, ref kbdownload.Ref, format string) ([]byte, string, string, error) {
	rowsOf := func(part string, required bool) ([][]any, bool, error) {
		record, err := s.store.GetQueryByRequestID(ctx, taskID, requestID+":"+part)
		if errors.Is(err, control.ErrNotFound) && !required {
			return nil, false, nil
		}
		if err != nil || record.Status != control.QueryCompleted {
			return nil, false, errors.New("download part is not completed")
		}
		if record.RequestDigest != digest(downloadPartDigestSource(part, taskID, knowledgeProduct, ref)) {
			return nil, false, errors.New("download part names another document")
		}
		rows, err := s.artifactRowsByQuery(ctx, record.ID)
		return rows, true, err
	}
	documentRows, _, err := rowsOf("document", true)
	if err != nil || len(documentRows) != 1 || len(documentRows[0]) != 2 {
		return nil, "", "", errors.New("document part unavailable")
	}
	chunkRows, _, err := rowsOf("chunks", true)
	if err != nil {
		return nil, "", "", err
	}
	valueRows, valuesPresent, err := rowsOf("values", false)
	if err != nil {
		return nil, "", "", err
	}
	if format == kbdownload.FormatOriginal {
		filesRows, _, err := rowsOf("files", true)
		if err != nil || !valuesPresent {
			return nil, "", "", errors.New("original parts unavailable")
		}
		described, err := describedOriginal(filesRows)
		if err != nil {
			return nil, "", "", err
		}
		product, found := s.catalog.LookupKnowledgeProduct(knowledgeProduct)
		if !found {
			return nil, "", "", errors.New("unknown knowledge product")
		}
		content, err := s.readOriginalObject(ctx, product, ref, described)
		return content, described.mediaType, ref.DocID + "-v" + strconv.FormatInt(ref.Version, 10) + kbpublish.FileMediaTypes[described.mediaType], err
	}
	title, _ := documentRows[0][0].(string)
	file := kbdownload.File{Ref: ref, Title: title}
	for _, row := range chunkRows {
		if len(row) != len(retrievalDisplayColumns) {
			return nil, "", "", errors.New("chunk row shape")
		}
		index, _ := responseInteger(row[2])
		digestValue, _ := row[3].(string)
		text, _ := row[4].(string)
		file.Chunks = append(file.Chunks, kbdownload.Chunk{Index: index, TextSHA256: digestValue, Text: text})
	}
	if valuesPresent {
		file.Values = []kbdownload.Value{}
		for _, row := range valueRows {
			if len(row) != 3 {
				return nil, "", "", errors.New("value row shape")
			}
			file.Values = append(file.Values, kbdownload.Value{Field: fmt.Sprint(row[0]), ValueType: fmt.Sprint(row[1]), CanonicalValue: fmt.Sprint(row[2])})
		}
	}
	content, err := kbdownload.Assemble(file)
	return content, kbdownload.FileMediaType, ref.FileName(), err
}

func fileFormat(format string) string {
	if format == kbdownload.FormatOriginal {
		return "original"
	}
	return kbdownload.FileVersion
}

// originalReadTimeout bounds the identity read of an original's object key; like
// the chunk text delivery read it is not a governed query.
const originalReadTimeout = 5 * time.Second

// readOriginalObject reads a document version's original from the originals
// store. The object key comes from kb.document_files by identity, the row must
// agree with the released files row, and the object must have exactly the
// released size and digest; anything else refuses delivery.
func (s *Service) readOriginalObject(ctx context.Context, product catalog.KnowledgeProduct, ref kbdownload.Ref, released originalDescription) ([]byte, error) {
	refused := &mcp.ToolError{Code: apierr.CodeConflict, Message: fmt.Sprintf("文档 %s 的原件与发布期 files 行不一致，拒绝交付", ref)}
	if s.kbOriginals == nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "本实例未配置原件存储"}
	}
	collection, err := pgTextLiteral(product.Collection)
	if err != nil || product.Collection == "" {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "知识产品集合名无法编码"}
	}
	docID, err := pgTextLiteral(ref.DocID)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeConflict, Message: "文档号无法编码"}
	}
	statement := "SELECT object_key, media_type, byte_size, file_sha256 FROM kb.document_files WHERE collection = " + collection +
		" AND doc_id = " + docID + " AND version = " + strconv.FormatInt(ref.Version, 10)
	result, err := s.connector.Query(ctx, dataconnector.QueryRequest{SQL: statement, StatementTimeout: originalReadTimeout, MaxRows: 1})
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeDataSourceUnavailable, Message: "读取原件位置失败"}
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 4 {
		return nil, refused
	}
	key, _ := result.Rows[0][0].(string)
	mediaType, _ := result.Rows[0][1].(string)
	size, _ := responseInteger(result.Rows[0][2])
	recorded, _ := result.Rows[0][3].(string)
	if key == "" || mediaType != released.mediaType || size != released.byteSize || recorded != released.sha256 {
		return nil, refused
	}
	reader, err := s.kbOriginals.Get(ctx, key)
	if err != nil {
		return nil, &mcp.ToolError{Code: apierr.CodeDataSourceUnavailable, Message: "读取原件对象失败"}
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, released.byteSize+1))
	if err != nil || int64(len(content)) != released.byteSize || kbdownload.SHA256(content) != released.sha256 {
		return nil, refused
	}
	return content, nil
}
