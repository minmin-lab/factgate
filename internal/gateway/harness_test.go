package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

type gatewayTestClock struct{ value time.Time }

func (clock *gatewayTestClock) Now() time.Time { return clock.value }

type fakeApproval struct {
	requests []approval.DraftRequest
	response approval.DraftResponse
	err      error
}

func (adapter *fakeApproval) CreateDraft(_ context.Context, request approval.DraftRequest) (approval.DraftResponse, error) {
	adapter.requests = append(adapter.requests, request)
	if adapter.err != nil {
		return approval.DraftResponse{}, adapter.err
	}
	response := adapter.response
	if response.DraftID == "" {
		response.DraftID = fmt.Sprintf("draft-%d", len(adapter.requests))
	}
	if response.URL == "" {
		response.URL = "http://oa.test/drafts/" + response.DraftID
	}
	if response.State == "" {
		response.State = "draft"
	}
	return response, nil
}

type fakeConnector struct {
	requests          []dataconnector.QueryRequest
	deadlineRemaining []time.Duration
	result            dataconnector.Result
	provenanceResult  dataconnector.Result
	queryErr          error
	pingErr           error
	attestation       dataconnector.Attestation
	attestationErr    error
	started           chan struct{}
	release           <-chan struct{}
	startOnce         sync.Once
}

func (connector *fakeConnector) Query(ctx context.Context, request dataconnector.QueryRequest) (dataconnector.Result, error) {
	connector.requests = append(connector.requests, request)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	if connector.started != nil {
		connector.startOnce.Do(func() { close(connector.started) })
	}
	if connector.release != nil {
		select {
		case <-connector.release:
		case <-ctx.Done():
			return dataconnector.Result{}, &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}
		}
	}
	return connector.result, connector.queryErr
}

func (connector *fakeConnector) QueryPair(ctx context.Context, request dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error) {
	connector.requests = append(connector.requests, request.Visible, request.Provenance)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	if connector.started != nil {
		connector.startOnce.Do(func() { close(connector.started) })
	}
	if connector.release != nil {
		select {
		case <-connector.release:
		case <-ctx.Done():
			return dataconnector.QueryPairResult{}, &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}
		}
	}
	if connector.queryErr != nil {
		return dataconnector.QueryPairResult{}, connector.queryErr
	}
	provenance := connector.provenanceResult
	if provenance.Columns == nil {
		provenance = connector.result
	}
	return dataconnector.QueryPairResult{Visible: connector.result, Provenance: provenance}, nil
}

func (connector *fakeConnector) QueryPairStream(ctx context.Context, request dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error) {
	connector.requests = append(connector.requests, request.Visible, request.Provenance)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	if connector.started != nil {
		connector.startOnce.Do(func() { close(connector.started) })
	}
	if connector.release != nil {
		select {
		case <-connector.release:
		case <-ctx.Done():
			return dataconnector.QueryPairStreamResult{}, &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}
		}
	}
	if connector.queryErr != nil {
		return dataconnector.QueryPairStreamResult{}, connector.queryErr
	}
	provenance := connector.provenanceResult
	if provenance.Columns == nil {
		provenance = connector.result
	}
	if sink, ok := request.ProvenanceSink.(dataconnector.VisibleResultSink); ok {
		if err := sink.VisibleResult(ctx, connector.result); err != nil {
			return dataconnector.QueryPairStreamResult{}, err
		}
	}
	if err := request.ProvenanceSink.Begin(ctx, provenance.Columns); err != nil {
		return dataconnector.QueryPairStreamResult{}, err
	}
	for _, row := range provenance.Rows {
		if err := request.ProvenanceSink.Row(ctx, row); err != nil {
			return dataconnector.QueryPairStreamResult{}, err
		}
	}
	return dataconnector.QueryPairStreamResult{Visible: connector.result, Provenance: dataconnector.StreamResult{
		Columns: provenance.Columns, RowCount: provenance.RowCount, DatabaseTime: provenance.DatabaseTime,
		Truncated: provenance.Truncated,
	}}, nil
}

