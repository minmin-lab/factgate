package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/principalattr"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type DataConnector interface {
	Query(context.Context, dataconnector.QueryRequest) (dataconnector.Result, error)
	Ping(context.Context) error
	Attestation(context.Context) (dataconnector.Attestation, error)
}

type Config struct {
	Catalog            *catalog.Catalog
	Store              *control.Store
	Approval           approval.ApprovalAdapter
	ReceiptVerifier    approval.ReceiptVerifier
	QueryReceiptSigner *queryreceipt.Signer
	Connector          DataConnector
	// SnapshotRegistry contains only verified V4 hot indexes. It may be nil for
	// legacy V1--V3 catalogs; every V4 execution fails closed when it is absent.
	SnapshotRegistry *ordinal.Registry
	// PrincipalAttributes is the attested subject-attribute snapshot that
	// Product eligibility predicates are evaluated against. It is required
	// whenever the Catalog declares eligibility on any Product.
	PrincipalAttributes *principalattr.Snapshot
	// KBEmbedder embeds text queries for knowledge-product search. It may be
	// nil, in which case text queries are refused and vector queries still
	// work; a configured embedder is checked against each product's declared
	// model before use.
	KBEmbedder        kbpublish.Embedder
	CallbackSecret    string
	Logger            *slog.Logger
	Clock             func() time.Time
	Background        context.Context
	SettlementTimeout time.Duration
	// SpoolDirectory is the parent for query-private encrypted temporary
	// directories. Empty uses the operating-system temporary directory.
	SpoolDirectory string
	// SpoolThresholdBytes may lower the 128 MiB production boundary (for
	// constrained deployments and tests) but cannot raise it.
	SpoolThresholdBytes int64
	// ResultArtifacts switches successful query results from legacy PostgreSQL
	// ciphertext rows to TaskGate-managed encrypted Parquet objects.
	ResultArtifacts *resultartifact.Manager
	ResultTTL       time.Duration
	// KBOriginals reads document originals for the download task's original
	// format (docs/kb-design.md §12.9); nil when no originals store is
	// configured, and an original download is then refused.
	KBOriginals KBOriginalStore
	// ArtifactOperationTimeout bounds canonical object promotion independently
	// from the short Control PostgreSQL settlement timeout.
	ArtifactOperationTimeout time.Duration
	PreviewMaxBytes          int64
	DeliveryBaseURL          string
	DeliverySigningKey       []byte
	DeliveryTTL              time.Duration
	// DownloadTimeout bounds a complete plaintext delivery independently of the
	// ordinary MCP response timeout. DownloadConcurrency caps simultaneous
	// object-store readers so slow clients cannot exhaust Gateway resources.
	DownloadTimeout     time.Duration
	DownloadConcurrency int
	// OAPublicBaseURL is where requesters open OA drafts. When set, a pending
	// task's status carries its draft link again (approval.DraftPageURL); the
	// draft id is the stored approval reference, so nothing new is persisted.
	OAPublicBaseURL string
}

type Service struct {
	catalog                  *catalog.Catalog
	store                    *control.Store
	approval                 approval.ApprovalAdapter
	receiptVerifier          approval.ReceiptVerifier
	queryReceiptSigner       *queryreceipt.Signer
	connector                DataConnector
	snapshotRegistry         *ordinal.Registry
	principalAttributes      *principalattr.Snapshot
	kbEmbedder               kbpublish.Embedder
	callbackSecret           []byte
	logger                   *slog.Logger
	clock                    func() time.Time
	background               context.Context
	settlementTimeout        time.Duration
	spoolDirectory           string
	spoolThreshold           int64
	resultArtifacts          *resultartifact.Manager
	kbOriginals              KBOriginalStore
	resultTTL                time.Duration
	artifactOperationTimeout time.Duration
	previewMaxBytes          int64
	deliveryBaseURL          string
	deliverySigningKey       []byte
	deliveryTTL              time.Duration
	downloadTimeout          time.Duration
	downloadSlots            chan struct{}
	oaPublicBaseURL          string
	pendingSettles           atomic.Int64
	artifactRecoveryFailures atomic.Int64
	artifactRecoveryRunning  atomic.Bool
	artifactRecoveryMu       sync.Mutex
	// markArtifactAvailable is a narrow fault-injection seam for the crash
	// window after canonical object creation and before the Control AVAILABLE
	// transaction commits. Production always leaves it nil.
	markArtifactAvailable func(context.Context, string, string, string) (control.ResultArtifact, error)
	// beforeReserveBudget is a narrow fault-injection seam for the window
	// between the preparation derivation and the reservation that establishes
	// the authoritative pre-state. Concurrent activity in that window is exactly
	// what the re-derivation exists to survive, and it cannot be provoked from
	// outside: the task lock serializes reservations, so a second request cannot
	// interleave without one. Production always leaves it nil.
	beforeReserveBudget func(context.Context, string)
	// highCardinalityDerivations isolates million-fact bitmap work from the
	// small-query pool. Capacity one is intentional and queue time is outside
	// the advertised execution SLO.
	highCardinalityDerivations chan struct{}
}

