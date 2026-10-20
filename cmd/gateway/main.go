package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/principalattr"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

const defaultGatewayConnectorMaxRows int64 = 1_200_000

const callbackPhaseTimingMarker = "DIAGNOSIS-NOT-FOR-PUBLICATION"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logicalCatalog, err := catalog.Load(requiredEnv("CATALOG_PATH"))
	if err != nil {
		logger.Error("catalog validation failed", "error", err)
		os.Exit(1)
	}
	// The attribute snapshot is optional unless the Catalog declares
	// eligibility on any Product; gatewayapp.New then refuses to start without
	// it, so a misconfigured deployment fails here rather than at request time.
	var principalAttributes *principalattr.Snapshot
	if path := strings.TrimSpace(os.Getenv("PRINCIPAL_ATTRIBUTES_PATH")); path != "" {
		principalAttributes, err = principalattr.Load(path)
		if err != nil {
			logger.Error("principal attribute snapshot validation failed", "error", err)
			os.Exit(1)
		}
		logger.Info("principal attribute snapshot loaded", "path", path, "sha256", principalAttributes.SHA256, "subjects", len(principalAttributes.Subjects))
	}
	key, err := control.ParseAES256Key(requiredEnv("GATEWAY_DATA_KEY"))
	if err != nil {
		logger.Error("invalid result encryption key", "error", err)
		os.Exit(1)
	}
	cipher, err := control.NewAES256GCMWithKeyID(env("GATEWAY_DATA_KEY_ID", control.DefaultResultEncryptionKeyID), key)
	if err != nil {
		logger.Error("initialize result cipher", "error", err)
		os.Exit(1)
	}
	pathStyle, err := boolEnv("GATEWAY_OBJECT_STORE_PATH_STYLE", true)
	if err != nil {
		logger.Error("invalid object-store addressing mode", "error", err)
		os.Exit(1)
	}
	objectBackend, err := resultartifact.NewS3Backend(resultartifact.S3Config{
		Endpoint:       requiredEnv("GATEWAY_OBJECT_STORE_ENDPOINT"),
		Region:         env("GATEWAY_OBJECT_STORE_REGION", "us-east-1"),
		Bucket:         requiredEnv("GATEWAY_OBJECT_STORE_BUCKET"),
		AccessKey:      requiredEnv("GATEWAY_OBJECT_STORE_ACCESS_KEY"),
		SecretKey:      requiredEnv("GATEWAY_OBJECT_STORE_SECRET_KEY"),
		ForcePathStyle: pathStyle,
	})
	if err != nil {
		logger.Error("initialize result object store", "error", err)
		os.Exit(1)
	}
	// The knowledge originals bucket (docs/kb-design.md §12.9) is optional and
	// read with its own read-only credentials.
	var kbOriginals gatewayapp.KBOriginalStore
	if bucket := strings.TrimSpace(os.Getenv("GATEWAY_KB_ORIGINALS_BUCKET")); bucket != "" {
		originals, err := resultartifact.NewS3Backend(resultartifact.S3Config{
			Endpoint:       requiredEnv("GATEWAY_OBJECT_STORE_ENDPOINT"),
			Region:         env("GATEWAY_OBJECT_STORE_REGION", "us-east-1"),
			Bucket:         bucket,
			AccessKey:      requiredEnv("GATEWAY_KB_ORIGINALS_ACCESS_KEY"),
			SecretKey:      requiredEnv("GATEWAY_KB_ORIGINALS_SECRET_KEY"),
			ForcePathStyle: pathStyle,
		})
		if err != nil {
			logger.Error("initialize knowledge originals store", "error", err)
			os.Exit(1)
		}
		kbOriginals = originals
		logger.Info("knowledge originals store", "bucket", bucket)
	}
	resultArtifacts, err := resultartifact.NewManager(objectBackend, cipher,
		env("GATEWAY_RESULT_TEMP_DIR", "/tmp/taskgate-result-artifacts"))
	if err != nil {
		logger.Error("initialize Parquet result artifacts", "error", err)
		os.Exit(1)
	}
	objectReadyCtx, objectReadyCancel := context.WithTimeout(ctx, 10*time.Second)
	objectReadyErr := resultArtifacts.Ready(objectReadyCtx)
	objectReadyCancel()
	if objectReadyErr != nil {
		logger.Error("result object store is unavailable", "error", objectReadyErr)
		os.Exit(1)
	}
	queryReceiptSigner, err := queryreceipt.NewSignerFromBase64(
		requiredEnv("GATEWAY_RECEIPT_KEY_ID"), requiredEnv("GATEWAY_RECEIPT_PRIVATE_KEY"),
	)
	if err != nil {
		logger.Error("initialize query receipt signer", "error", err)
		os.Exit(1)
	}
	queryReceiptKeyBundle, err := queryReceiptPublicKeyBundleFromEnv(queryReceiptSigner, time.Now().UTC())
	if err != nil {
		logger.Error("initialize query receipt public key bundle", "error", err)
		os.Exit(1)
	}
	controlMaxOpen, err := positiveInt64Env("GATEWAY_CONTROL_MAX_OPEN_CONNECTIONS", 10)
	if err != nil || controlMaxOpen > 4096 {
		logger.Error("invalid Control PostgreSQL open-connection limit", "error", "must be between 1 and 4096")
		os.Exit(1)
	}
	controlMaxIdle, err := positiveInt64Env("GATEWAY_CONTROL_MAX_IDLE_CONNECTIONS", 4)
	if err != nil || controlMaxIdle > controlMaxOpen {
		logger.Error("invalid Control PostgreSQL idle-connection limit", "error", "must be between 1 and the open-connection limit")
		os.Exit(1)
	}
	controlConnMaxLifetime, err := positiveDurationEnv("GATEWAY_CONTROL_CONNECTION_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		logger.Error("invalid Control PostgreSQL connection lifetime", "error", err)
		os.Exit(1)
	}
	storeOptions := []control.Option{
		control.WithPoolConfig(control.PoolConfig{
			MaxOpenConns: int(controlMaxOpen), MaxIdleConns: int(controlMaxIdle),
			ConnMaxLifetime: controlConnMaxLifetime,
		}),
		control.WithRecoveryReceiptBuilder(func(evidence control.QueryReceipt) (control.SaveQueryReceiptRequest, error) {
			return gatewayapp.BuildQueryReceiptRequest(evidence, queryReceiptSigner, time.Now().UTC())
		}),
	}
	callbackTimingOption, err := callbackPhaseTimingOption(os.Getenv("TASKGATE_CALLBACK_PHASE_TIMING"), os.Stdout)
	if err != nil {
		logger.Error("invalid callback phase timing marker", "error", err)
		os.Exit(1)
	}
	if callbackTimingOption != nil {
		storeOptions = append(storeOptions, callbackTimingOption)
	}
	store, err := control.Open(ctx, requiredEnv("CONTROL_POSTGRES_DSN"), cipher, storeOptions...)
	if err != nil {
		logger.Error("open control store", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	if !logicalCatalog.V4Enabled() {
		// A V4 marker is permanent. Checking it even for a legacy Catalog makes
		// replacing the Catalog or Gateway binary an unavailable downgrade, not
		// a way around the ordinal exposure boundary.
		if err := store.EnforceExposureDeploymentMode(ctx, logicalCatalog.SHA256, false); err != nil {
			logger.Error("legacy Catalog refused by exposure deployment mode", "error", err)
			os.Exit(1)
		}
	}
	snapshotRegistry, err := snapshotRegistryFromEnv(ctx, logicalCatalog, store)
	if err != nil {
		logger.Error("activate snapshot index publications", "error", err)
		os.Exit(1)
	}
	if snapshotRegistry == nil && len(logicalCatalog.SnapshotPublications) != 0 {
		// A Catalog that publishes V4 snapshots is not serviceable without its
		// verified HOT indexes. Do not advertise a healthy Gateway that will reject
		// every otherwise-authorized request only after creating task/query state.
		logger.Error("snapshot index artifact directory is required by the Catalog")
		os.Exit(1)
	}
	if kbManifests, err := activateKnowledgeProducts(logicalCatalog, strings.TrimSpace(os.Getenv("GATEWAY_KB_ARTIFACT_DIR"))); err != nil {
		logger.Error("knowledge product activation failed", "error", err)
		os.Exit(1)
	} else {
		for _, manifest := range kbManifests {
			logger.Info("knowledge product activated", "knowledge_product", manifest.KnowledgeProduct, "epoch", manifest.Epoch,
				"manifest_digest", manifest.ManifestDigest, "chunks", manifest.Counts.Chunks)
		}
	}

	kbEmbedder, kbEmbedderKind, err := kbEmbedderFor(logicalCatalog, strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")))
	if err != nil {
		logger.Error("knowledge product embedder configuration failed", "error", err)
		os.Exit(1)
	}
	if len(logicalCatalog.KnowledgeProducts) > 0 {
		logger.Info("knowledge product query embedder", "kind", kbEmbedderKind)
	}

	aliceToken := requiredEnv("TASKBOUND_ALICE_TOKEN")
	carolToken := requiredEnv("TASKBOUND_CAROL_TOKEN")
	alice := control.Principal{ID: "principal_alice", Subject: "alice", Role: "query", TokenHash: tokenDigest(aliceToken)}
	carol := control.Principal{ID: "principal_carol", Subject: "carol", Role: "auditor", TokenHash: tokenDigest(carolToken)}
	if err := bootstrapPrincipal(ctx, store, alice); err != nil {
		logger.Error("bootstrap Alice", "error", err)
		os.Exit(1)
	}
	if err := bootstrapPrincipal(ctx, store, carol); err != nil {
		logger.Error("bootstrap Carol", "error", err)
		os.Exit(1)
	}
	var filePrincipals []control.Principal
	if path := strings.TrimSpace(os.Getenv("PRINCIPALS_PATH")); path != "" {
		// Additional query principals, for example the subjects an eligibility
		// experiment needs; the file holds token digests only.
		extra, err := loadPrincipalsFile(path, []control.Principal{alice, carol})
		if err != nil {
			logger.Error("load principals file", "error", err)
			os.Exit(1)
		}
		for _, principal := range extra {
			if err := bootstrapPrincipal(ctx, store, principal); err != nil {
				logger.Error("bootstrap file principal", "principal", principal.ID, "error", err)
				os.Exit(1)
			}
		}
		filePrincipals = extra
		logger.Info("principals bootstrapped from file", "count", len(extra))
	}
	oaClient, err := approval.NewClient(requiredEnv("OA_BASE_URL"), requiredEnv("OA_SERVICE_TOKEN"), nil)
	if err != nil {
		logger.Error("initialize OA adapter", "error", err)
		os.Exit(1)
	}
	oaReceiptVerifier, err := approvalReceiptVerifierFromEnv()
	if err != nil {
		logger.Error("initialize OA approval receipt verifier", "error", err)
		os.Exit(1)
	}
	source, expectedSchema, err := expectedDatasource(logicalCatalog)
	if err != nil {
		logger.Error("build datasource attestation", "error", err)
		os.Exit(1)
	}
	businessDSN := sourceDSN(source, requiredSecret(source.SecretRef))
	connectorMaxRows, err := positiveInt64Env("GATEWAY_CONNECTOR_MAX_ROWS", defaultGatewayConnectorMaxRows)
	if err != nil {
		logger.Error("invalid connector row ceiling", "error", err)
		os.Exit(1)
	}
	connectorStatementTimeout, err := positiveDurationEnv("GATEWAY_CONNECTOR_STATEMENT_TIMEOUT", 5*time.Second)
	if err != nil {
		logger.Error("invalid connector statement timeout", "error", err)
		os.Exit(1)
	}
	settlementTimeout, err := positiveDurationEnv("GATEWAY_SETTLEMENT_TIMEOUT", 5*time.Second)
	if err != nil {
		logger.Error("invalid settlement timeout", "error", err)
		os.Exit(1)
	}
	artifactOperationTimeout, err := positiveDurationEnv("GATEWAY_RESULT_PROMOTION_TIMEOUT", 30*time.Minute)
	if err != nil {
		logger.Error("invalid result promotion timeout", "error", err)
		os.Exit(1)
	}
	stagingOrphanTTL, err := positiveDurationEnv("GATEWAY_RESULT_STAGING_ORPHAN_TTL", 24*time.Hour)
	if err != nil {
		logger.Error("invalid result staging orphan TTL", "error", err)
		os.Exit(1)
	}
	if stagingOrphanTTL < time.Hour {
		logger.Error("invalid result staging orphan TTL", "error", "must be at least 1h")
		os.Exit(1)
	}
	if purged, purgeErr := resultArtifacts.PurgeLocalStagingBefore(time.Now().UTC().Add(-stagingOrphanTTL)); purgeErr != nil {
		logger.Warn("local encrypted Parquet staging cleanup failed", "error", purgeErr)
	} else if purged > 0 {
		logger.Info("local encrypted Parquet staging cleanup completed", "purged", purged)
	}
	retention, err := retentionConfigFromEnv()
	if err != nil {
		logger.Error("initialize retention policy", "error", err)
		os.Exit(1)
	}
	deliveryTTL, err := positiveDurationEnv("GATEWAY_RESULT_DELIVERY_TTL", 5*time.Minute)
	if err != nil {
		logger.Error("invalid result delivery TTL", "error", err)
		os.Exit(1)
	}
	downloadTimeout, err := positiveDurationEnv("GATEWAY_RESULT_DOWNLOAD_TIMEOUT", 30*time.Minute)
	if err != nil {
		logger.Error("invalid result download timeout", "error", err)
		os.Exit(1)
	}
	downloadConcurrency64, err := positiveInt64Env("GATEWAY_RESULT_DOWNLOAD_CONCURRENCY", 4)
	if err != nil || downloadConcurrency64 > 1024 {
		logger.Error("invalid result download concurrency", "error", "must be between 1 and 1024")
		os.Exit(1)
	}
	previewMaxBytes, err := positiveInt64Env("GATEWAY_RESULT_PREVIEW_MAX_BYTES", 64<<20)
	if err != nil {
		logger.Error("invalid result preview byte limit", "error", err)
		os.Exit(1)
	}
	deliveryBaseURL, deliverySigningKey, err := deliveryConfigFromEnv()
	if err != nil {
		logger.Error("initialize result delivery", "error", err)
		os.Exit(1)
	}
	connectorMaxConnections, err := positiveInt64Env("GATEWAY_CONNECTOR_MAX_CONNECTIONS", int64(dataconnector.DefaultMaxConnections))
	if err != nil || connectorMaxConnections > 4096 {
		logger.Error("invalid Business PostgreSQL connector connection limit", "error", "must be between 1 and 4096")
		os.Exit(1)
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: businessDSN, StatementTimeout: connectorStatementTimeout,
		ConnectTimeout: 10 * time.Second, MaxRows: connectorMaxRows, MaxConnections: int32(connectorMaxConnections),
		ExpectedSchema: expectedSchema, ExpectedSchemaDigest: source.SchemaDigest,
		ExpectedAttestation: dataconnector.ExpectedAttestation{
			DatasourceID: source.DatasourceID, Database: source.Database, User: source.User,
			PostgreSQLMajorVersion: source.PostgreSQLMajorVersion,
		},
	})
	if err != nil {
		logger.Error("initialize PostgreSQL connector", "error", err)
		os.Exit(1)
	}
	defer connector.Close()
	service, err := gatewayapp.New(gatewayapp.Config{
		Catalog: logicalCatalog, Store: store, Approval: oaClient, PrincipalAttributes: principalAttributes, KBEmbedder: kbEmbedder,
		Connector: connector, CallbackSecret: requiredEnv("OA_CALLBACK_SECRET"), Logger: logger,
		ReceiptVerifier: oaReceiptVerifier, QueryReceiptSigner: queryReceiptSigner, Background: ctx,
		SettlementTimeout: settlementTimeout, SnapshotRegistry: snapshotRegistry,
		ResultArtifacts: resultArtifacts, ResultTTL: retention.ResultTTL, KBOriginals: kbOriginals,
		ArtifactOperationTimeout: artifactOperationTimeout,
		PreviewMaxBytes:          previewMaxBytes, DeliveryBaseURL: deliveryBaseURL,
		DeliverySigningKey: deliverySigningKey, DeliveryTTL: deliveryTTL,
		DownloadTimeout: downloadTimeout, DownloadConcurrency: int(downloadConcurrency64),
		OAPublicBaseURL: oaPublicBaseURLFromEnv(logger),
	})
	if err != nil {
		logger.Error("initialize gateway service", "error", err)
		os.Exit(1)
	}
	auditAnchor, err := auditAnchorConfigFromEnv()
	if err != nil {
		logger.Error("initialize audit anchor publisher", "error", err)
		os.Exit(1)
	}
	digestIdentities := make([]mcp.DigestIdentity, 0, len(filePrincipals))
	for _, principal := range filePrincipals {
		digestIdentities = append(digestIdentities, mcp.DigestIdentity{TokenSHA256: principal.TokenHash,
			Principal: mcp.Principal{ID: principal.ID, Subject: principal.Subject, Role: principal.Role}})
	}
	authenticator := mcp.NewStaticAuthenticatorWithDigests([]mcp.TokenIdentity{
		{Token: aliceToken, Principal: mcp.Principal{ID: "principal_alice", Subject: "alice", Role: "query"}},
		{Token: carolToken, Principal: mcp.Principal{ID: "principal_carol", Subject: "carol", Role: "auditor"}},
	}, digestIdentities)
	mcpServer, err := mcp.NewServer(authenticator, service, logger)
	if err != nil {
		logger.Error("initialize MCP server", "error", err)
		os.Exit(1)
	}

	var concurrencyProbe *gatewayapp.ConcurrencyProbe
	if concurrencyToken := strings.TrimSpace(os.Getenv("GATEWAY_EVALUATION_CONCURRENCY_TOKEN")); concurrencyToken != "" {
		httpActiveCapacity, capacityErr := positiveInt64Env("GATEWAY_EVALUATION_CONCURRENCY_HTTP_ACTIVE", 512)
		if capacityErr != nil || httpActiveCapacity > 4096 {
			logger.Error("invalid evaluation concurrency active capacity", "error", "must be between 1 and 4096")
			os.Exit(1)
		}
		httpQueueCapacity, capacityErr := positiveInt64Env("GATEWAY_EVALUATION_CONCURRENCY_HTTP_QUEUE", 512)
		if capacityErr != nil || httpQueueCapacity > 16384 {
			logger.Error("invalid evaluation concurrency queue capacity", "error", "must be between 1 and 16384")
			os.Exit(1)
		}
		concurrencyProbe, err = gatewayapp.NewConcurrencyProbe(gatewayapp.ConcurrencyProbeConfig{
			Token: concurrencyToken, MaxActive: int(httpActiveCapacity), MaxQueued: int(httpQueueCapacity),
			PoolStats: store.DBStats, ConnectorMaxConnections: int(connectorMaxConnections),
		})
		if err != nil {
			logger.Error("initialize authenticated evaluation concurrency probe", "error", err)
			os.Exit(1)
		}
	}

	router := chi.NewRouter()
	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	router.Get("/health/ready", readiness(store, connector, service))
	router.Get("/.well-known/taskgate/query-receipt-keyring.json", queryReceiptKeyringHandler(queryReceiptKeyBundle))
	if concurrencyProbe != nil {
		router.Mount(gatewayapp.ConcurrencyProbeAdminPath, concurrencyProbe.AdminHandler())
		router.With(func(next http.Handler) http.Handler { return concurrencyProbe.Middleware(next) }).Handle("/mcp", mcpServer)
	} else {
		router.Handle("/mcp", mcpServer)
	}
	router.Handle("/api/v1/oa/callback", service.OACallbackHandler())
	router.Handle("/api/v1/results/{result_id}/download", service.ResultDownloadHandler())
	router.Handle("/api/v1/kb/documents/download", service.DocumentDownloadHandler())
	mountRetentionAdmin(router, store, retention, logger, service)
	mountProfileDiagnostic(router, activatedSnapshotState, retention.AdminToken, service.ReadyError, store.DB())

	go sweepExpired(ctx, store, logger)
	go sweepPendingResultArtifacts(ctx, service, logger)
	go sweepOrphanResultStaging(ctx, service, stagingOrphanTTL, logger)
	go sweepRetention(ctx, store, retention, logger, service)
	if auditAnchor.enabled() {
		go sweepAuditAnchors(ctx, store, auditAnchor, logger)
	}

	httpRequestTimeout, err := positiveDurationEnv("GATEWAY_HTTP_REQUEST_TIMEOUT", 130*time.Second)
	if err != nil {
		logger.Error("invalid HTTP request timeout", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr: env("GATEWAY_ADDR", ":8082"), Handler: router, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: httpRequestTimeout, WriteTimeout: httpRequestTimeout, IdleTimeout: 60 * time.Second,
	}
	go func() {
		logger.Info("gateway listening", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("gateway stopped", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	// Diagnostic-only (no-op unless the diagnosis marker enabled callback phase
	// timing): record every submitted callback still in flight before the HTTP
	// drain, which could otherwise outlast the container stop grace period.
	store.SnapshotInflightCallbackPhases("shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}

func callbackPhaseTimingOption(marker string, writer io.Writer) (control.Option, error) {
	if marker == "" {
		return nil, nil
	}
	if marker != callbackPhaseTimingMarker || writer == nil {
		return nil, errors.New("callback phase timing requires the exact non-publication diagnosis marker and an output writer")
	}
	return control.WithCallbackPhaseTiming(writer), nil
}

func sweepPendingResultArtifacts(ctx context.Context, service *gatewayapp.Service, logger *slog.Logger) {
	reconcile := func() {
		count, err := service.ReconcilePendingArtifacts(ctx)
		if err != nil {
			logger.Warn("pending Parquet result recovery failed", "recovered", count, "error", err)
		} else if count > 0 {
			logger.Info("pending Parquet results recovered", "count", count)
		}
	}
	reconcile()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func sweepOrphanResultStaging(ctx context.Context, service *gatewayapp.Service, ttl time.Duration, logger *slog.Logger) {
	cleanup := func() {
		count, err := service.PurgeOrphanStagingBefore(ctx, time.Now().UTC().Add(-ttl))
		if err != nil {
			logger.Warn("orphan Parquet staging cleanup failed", "purged", count, "error", err)
		} else if count > 0 {
			logger.Info("orphan Parquet staging cleanup completed", "purged", count)
		}
	}
	cleanup()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

// expectedDatasource delegates to the one shared builder so the Gateway and the
// evaluation cannot derive different ExpectedSchema entry counts. The
// attestation multiplicity in observer-accounting-v3 is a function of that
// count, so a second implementation here would be a second answer.
func expectedDatasource(logicalCatalog *catalog.Catalog) (catalog.Source, []dataconnector.ViewSchema, error) {
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return catalog.Source{}, nil, err
	}
	return built.Source, built.Entries, nil
}

func sourceDSN(source catalog.Source, password string) string {
	value := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(source.User, password),
		Host:   net.JoinHostPort(source.Address, strconv.Itoa(source.Port)),
		Path:   source.Database,
	}
	query := value.Query()
	query.Set("sslmode", "disable")
	value.RawQuery = query.Encode()
	return value.String()
}

type receiptKeyringEntry struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	ValidFrom string `json:"valid_from,omitempty"`
	RetiredAt string `json:"retired_at,omitempty"`
}

func approvalReceiptVerifierFromEnv() (approval.ReceiptVerifier, error) {
	if raw := strings.TrimSpace(os.Getenv("OA_RECEIPT_KEYRING_JSON")); raw != "" {
		var entries []receiptKeyringEntry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, err
		}
		keys := make([]approval.ReceiptVerifyingKey, 0, len(entries))
		for _, entry := range entries {
			publicKey, err := decodeEd25519PublicKey(entry.PublicKey)
			if err != nil {
				return nil, err
			}
			validFrom, err := parseOptionalRFC3339(entry.ValidFrom)
			if err != nil {
				return nil, err
			}
			retiredAt, err := parseOptionalRFC3339(entry.RetiredAt)
			if err != nil {
				return nil, err
			}
			keys = append(keys, approval.ReceiptVerifyingKey{
				KeyID: entry.KeyID, PublicKey: publicKey, ValidFrom: validFrom, RetiredAt: retiredAt,
			})
		}
		return approval.NewEd25519ReceiptVerifierWithKeyring(keys)
	}
	return approval.NewReceiptVerifierFromBase64(requiredEnv("OA_RECEIPT_KEY_ID"), requiredEnv("OA_RECEIPT_PUBLIC_KEY"))
}

func queryReceiptPublicKeyBundleFromEnv(signer *queryreceipt.Signer, publishedAt time.Time) (queryreceipt.PublicKeyBundleV1, error) {
	if signer == nil || signer.KeyID() == "" || signer.PublicKey() == nil {
		return queryreceipt.PublicKeyBundleV1{}, queryreceipt.ErrInvalidKey
	}
	keys, err := queryReceiptVerifyingKeysFromEnv(strings.TrimSpace(os.Getenv("GATEWAY_RECEIPT_KEYRING_JSON")))
	if err != nil {
		return queryreceipt.PublicKeyBundleV1{}, err
	}
	activePublicKey := signer.PublicKey()
	activeFound := false
	for _, key := range keys {
		if key.KeyID != signer.KeyID() {
			continue
		}
		activeFound = true
		if !bytes.Equal(key.PublicKey, activePublicKey) {
			return queryreceipt.PublicKeyBundleV1{}, queryreceipt.ErrInvalidKey
		}
		if !key.RetiredAt.IsZero() && publishedAt.UTC().After(key.RetiredAt.UTC()) {
			return queryreceipt.PublicKeyBundleV1{}, queryreceipt.ErrKeyNotValid
		}
	}
	if !activeFound {
		keys = append(keys, queryreceipt.VerifyingKey{KeyID: signer.KeyID(), PublicKey: activePublicKey})
	}
	return queryreceipt.NewPublicKeyBundle(signer.KeyID(), keys, publishedAt)
}

func queryReceiptVerifyingKeysFromEnv(raw string) ([]queryreceipt.VerifyingKey, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var entries []receiptKeyringEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	keys := make([]queryreceipt.VerifyingKey, 0, len(entries))
	for _, entry := range entries {
		publicKey, err := decodeEd25519PublicKey(entry.PublicKey)
		if err != nil {
			return nil, queryreceipt.ErrInvalidKey
		}
		validFrom, err := parseOptionalRFC3339(entry.ValidFrom)
		if err != nil {
			return nil, err
		}
		retiredAt, err := parseOptionalRFC3339(entry.RetiredAt)
		if err != nil {
			return nil, err
		}
		keys = append(keys, queryreceipt.VerifyingKey{
			KeyID: entry.KeyID, PublicKey: publicKey, ValidFrom: validFrom, RetiredAt: retiredAt,
		})
	}
	return keys, nil
}

func queryReceiptKeyringHandler(bundle queryreceipt.PublicKeyBundleV1) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=300")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(bundle)
	}
}

func decodeEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(encoded)
	}
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, approval.ErrInvalidReceiptKey
	}
	return ed25519.PublicKey(key), nil
}