func (connector *fakeConnector) Ping(context.Context) error { return connector.pingErr }

func (connector *fakeConnector) Attestation(context.Context) (dataconnector.Attestation, error) {
	if connector.pingErr != nil {
		return dataconnector.Attestation{}, connector.pingErr
	}
	if connector.attestationErr != nil {
		return dataconnector.Attestation{}, connector.attestationErr
	}
	return connector.attestation, nil
}

type gatewayHarness struct {
	service   *Service
	store     *control.Store
	catalog   *catalog.Catalog
	approval  *fakeApproval
	connector *fakeConnector
	clock     *gatewayTestClock
	alice     mcp.Principal
	carol     mcp.Principal
	secret    string
	// dsn is the isolated migrated schema this harness's store was opened on.
	// Tests that must observe or perturb Control state the store deliberately
	// exposes no API for open their own connection to it.
	dsn string
}

// controlDB opens a second connection to this harness's Control schema.
func (harness *gatewayHarness) controlDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", harness.dsn)
	if err != nil {
		t.Fatalf("open control schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newGatewayHarness(t *testing.T) *gatewayHarness {
	t.Helper()
	ctx := context.Background()
	loadedCatalog, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("create result cipher: %v", err)
	}
	clock := &gatewayTestClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	dsn := testpostgres.SchemaDSN(t)
	store, err := control.Open(ctx, dsn, cipher, control.WithClock(clock))
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	alice := mcp.Principal{ID: "principal-alice", Subject: "alice", Role: "query"}
	carol := mcp.Principal{ID: "principal-carol", Subject: "carol", Role: "auditor"}
	for _, principal := range []mcp.Principal{alice, carol} {
		if err := store.CreatePrincipal(ctx, control.Principal{
			ID: principal.ID, Subject: principal.Subject, Role: principal.Role, CreatedAt: clock.value,
		}); err != nil {
			t.Fatalf("create principal %s: %v", principal.Subject, err)
		}
	}

	approvalAdapter := &fakeApproval{}
	connector := &fakeConnector{attestation: testCatalogAttestation(t, loadedCatalog), result: dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{"sensitive-row", 123.45}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}}
	secret := "gateway-callback-test-secret"
	background, cancelBackground := context.WithCancel(context.Background())
	t.Cleanup(cancelBackground)
	service, err := New(Config{
		Catalog: loadedCatalog, Store: store, Approval: approvalAdapter,
		Connector: connector, CallbackSecret: secret,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.Now, Background: background,
	})
	if err != nil {
		t.Fatalf("create gateway service: %v", err)
	}
	return &gatewayHarness{
		service: service, store: store, catalog: loadedCatalog,
		approval: approvalAdapter, connector: connector, clock: clock,
		alice: alice, carol: carol, secret: secret, dsn: dsn,
	}
}

func testCatalogAttestation(t *testing.T, loadedCatalog *catalog.Catalog) dataconnector.Attestation {
	t.Helper()
	if len(loadedCatalog.Sources) != 1 {
		t.Fatalf("test catalog sources = %d, want 1", len(loadedCatalog.Sources))
	}
	source := loadedCatalog.Sources[0]
	if source.SchemaDigest == "" {
		t.Fatal("test catalog source is missing schema_digest")
	}
	return dataconnector.Attestation{
		DatasourceID: source.DatasourceID, Database: source.Database, User: source.User,
		PostgreSQLMajorVersion: source.PostgreSQLMajorVersion, SchemaDigest: source.SchemaDigest,
	}
}

func callGatewayTool(service *Service, principal mcp.Principal, name string, arguments any) (map[string]any, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	result, err := service.CallTool(context.Background(), principal, name, raw)
	if err != nil {
		return nil, err
	}
	structured, ok := result.Structured.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %s returned %T, want map[string]any", name, result.Structured)
	}
	return structured, nil
}