type pendingContext struct {
	Products        []string            `json:"products"`
	Columns         map[string][]string `json:"columns"`
	MandatoryScope  map[string]any      `json:"mandatory_scope"`
	Budget          domain.Budget       `json:"budget"`
	Sensitivity     domain.Sensitivity  `json:"sensitivity"`
	DatasourceID    string              `json:"datasource_id"`
	SchemaDigest    string              `json:"schema_digest"`
	ApprovalMode    domain.ApprovalMode `json:"approval_mode"`
	Approver        string              `json:"approver,omitempty"`
	CallbackContext string              `json:"callback_context"`
	// ViewBinding is the immutable evidence behind the digest carried by the
	// signed manifest. It remains in the private pending context so callback
	// activation can persist the exact content-addressed set without exposing
	// physical dependency names to an agent or OA receipt.
	ViewBinding *pendingViewBinding `json:"view_binding,omitempty"`
	// Eligibility is the private record of the envelope compilation that
	// admitted this request before any approver saw it. It is absent on
	// requests whose Products declare no eligibility and on legacy records.
	Eligibility *eligibilityVerdict `json:"eligibility,omitempty"`
	// ApproverEligibility is the same compilation for the route's approver
	// when the route verifies its approver (catalog ApprovalRoute.VerifyApprover).
	ApproverEligibility *eligibilityVerdict `json:"approver_eligibility,omitempty"`
	// VerifyApprover makes the approval callback re-check the actual approver
	// against the data products' rules before any grant is issued: set by a
	// verifying route and always for an approver the requester chose.
	VerifyApprover bool `json:"verify_approver,omitempty"`
	ApproverChosen bool `json:"approver_chosen,omitempty"`
	// Chain is the routing record of an approval-chain task
	// (approval_chain.go); Approver is then its first step's approver.
	Chain *pendingChain `json:"approval_chain,omitempty"`
}