func parseOptionalRFC3339(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func requiredSecret(secretRef string) string {
	name := strings.TrimPrefix(secretRef, "env:")
	return requiredEnv(name)
}

func requiredEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		slog.Error("required environment variable is missing", "name", key)
		os.Exit(1)
	}
	return value
}

func tokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func bootstrapPrincipal(ctx context.Context, store *control.Store, wanted control.Principal) error {
	existing, err := store.GetPrincipal(ctx, wanted.ID)
	if errors.Is(err, control.ErrNotFound) {
		wanted.CreatedAt = time.Now().UTC()
		return store.CreatePrincipal(ctx, wanted)
	}
	if err != nil {
		return err
	}
	if existing.Subject != wanted.Subject || existing.Role != wanted.Role || existing.TokenHash != wanted.TokenHash || existing.DisabledAt != nil {
		return errors.New("persisted principal does not match configured identity; reset the demo volume or restore the token")
	}
	return nil
}

func readiness(store *control.Store, connector *dataconnector.Connector, service *gatewayapp.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.DB().PingContext(ctx); err != nil {
			http.Error(w, "control store unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := connector.Ping(ctx); err != nil {
			http.Error(w, "data source unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := service.ReadyError(); err != nil {
			http.Error(w, "query settlement pending", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type retentionConfig struct {
	ResultTTL     time.Duration
	SweepInterval time.Duration
	AdminToken    string
}

func retentionConfigFromEnv() (retentionConfig, error) {
	resultTTL, err := optionalDurationEnv("GATEWAY_RESULT_RETENTION_TTL")
	if err != nil {
		return retentionConfig{}, err
	}
	interval, err := optionalDurationEnv("GATEWAY_RESULT_RETENTION_SWEEP_INTERVAL")
	if err != nil {
		return retentionConfig{}, err
	}
	if interval == 0 {
		interval = time.Hour
	}
	if resultTTL < 0 || interval < 0 {
		return retentionConfig{}, errors.New("retention durations must be non-negative")
	}
	return retentionConfig{
		ResultTTL: resultTTL, SweepInterval: interval, AdminToken: strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_TOKEN")),
	}, nil
}

func optionalDurationEnv(key string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, nil
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		return parsed, nil
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(seconds) * time.Second, nil
}

func mountRetentionAdmin(router chi.Router, store *control.Store, config retentionConfig, logger *slog.Logger,
	services ...*gatewayapp.Service) {
	if strings.TrimSpace(config.AdminToken) == "" {
		return
	}
	router.Group(func(r chi.Router) {
		r.Use(adminTokenAuth(config.AdminToken))
		r.Post("/admin/v1/retention/purge", purgeRetentionHandler(store, config, logger, services...))
		r.Put("/admin/v1/retention/legal-holds/{task_id}", setRetentionHoldHandler(store))
		r.Delete("/admin/v1/retention/legal-holds/{task_id}", clearRetentionHoldHandler(store))
		r.Post("/admin/v1/result-encryption-keys/{key_id}/erase", eraseResultEncryptionKeyHandler(store))
	})
}

func adminTokenAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
			if !ok {
				http.Error(w, "admin token required", http.StatusUnauthorized)
				return
			}
			value = strings.TrimSpace(value)
			got := sha256.Sum256([]byte(value))
			want := sha256.Sum256([]byte(token))
			if strings.TrimSpace(token) == "" || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				http.Error(w, "admin token required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func purgeRetentionHandler(store *control.Store, config retentionConfig, logger *slog.Logger,
	services ...*gatewayapp.Service) http.HandlerFunc {
	type requestBody struct {
		Cutoff string `json:"cutoff"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		if r.Body != nil {
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "invalid purge request", http.StatusBadRequest)
				return
			}
		}
		cutoff, err := retentionCutoff(body.Cutoff, config.ResultTTL, time.Now().UTC())
		if err != nil {
			http.Error(w, "cutoff or GATEWAY_RESULT_RETENTION_TTL is required", http.StatusBadRequest)
			return
		}
		purged, err := store.PurgeEncryptedResultsBefore(r.Context(), cutoff)
		if err != nil {
			logger.Warn("manual retention purge failed", "error", err)
			http.Error(w, "retention purge failed", http.StatusConflict)
			return
		}
		var objectPurged int64
		if len(services) > 0 && services[0] != nil {
			objectPurged, err = services[0].PurgeResultArtifactsBefore(r.Context(), cutoff)
			if err != nil {
				logger.Warn("manual object retention purge failed", "error", err)
				http.Error(w, "retention purge failed", http.StatusConflict)
				return
			}
		}
		writeMainJSON(w, http.StatusOK, map[string]any{"cutoff": jsonTime(cutoff),
			"purged_results": purged + objectPurged, "purged_object_results": objectPurged})
	}
}

func retentionCutoff(raw string, ttl time.Duration, now time.Time) (time.Time, error) {
	if strings.TrimSpace(raw) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
		if err != nil {
			return time.Time{}, err
		}
		return parsed.UTC(), nil
	}
	if ttl <= 0 {
		return time.Time{}, errors.New("retention TTL disabled")
	}
	return now.Add(-ttl).UTC(), nil
}

func setRetentionHoldHandler(store *control.Store) http.HandlerFunc {
	type requestBody struct {
		Reason string `json:"reason"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body requestBody
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			http.Error(w, "invalid legal hold request", http.StatusBadRequest)
			return
		}
		hold, err := store.SetResultRetentionHold(r.Context(), chi.URLParam(r, "task_id"), body.Reason, "admin")
		if err != nil {
			retentionHTTPError(w, err)
			return
		}
		writeMainJSON(w, http.StatusOK, retentionHoldJSON(hold))
	}
}

func clearRetentionHoldHandler(store *control.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hold, err := store.ClearResultRetentionHold(r.Context(), chi.URLParam(r, "task_id"), "admin")
		if err != nil {
			retentionHTTPError(w, err)
			return
		}
		writeMainJSON(w, http.StatusOK, retentionHoldJSON(hold))
	}
}

func eraseResultEncryptionKeyHandler(store *control.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := store.EraseResultEncryptionKey(r.Context(), chi.URLParam(r, "key_id"), "admin")
		if err != nil {
			retentionHTTPError(w, err)
			return
		}
		writeMainJSON(w, http.StatusOK, resultEncryptionKeyJSON(key))
	}
}

func retentionHTTPError(w http.ResponseWriter, err error) {
	switch control.CodeOf(err) {
	case control.CodeNotFound:
		http.Error(w, "admin resource not found", http.StatusNotFound)
	case control.CodeInvalid:
		http.Error(w, "invalid admin request", http.StatusBadRequest)
	default:
		http.Error(w, "admin request failed", http.StatusConflict)
	}
}

func resultEncryptionKeyJSON(key control.ResultEncryptionKey) map[string]any {
	return map[string]any{
		"key_id":     key.KeyID,
		"status":     string(key.Status),
		"created_at": jsonTime(key.CreatedAt),
		"erased_at":  nullableJSONTime(key.ErasedAt),
		"erased_by":  key.ErasedBy,
	}
}

func retentionHoldJSON(hold control.ResultRetentionHold) map[string]any {
	return map[string]any{
		"task_id":     hold.TaskID,
		"reason":      hold.Reason,
		"created_by":  hold.CreatedBy,
		"created_at":  jsonTime(hold.CreatedAt),
		"released_by": hold.ReleasedBy,
		"released_at": nullableJSONTime(hold.ReleasedAt),
	}
}

func writeMainJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func nullableJSONTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return jsonTime(*value)
}

func jsonTime(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func sweepRetention(ctx context.Context, store *control.Store, config retentionConfig, logger *slog.Logger,
	services ...*gatewayapp.Service) {
	ticker := time.NewTicker(config.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			var purged int64
			if config.ResultTTL > 0 {
				cutoff := now.Add(-config.ResultTTL)
				var err error
				purged, err = store.PurgeEncryptedResultsBefore(ctx, cutoff)
				if err != nil {
					logger.Warn("retention purge sweep failed", "error", err)
					continue
				}
			}
			if len(services) > 0 && services[0] != nil {
				objectPurged, objectErr := services[0].PurgeExpiredResultArtifacts(ctx, now)
				if objectErr != nil {
					logger.Warn("object retention purge sweep failed", "error", objectErr)
					continue
				}
				purged += objectPurged
			}
			if purged > 0 {
				logger.Info("retention purge sweep completed", "purged_results", purged, "swept_at", now)
			}
		}
	}
}

type auditAnchorConfig struct {
	URL      string
	Interval time.Duration
	Signer   *auditchain.AnchorSigner
	Client   *http.Client
}

func (config auditAnchorConfig) enabled() bool {
	return strings.TrimSpace(config.URL) != ""
}

func auditAnchorConfigFromEnv() (auditAnchorConfig, error) {
	anchorURL := strings.TrimSpace(os.Getenv("GATEWAY_AUDIT_ANCHOR_URL"))
	if anchorURL == "" {
		return auditAnchorConfig{}, nil
	}
	parsed, err := url.Parse(anchorURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return auditAnchorConfig{}, errors.New("GATEWAY_AUDIT_ANCHOR_URL must be an absolute http(s) URL")
	}
	interval, err := optionalDurationEnv("GATEWAY_AUDIT_ANCHOR_INTERVAL")
	if err != nil {
		return auditAnchorConfig{}, err
	}
	if interval == 0 {
		interval = 5 * time.Minute
	}
	if interval < 0 {
		return auditAnchorConfig{}, errors.New("audit anchor interval must be non-negative")
	}
	keyID := strings.TrimSpace(os.Getenv("GATEWAY_AUDIT_ANCHOR_KEY_ID"))
	privateKey := strings.TrimSpace(os.Getenv("GATEWAY_AUDIT_ANCHOR_PRIVATE_KEY"))
	if keyID == "" || privateKey == "" {
		return auditAnchorConfig{}, errors.New("GATEWAY_AUDIT_ANCHOR_KEY_ID and GATEWAY_AUDIT_ANCHOR_PRIVATE_KEY are required when GATEWAY_AUDIT_ANCHOR_URL is set")
	}
	signer, err := auditchain.NewAnchorSignerFromBase64(keyID, privateKey)
	if err != nil {
		return auditAnchorConfig{}, err
	}
	return auditAnchorConfig{
		URL: anchorURL, Interval: interval, Signer: signer, Client: &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func sweepAuditAnchors(ctx context.Context, store *control.Store, config auditAnchorConfig, logger *slog.Logger) {
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	var last auditchain.Checkpoint
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			anchor, err := publishAuditAnchor(ctx, store, config, time.Now().UTC())
			if err != nil {
				logger.Warn("audit checkpoint anchor failed", "error", err)
				continue
			}
			if anchor.Sequence != last.Sequence || anchor.Hash != last.Hash {
				logger.Info("audit checkpoint anchored", "sequence", anchor.Sequence, "hash", anchor.Hash)
				last = auditchain.Checkpoint{Sequence: anchor.Sequence, Hash: anchor.Hash}
			}
		}
	}
}

func publishAuditAnchor(ctx context.Context, store *control.Store, config auditAnchorConfig, signedAt time.Time) (auditchain.SignedCheckpointAnchorV1, error) {
	checkpoint, err := store.AuditCheckpoint(ctx)
	if err != nil {
		return auditchain.SignedCheckpointAnchorV1{}, err
	}
	return postAuditCheckpointAnchor(ctx, config, checkpoint, signedAt)
}

func postAuditCheckpointAnchor(ctx context.Context, config auditAnchorConfig, checkpoint auditchain.Checkpoint, signedAt time.Time) (auditchain.SignedCheckpointAnchorV1, error) {
	if !config.enabled() || config.Signer == nil {
		return auditchain.SignedCheckpointAnchorV1{}, errors.New("audit anchor publisher is not configured")
	}
	anchor, err := config.Signer.SignCheckpoint(checkpoint, signedAt)
	if err != nil {
		return auditchain.SignedCheckpointAnchorV1{}, err
	}
	body, err := json.Marshal(anchor)
	if err != nil {
		return auditchain.SignedCheckpointAnchorV1{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.URL, bytes.NewReader(body))
	if err != nil {
		return auditchain.SignedCheckpointAnchorV1{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", anchor.AnchorID)
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return auditchain.SignedCheckpointAnchorV1{}, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return auditchain.SignedCheckpointAnchorV1{}, fmt.Errorf("audit anchor sink returned %s", response.Status)
	}
	return anchor, nil
}

func sweepExpired(ctx context.Context, store *control.Store, logger *slog.Logger) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			afterID := ""
			for {
				tasks, err := store.ListTasks(ctx, control.TaskFilter{State: control.TaskActive, AfterID: afterID, Limit: 500})
				if err != nil {
					logger.Warn("list tasks for expiry sweep", "after_id", afterID, "error", err)
					break
				}
				for _, task := range tasks {
					if task.ExpiresAt == nil || now.Before(*task.ExpiresAt) {
						continue
					}
					_, err := store.TransitionTask(ctx, control.TaskTransition{
						TaskID: task.ID, ExpectedFrom: control.TaskActive, To: control.TaskArchived,
						Reason: control.TerminalExpired, Actor: "system",
					})
					if err != nil && !errors.Is(err, control.ErrInvalidStateChange) {
						logger.Warn("archive expired task", "task_id", task.ID, "error", err)
					}
				}
				if len(tasks) < 500 {
					break
				}
				afterID = tasks[len(tasks)-1].ID
			}
		}
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

// oaPublicBaseURLFromEnv reads the OA's public address, the base of the draft
// links get_task_status rebuilds for pending tasks. It is optional, and an
// unusable value only leaves those links out.
func oaPublicBaseURLFromEnv(logger *slog.Logger) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_OA_BASE_URL")), "/")
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		logger.Warn("PUBLIC_OA_BASE_URL is not an absolute http(s) URL; pending tasks will not carry their OA draft link", "value", base)
		return ""
	}
	return base
}

func deliveryConfigFromEnv() (string, []byte, error) {
	base := strings.TrimRight(env("GATEWAY_PUBLIC_BASE_URL", "http://127.0.0.1:8082"), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, errors.New("GATEWAY_PUBLIC_BASE_URL must be an absolute http(s) URL without query or fragment")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", nil, errors.New("GATEWAY_PUBLIC_BASE_URL requires https except for loopback development addresses")
	}
	key := strings.TrimSpace(os.Getenv("GATEWAY_DELIVERY_SIGNING_KEY"))
	if key == "" {
		// The demo keeps backward-compatible configuration. Production should
		// supply an independent random capability-signing secret.
		key = requiredEnv("OA_CALLBACK_SECRET")
	}
	if len(key) < 32 {
		return "", nil, errors.New("GATEWAY_DELIVERY_SIGNING_KEY must contain at least 32 bytes")
	}
	return base, []byte(key), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func positiveInt64Env(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func positiveDurationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value, err := optionalDurationEnv(key)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return value, nil
}