func mustCallGatewayTool(t *testing.T, service *Service, principal mcp.Principal, name string, arguments any) map[string]any {
	t.Helper()
	result, err := callGatewayTool(service, principal, name, arguments)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func requireToolCode(t *testing.T, err error, code string) {
	t.Helper()
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error = %T %v, want *mcp.ToolError", err, err)
	}
	if toolErr.Code != code {
		t.Fatalf("tool error code = %q, want %q (error: %v)", toolErr.Code, code, err)
	}
}

func (harness *gatewayHarness) createActiveSummaryTask(t *testing.T, taskID string) {
	harness.createSummaryTaskWithGrant(t, taskID, nil)
}

func (harness *gatewayHarness) createNarrowedSummaryTask(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1)) {
	t.Helper()
	if narrow == nil {
		t.Fatal("narrowing function is required")
	}
	harness.createSummaryTaskWithGrant(t, taskID, narrow)
}

func (harness *gatewayHarness) createSummaryTaskWithGrant(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1)) {
	harness.createSummaryTaskWithGrantAndExposure(t, taskID, narrow, control.ExposureLimits{})
}

func (harness *gatewayHarness) createExposureSummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposure(t, taskID, nil, limits)
}

func (harness *gatewayHarness) createSummaryTaskWithGrantAndExposure(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, narrow, exposureLimits, "taskgate-exposure-v1")
}

func (harness *gatewayHarness) createExposureV2SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, "taskgate-exposure-v2")
}

func (harness *gatewayHarness) createExposureV3SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, exposure.ProfileV3)
}

func (harness *gatewayHarness) createExposureV4SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, exposure.ProfileV4)
}

func (harness *gatewayHarness) createExposureV5SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, exposure.ProfileV5)
}

func (harness *gatewayHarness) installCatalogV4SnapshotRegistry(t *testing.T) map[string]ordinal.SnapshotIndex {
	t.Helper()
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("create snapshot registry: %v", err)
	}
	indexes := make(map[string]ordinal.SnapshotIndex, len(harness.catalog.SnapshotPublications))
	for _, publication := range harness.catalog.SnapshotPublications {
		path := filepath.Join("..", "..", "config", "snapshots", publication.Name+".json")
		file, openErr := os.Open(path)
		if openErr != nil {
			t.Fatalf("open snapshot compiler input %s: %v", publication.Name, openErr)
		}
		input, decodeErr := snapshotbundle.DecodeCompilerInput(file)
		closeErr := file.Close()
		if decodeErr != nil {
			t.Fatalf("decode snapshot compiler input %s: %v", publication.Name, decodeErr)
		}
		if closeErr != nil {
			t.Fatalf("close snapshot compiler input %s: %v", publication.Name, closeErr)
		}
		var parsed *ordinal.HotDictionary
		if prebuiltDir := strings.TrimSpace(os.Getenv("TASKGATE_GATEWAY_PREBUILT_SNAPSHOT_DIR")); prebuiltDir != "" {
			// The evaluation gate compiles every publication once, from source,
			// in the dedicated snapshot-index Compose services (each in its own
			// memory cgroup) and shares the result on the snapshot-index volume.
			// Re-parsing that already-pinned bundle activates exactly what the
			// Gateway loads, without a second in-process compile whose scale-e7
			// transient (~26 GiB) OOMs the shared host. The digest assertions
			// below still bind it to the Catalog and the committed compiler input.
			parsed = loadPrebuiltSnapshotIndex(t, prebuiltDir, publication.Name, publication.ManifestDigest)
		} else {
			if len(input.Snapshot.Rows) == 0 {
				// The committed compiler input carries no rows: the source rows for
				// this publication live in the Business database rather than in the
				// repository. Scan them the way cmd/snapshot-index does, so the test
				// consumes the same bundle production would activate.
				if !fullSnapshotRegistryRequested() {
					continue
				}
				input = scanLiveSnapshotRows(t, input, publication.Name)
			}
			bundle, compileErr := snapshotbundle.Compile(input)
			if compileErr != nil {
				t.Fatalf("compile snapshot publication %s: %v", publication.Name, compileErr)
			}
			var parseErr error
			parsed, parseErr = ordinal.ParseHotDictionary(bundle.Hot, publication.ManifestDigest)
			if parseErr != nil {
				t.Fatalf("parse snapshot publication %s: %v", publication.Name, parseErr)
			}
		}
		// The compiler input declares what the bundle must digest to, and the
		// Catalog declares what the Gateway will accept. Both are checked: a
		// fixture that matched neither would be a hand-written double again.
		assertCompiledBundleMatchesExpectedDigests(t, publication.Name, parsed.Manifest(),
			parsed.ManifestDigest(), input.ExpectedDigests)
		var index ordinal.SnapshotIndex = parsed
		if index.DictionaryDigest() != publication.DictionaryDigest ||
			index.Manifest().SidecarDigest != publication.SidecarDigest {
			t.Fatalf("snapshot publication %s does not match Catalog", publication.Name)
		}
		if err := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: harness.catalog.SHA256, PublicationName: publication.Name,
		}, publication.ManifestDigest, index); err != nil {
			t.Fatalf("register snapshot publication %s: %v", publication.Name, err)
		}
		if err := harness.store.PutOrdinalSnapshotPublication(context.Background(), publication.ManifestDigest, index, nil); err != nil {
			t.Fatalf("persist snapshot publication %s: %v", publication.Name, err)
		}
		indexes[publication.Name] = index
	}
	harness.service.snapshotRegistry = registry
	return indexes
}