func New(config Config) (*Service, error) {
	if config.Catalog == nil || config.Store == nil || config.Approval == nil || config.Connector == nil || config.CallbackSecret == "" {
		return nil, errors.New("gateway catalog, store, approval, connector, and callback secret are required")
	}
	if catalogDeclaresEligibility(config.Catalog) && config.PrincipalAttributes == nil {
		return nil, errors.New("catalog declares product eligibility but no principal attribute snapshot is configured")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.Background == nil {
		config.Background = context.Background()
	}
	if config.SettlementTimeout <= 0 {
		config.SettlementTimeout = defaultSettlementTimeout
	}
	if config.SpoolThresholdBytes <= 0 {
		config.SpoolThresholdBytes = querySpoolThreshold
	} else if config.SpoolThresholdBytes > querySpoolThreshold {
		config.SpoolThresholdBytes = querySpoolThreshold
	}
	if config.ResultTTL < 0 {
		return nil, errors.New("result TTL cannot be negative")
	}
	if config.ArtifactOperationTimeout <= 0 {
		config.ArtifactOperationTimeout = 30 * time.Minute
	}
	if config.PreviewMaxBytes <= 0 {
		config.PreviewMaxBytes = 64 << 20
	}
	if config.DeliveryTTL <= 0 {
		config.DeliveryTTL = 5 * time.Minute
	}
	if config.DownloadTimeout <= 0 {
		config.DownloadTimeout = 30 * time.Minute
	}
	if config.DownloadConcurrency <= 0 {
		config.DownloadConcurrency = 4
	}
	if config.ResultArtifacts != nil {
		if strings.TrimSpace(config.DeliveryBaseURL) == "" {
			config.DeliveryBaseURL = "http://127.0.0.1:8082"
		}
		if len(config.DeliverySigningKey) == 0 {
			config.DeliverySigningKey = []byte(config.CallbackSecret)
		}
	}
	if config.ReceiptVerifier == nil {
		// The demo derives a stable Ed25519 key from its existing secret so old
		// compose environments keep working. Deployments can supply a public-key
		// verifier independently of the HMAC transport secret.
		config.ReceiptVerifier = approval.DemoReceiptVerifier([]byte(config.CallbackSecret))
	}
	if config.QueryReceiptSigner == nil {
		// Tests and the self-contained demo get a deterministic fallback. The
		// production entry point supplies an independently configured key.
		config.QueryReceiptSigner = queryreceipt.DemoSigner([]byte(config.CallbackSecret))
	}
	service := &Service{
		catalog: config.Catalog, store: config.Store, approval: config.Approval,
		receiptVerifier:    config.ReceiptVerifier,
		queryReceiptSigner: config.QueryReceiptSigner, connector: config.Connector,
		snapshotRegistry: config.SnapshotRegistry, principalAttributes: config.PrincipalAttributes, kbEmbedder: config.KBEmbedder,
		callbackSecret: []byte(config.CallbackSecret), logger: config.Logger,
		clock: config.Clock, background: config.Background, settlementTimeout: config.SettlementTimeout,
		spoolDirectory:             config.SpoolDirectory,
		spoolThreshold:             config.SpoolThresholdBytes,
		resultArtifacts:            config.ResultArtifacts,
		kbOriginals:                config.KBOriginals,
		resultTTL:                  config.ResultTTL,
		artifactOperationTimeout:   config.ArtifactOperationTimeout,
		previewMaxBytes:            config.PreviewMaxBytes,
		deliveryBaseURL:            strings.TrimRight(config.DeliveryBaseURL, "/"),
		deliverySigningKey:         append([]byte(nil), config.DeliverySigningKey...),
		deliveryTTL:                config.DeliveryTTL,
		downloadTimeout:            config.DownloadTimeout,
		downloadSlots:              make(chan struct{}, config.DownloadConcurrency),
		highCardinalityDerivations: make(chan struct{}, 1),
		oaPublicBaseURL:            strings.TrimRight(config.OAPublicBaseURL, "/"),
	}
	// The first background reconciliation clears this gate. Liveness can start
	// immediately, but readiness must not race ahead of durable PENDING intents.
	if config.ResultArtifacts != nil {
		service.artifactRecoveryRunning.Store(true)
	}
	return service, nil
}

// ReadyError reports durable query-budget settlements that have not yet been
// persisted. While this is non-nil, accepting more queries could hide an
// outstanding reservation or temporarily overstate the remaining budget.
func (s *Service) ReadyError() error {
	if s == nil {
		return errors.New("gateway service is nil")
	}
	if pending := s.pendingSettles.Load(); pending > 0 {
		return fmt.Errorf("%d query budget settlement(s) pending", pending)
	}
	if failed := s.artifactRecoveryFailures.Load(); failed > 0 {
		return fmt.Errorf("%d pending result artifact recovery failure(s)", failed)
	}
	if s.artifactRecoveryRunning.Load() {
		return errors.New("pending result artifact recovery is in progress")
	}
	if s.resultArtifacts != nil {
		ctx, cancel := context.WithTimeout(s.background, 2*time.Second)
		defer cancel()
		if err := s.resultArtifacts.Ready(ctx); err != nil {
			return fmt.Errorf("result object store unavailable: %w", err)
		}
	}
	return nil
}

func randomID(prefix string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(buffer)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "工具参数不符合契约"}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "工具参数包含多余内容"}
	}
	return nil
}

func toolError(err error) error {
	if err == nil {
		return nil
	}
	var already *mcp.ToolError
	if errors.As(err, &already) {
		return already
	}
	var policyErr *sqlpolicy.PolicyError
	if errors.As(err, &policyErr) {
		return &mcp.ToolError{Code: string(policyErr.Code), Message: policyErr.Error()}
	}
	var connectorErr *dataconnector.Error
	if errors.As(err, &connectorErr) {
		return &mcp.ToolError{Code: string(connectorErr.Code), Message: connectorErr.Error()}
	}
	var appErr *apierr.Error
	if errors.As(err, &appErr) {
		return &mcp.ToolError{Code: appErr.Code, Message: appErr.Message}
	}
	switch control.CodeOf(err) {
	case control.CodeNotFound:
		return &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的任务或凭证不存在"}
	case control.CodeTaskNotActive, control.CodeTaskExpired:
		return &mcp.ToolError{Code: apierr.CodeTaskNotActive, Message: "任务当前不可查询；请检查审批、期限或归档状态"}
	case control.CodeBudgetExhausted:
		return &mcp.ToolError{Code: apierr.CodeBudgetExhausted, Message: "任务预算已耗尽并已归档"}
	case control.CodeExposureBudgetExhausted:
		return &mcp.ToolError{Code: apierr.CodeExposureBudgetExhausted, Message: "查询结果会超过根任务的数据暴露预算，因此未释放"}
	case control.CodeExposureEvidenceRequired:
		return &mcp.ToolError{Code: apierr.CodeExposureEvidenceRequired, Message: "该任务要求使用可生成精确暴露证据的结构化查询计划"}
	case control.CodeViewSemanticChanged:
		return &mcp.ToolError{Code: string(dataconnector.CodeViewSemanticChanged), Message: "语义 View 已变化；历史结果仍可重放，但新查询必须创建新任务并重新审批"}
	case control.CodeQueryInProgress:
		return queryInProgressError()
	case control.CodeInvalid, control.CodeInvalidStateChange:
		return &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "请求不符合任务状态或数据契约"}
	case control.CodeIdempotencyConflict:
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "幂等键已用于不同请求"}
	case control.CodeConflict, control.CodeCallbackInProgress:
		return &mcp.ToolError{Code: apierr.CodeConflict, Message: "请求与当前状态冲突；请重试或刷新状态"}
	}
	return &mcp.ToolError{Code: apierr.CodeInternal, Message: "请求处理失败；请使用 trace_id 联系管理员", Cause: err}
}

// queryInProgressError is the refusal of a second reservation while the
// task's one in-flight query is outstanding (threat-model: one in-flight query
// per task). Nothing is recorded for the refused request, so the same
// request_id may be retried; the code stays CONFLICT, which harnesses retry on.
func queryInProgressError() error {
	return &mcp.ToolError{Code: apierr.CodeConflict,
		Message: "同一任务已有查询正在执行；每个任务同一时刻只允许一个在途查询，待其返回后可用同一 request_id 重试（委托子任务各有独立名额）",
		Details: map[string]any{"reason": "QUERY_IN_PROGRESS", "retryable": true, "same_request_id_ok": true, "slot": "task"}}
}

func forbidden() error {
	return &mcp.ToolError{Code: apierr.CodeForbidden, Message: "当前身份无权执行此工具"}
}

func notFound() error {
	return &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的任务或凭证不存在"}
}

func (s *Service) ownedTask(ctx context.Context, principal mcp.Principal, taskID string) (control.Task, error) {
	task, err := s.store.GetTask(ctx, taskID)
	if err != nil {
		return control.Task{}, toolError(err)
	}
	if principal.Role != "query" || task.PrincipalID != principal.ID {
		return control.Task{}, notFound()
	}
	return task, nil
}

func publicTask(task control.Task) map[string]any {
	return map[string]any{
		"task_id": task.ID, "objective": task.Objective, "state": task.State,
		"terminal_reason": task.TerminalReason, "catalog_version": task.CatalogVersion,
		"sensitivity": task.Sensitivity, "created_at": task.CreatedAt,
		"updated_at": task.UpdatedAt, "expires_at": task.ExpiresAt,
		"root_task_id": task.RootTaskID, "parent_task_id": task.ParentTaskID,
	}
}

func publicBudget(snapshot control.BudgetSnapshot) map[string]any {
	return map[string]any{
		"limits":     map[string]any{"queries": snapshot.Limits.Queries, "rows": snapshot.Limits.Rows, "db_ms": snapshot.Limits.DBMS},
		"used":       map[string]any{"queries": snapshot.Usage.UsedQueries, "rows": snapshot.Usage.UsedRows, "db_ms": snapshot.Usage.UsedDBMS},
		"reserved":   map[string]any{"queries": snapshot.Usage.ReservedQueries, "rows": snapshot.Usage.ReservedRows, "db_ms": snapshot.Usage.ReservedDBMS},
		"remaining":  map[string]any{"queries": snapshot.Remaining().Queries, "rows": snapshot.Remaining().Rows, "db_ms": snapshot.Remaining().DBMS},
		"updated_at": snapshot.UpdatedAt,
	}
}