// loadPrebuiltSnapshotIndex parses a publication bundle the evaluation gate
// already produced on the shared snapshot-index volume, the same layout and
// primitives cmd/gateway's loader uses (<dir>/<name>/<name>.bundle.json plus
// the HOT file the manifest names). ParseHotDictionary re-checks the manifest
// digest, and the caller's assertions bind the result to the Catalog and the
// committed compiler input, so this is the compile path's proof without its
// in-process compile. Used only when TASKGATE_GATEWAY_PREBUILT_SNAPSHOT_DIR is
// set (the Compose gate); host runs still compile from source.
func loadPrebuiltSnapshotIndex(t *testing.T, baseDir, name, manifestDigest string) *ordinal.HotDictionary {
	t.Helper()
	directory := filepath.Join(baseDir, name)
	manifestBytes, err := os.ReadFile(filepath.Join(directory, name+".bundle.json"))
	if err != nil {
		t.Fatalf("read prebuilt bundle manifest %s: %v", name, err)
	}
	bundleManifest, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("decode prebuilt bundle manifest %s: %v", name, err)
	}
	hotBytes, err := os.ReadFile(filepath.Join(directory, bundleManifest.Hot.Name))
	if err != nil {
		t.Fatalf("read prebuilt HOT artifact %s: %v", name, err)
	}
	parsed, err := ordinal.ParseHotDictionary(hotBytes, manifestDigest)
	if err != nil {
		t.Fatalf("parse prebuilt HOT artifact %s: %v", name, err)
	}
	return parsed
}

func (harness *gatewayHarness) createSummaryTaskWithGrantAndExposureProfile(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits, profile string) {
	harness.createTaskWithGrantAndExposureProfile(t, taskID, narrow, exposureLimits, profile,
		[]string{"expense_summary"}, map[string][]string{"expense_summary": {"month", "total_amount"}}, domain.SensitivityLow)
}

func (harness *gatewayHarness) createTaskWithGrantAndExposureProfile(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits, profile string, products []string, columns map[string][]string, sensitivity domain.Sensitivity) {
	t.Helper()
	budget := domain.Budget{
		MaxQueries: 10, MaxRows: 500, MaxDBTime: 30 * time.Second,
		PerQueryTimeout: 5 * time.Second, TaskTTL: 30 * time.Minute,
	}
	if exposureLimits.ReleaseFacts > 0 || exposureLimits.InfluenceFacts > 0 {
		budget.MaxReleaseFacts = exposureLimits.ReleaseFacts
		budget.MaxInfluenceFacts = exposureLimits.InfluenceFacts
		budget.MaxOutcomeFacts = exposureLimits.OutcomeFacts
		budget.ExposureProfileVersion = profile
		if profile == exposure.ProfileV5 {
			budget.PredicateFootprint = &domain.PredicateFootprintLimitsV1{Version: domain.PredicateFootprintV1,
				MaxRawLiteralsPerQuery: 1000, MaxUniqueAtomsPerQuery: 9, MaxAtomPayloadBytes: 4096,
				MaxTotalAtomPayloadBytes: 36864}
		}
	}
	pendingValue := pendingContext{
		Products:       append([]string(nil), products...),
		Columns:        columns,
		MandatoryScope: map[string]any{"department": []any{"销售部"}}, Budget: budget, Sensitivity: sensitivity,
		DatasourceID: harness.connector.attestation.DatasourceID, SchemaDigest: harness.connector.attestation.SchemaDigest,
		ApprovalMode: domain.ApprovalModeManual, Approver: "bob", CallbackContext: "seed-context-" + taskID,
	}
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: taskID,
		HumanSubject: harness.alice.Subject, AgentID: harness.alice.ID,
		DeclaredObjective: "summarize travel expenses", Products: pendingValue.Products,
		ApprovedColumns: pendingValue.Columns, MandatoryScope: pendingValue.MandatoryScope,
		Sensitivity: pendingValue.Sensitivity,
		Budget: approval.AuthorizationBudgetV1{
			MaxQueries: budget.MaxQueries, MaxResultRows: budget.MaxRows, MaxDBMS: budget.MaxDBTime.Milliseconds(),
			PerQueryTimeoutMS: budget.PerQueryTimeout.Milliseconds(), TaskTTLMS: budget.TaskTTL.Milliseconds(),
			MaxReleaseFacts: budget.MaxReleaseFacts, MaxInfluenceFacts: budget.MaxInfluenceFacts,
			MaxOutcomeFacts:        budget.MaxOutcomeFacts,
			ExposureProfileVersion: budget.ExposureProfileVersion,
			PredicateFootprint:     cloneDomainPredicateFootprint(budget.PredicateFootprint),
		},
		CatalogVersion: harness.catalog.CatalogVersion, CatalogSHA256: harness.catalog.SHA256,
		DatasourceID:    harness.connector.attestation.DatasourceID,
		SchemaDigest:    harness.connector.attestation.SchemaDigest,
		CallbackContext: pendingValue.CallbackContext, Nonce: "00000000000000000000000000000001",
	}
	manifestDigest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatalf("hash authorization manifest: %v", err)
	}
	pending, err := json.Marshal(persistedPendingContext{
		pendingContext: pendingValue, Manifest: manifest, ManifestDigest: manifestDigest,
	})
	if err != nil {
		t.Fatalf("marshal pending context: %v", err)
	}
	if err := harness.store.CreateTask(context.Background(), control.Task{
		ID: taskID, PrincipalID: harness.alice.ID, Objective: "summarize travel expenses",
		State: control.TaskAwaitingApproval, CatalogVersion: harness.catalog.CatalogVersion,
		Sensitivity:    string(sensitivity),
		RequestContext: pending, ApprovalRef: "seed-draft-" + taskID,
		CreatedAt: harness.clock.value, UpdatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create active-task seed: %v", err)
	}
	core, err := domain.CoreFromManifest(manifest, manifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build grant core: %v", err)
	}
	decision := approval.ApprovalDecisionApprove
	if narrow != nil {
		narrow(&core)
		if err := core.Validate(); err != nil {
			t.Fatalf("validate narrowed grant core: %v", err)
		}
		decision = approval.ApprovalDecisionNarrow
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("hash grant core: %v", err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "seed-receipt-" + taskID,
		TaskID: taskID, Decision: decision, ManifestDigest: manifestDigest,
		ApprovedGrantDigest: coreDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign approval receipt: %v", err)
	}
	finalGrantJSON, err := approval.EncodeTaskGrantV1(approval.TaskGrantV1{
		Version: domain.TaskGrantV1Version, Core: core, ApprovalReceipt: receipt,
	})
	if err != nil {
		t.Fatalf("encode final grant: %v", err)
	}
	_, err = harness.store.ApplyApprovalCallback(context.Background(), control.ApprovalCallback{
		EventID: "seed-event-" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		Event: control.ApprovalEvent{
			TaskID: taskID, Actor: "bob", Decision: "approved",
			Payload: json.RawMessage(`{"source":"test"}`), CreatedAt: harness.clock.value,
		},
		ExpectedState: control.TaskAwaitingApproval, NewState: control.TaskActive,
		Grant: &control.TaskGrant{
			TaskID: taskID, Subject: harness.alice.Subject, Purpose: "summarize travel expenses",
			ApprovedProducts: append([]string(nil), products...),
			ApprovedColumns:  columns,
			MandatoryScope:   json.RawMessage(`{"department":["销售部"]}`), SensitivityCeiling: string(sensitivity),
			Budget: control.BudgetLimits{
				Queries: core.Budget.MaxQueries, Rows: core.Budget.MaxResultRows, DBMS: core.Budget.MaxDBMS,
			},
			Exposure: control.ExposureGrant{
				Limits: control.ExposureLimits{ReleaseFacts: core.Budget.MaxReleaseFacts,
					InfluenceFacts: core.Budget.MaxInfluenceFacts, OutcomeFacts: core.Budget.MaxOutcomeFacts},
				ProfileVersion:     core.Budget.ExposureProfileVersion,
				PredicateFootprint: controlPredicateFootprint(core.Budget.PredicateFootprint),
			},
			ExpiresAt: core.ExpiresAt, CatalogVersion: harness.catalog.CatalogVersion,
			CatalogDigest: core.CatalogSHA256, DatasourceID: core.DatasourceID, SchemaDigest: core.SchemaDigest,
			ApprovalReceipt: finalGrantJSON, CreatedAt: harness.clock.value,
		},
		Response: []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("approve active-task seed: %v", err)
	}
}

// liveSnapshotBundles caches one compiled bundle per publication for the whole
// test binary.
//
// Scanning fifty thousand source rows and compiling them is real work, and five
// tests install the same registry. The cache is keyed by publication name and
// holds the verified compiler input rather than the parsed index, because
// ParseHotDictionary hands out a live structure that a test could mutate.
var liveSnapshotBundles sync.Map

// fullSnapshotRegistryRequested reports whether this run should materialise the
// publications whose committed input carries no rows.
//
// Those are scanned out of the Business database and compiled: five of the
// Catalog's seven publications take that path, measured at 25.84 GB peak on a
// 30 GB host. The cases that genuinely need them prepare ordinal-program plans
// and live behind taskgate_scale, so the acceptance run installs only the
// publications that carry committed rows.
func fullSnapshotRegistryRequested() bool {
	return scaleLaneBuild ||
		strings.TrimSpace(os.Getenv("TASKGATE_GATEWAY_FULL_SNAPSHOT_REGISTRY")) != ""
}

// scanLiveSnapshotRows fills a compiler input's rows from the Business database.
//
// The publication's source rows are deliberately not committed: they are fifty
// thousand rows of generated ProvSQL data whose place is the database, not the
// repository. cmd/snapshot-index scans them the same way, so a test that scans
// them here consumes the bundle production would activate rather than a
// hand-written double whose manifest carries no cold payload, no hot index and
// no segments.
func scanLiveSnapshotRows(t *testing.T, input snapshotbundle.CompilerInput, name string) snapshotbundle.CompilerInput {
	t.Helper()
	if cached, found := liveSnapshotBundles.Load(name); found {
		return cached.(snapshotbundle.CompilerInput)
	}
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skipf("publication %s carries no committed source rows; set BUSINESS_TEST_POSTGRES_DSN "+
			"(scripts/db-test-env.sh) so the real bundle can be materialized", name)
	}
	scanned, err := snapshotbundle.ScanPostgresSnapshot(t.Context(), input, dsn)
	if err != nil {
		t.Fatalf("scan source relation %s for publication %s: %v", input.SourceRelation, name, err)
	}
	if len(scanned.Snapshot.Rows) == 0 {
		t.Fatalf("publication %s scanned no rows from %s; the Business deployment does not carry its source data",
			name, input.SourceRelation)
	}
	liveSnapshotBundles.Store(name, scanned)
	return scanned
}

// assertCompiledBundleMatchesExpectedDigests proves the materialized bundle is
// the one the repository and the Catalog already committed to.
//
// The compiler input's expected_digests were produced by a verified publication
// run. Checking them here is what makes the fixture evidence rather than
// whatever the current code happens to emit: if the compiler, the dictionary
// layout or the source rows change, this fails instead of silently activating a
// different bundle under the same publication name.
func assertCompiledBundleMatchesExpectedDigests(t *testing.T, name string, manifest ordinal.DictionaryManifest,
	manifestDigest string, expected snapshotbundle.ExpectedDigests) {
	t.Helper()
	for _, digest := range []struct{ label, want, got string }{
		{"sidecar", expected.SidecarDigest, manifest.SidecarDigest},
		{"dictionary", expected.DictionaryDigest, manifest.DictionaryDigest},
		{"manifest", expected.ManifestDigest, manifestDigest},
		{"cold payload", expected.ColdPayloadDigest, manifest.ColdPayloadDigest},
		{"hot index", expected.HotIndexDigest, manifest.HotIndexDigest},
	} {
		if digest.want == "" {
			t.Fatalf("publication %s declares no expected %s digest, so the compiled bundle is unverifiable",
				name, digest.label)
		}
		if digest.want != digest.got {
			t.Fatalf("publication %s compiled to %s digest %s but its input expects %s",
				name, digest.label, digest.got, digest.want)
		}
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("publication %s compiled to an invalid dictionary manifest: %v", name, err)
	}
}

// prepareOrdinalForTest prepares one plan through the production preparation
// path and returns the ordinal execution the Gateway would execute it with.
//
// Fixtures that fabricate a provenance row need the compiled program's handle
// aliases and evidence-field aliases. Since T1d there is exactly one place those
// come from -- internal/physicalquery, reached through Service.preparePlan -- so
// the fixture is built against the program production actually compiled. A
// fixture that compiled its own would agree only until the two drifted, and the
// drift would show up as an unresolvable row handle rather than as a mismatch
// anyone could read.
func prepareOrdinalForTest(t *testing.T, harness *gatewayHarness, taskID string,
	plan queryplan.QueryPlan) *boundOrdinalExecution {
	t.Helper()
	grant, err := harness.store.GetGrant(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load grant for %s: %v", taskID, err)
	}
	prepared, err := harness.service.preparePlan(grant, plan)
	if err != nil {
		t.Fatalf("prepare %s: %v", taskID, err)
	}
	if prepared.Exposure == nil || prepared.Exposure.ordinal == nil {
		t.Fatalf("task %s prepared no ordinal execution", taskID)
	}
	return prepared.Exposure.ordinal
}
