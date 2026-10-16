package gateway

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/kbqueryset"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// E-KB4 retrieval quality (docs/kb_experiment_design.md, pilot class).
//
// For every frozen query the gateway's search (text query embedded by the
// trusted service) and direct exact retrieval on kb.chunks with the same
// vector and the same mandatory scope are compared member by member, and the
// rank of the target document is recorded for recall@k and MRR. The whole
// run is repeated on KB_EKB4_DEPLOYMENTS independent control stores and the
// gateway hit sets are compared across them. The report is JSON; it is pilot
// evidence (campaign_class kb-pilot) until the author decides otherwise.
//
//	KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT (all required)
//	KB_EKB4_QUERYSET  directory with queries.jsonl and manifest.json (default evaluation/kb-pilot/queryset-v2)
//	KB_EKB4_REPORT    output JSON path (required)
//	KB_EKB4_DEPLOYMENTS repetitions on fresh control stores (default 3)
//	KB_EKB4_LIMIT     use only the first N queries (smoke runs)
func TestEKB4RetrievalQuality(t *testing.T) {
	businessDSN, artifactDir, epoch := os.Getenv("KB_E2E_BUSINESS_DSN"), os.Getenv("KB_E2E_ARTIFACT_DIR"), os.Getenv("KB_E2E_EPOCH")
	endpoint, reportPath := strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")), os.Getenv("KB_EKB4_REPORT")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || endpoint == "" || reportPath == "" {
		t.Skip("KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT and KB_EKB4_REPORT are required")
	}
	querysetDir := os.Getenv("KB_EKB4_QUERYSET")
	if querysetDir == "" {
		querysetDir = filepath.Join("..", "..", "evaluation", "kb-pilot", "queryset-v2")
	}
	deployments := 3
	if v := os.Getenv("KB_EKB4_DEPLOYMENTS"); v != "" {
		deployments, _ = strconv.Atoi(v)
	}
	limit := 0
	if v := os.Getenv("KB_EKB4_LIMIT"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()
	started := time.Now()

	// Frozen inputs.
	queries, querysetManifest := loadQueryset(t, querysetDir)
	if limit > 0 && limit < len(queries) {
		queries = queries[:limit]
	}
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	demo, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := catalog.Parse([]byte(kbOnlyCatalog(t, string(demo), string(fragment), "", "")))
	if err != nil {
		t.Fatal(err)
	}
	built, err := catalogschema.Build(probe)
	if err != nil {
		t.Fatal(err)
	}
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
	if err != nil {
		t.Fatal(err)
	}
	attestation, err := probeConnector.Attestation(ctx)
	probeConnector.Close()
	if err != nil {
		t.Fatal(err)
	}
	parsedDSN, err := url.Parse(businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	database := strings.Trim(parsedDSN.Path, "/")
	product := probe.KnowledgeProducts[0]

	// Query vectors, once: the gateway embeds the normalized text through the
	// same service, so the direct arm must rank with the same vector.
	embedder, err := kbpublish.NewHTTPEmbedder(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := embedder.Check(kbpublish.Embedding{Model: product.Embedding.Model, Dimension: product.Embedding.Dimension, WeightsSHA256: product.Embedding.WeightsSHA256}); err != nil {
		t.Fatalf("embedding service does not match the knowledge product: %v", err)
	}
	vectors := map[string]string{}
	for start := 0; start < len(queries); start += 32 {
		end := start + 32
		if end > len(queries) {
			end = len(queries)
		}
		texts := make([]string, 0, end-start)
		for _, query := range queries[start:end] {
			texts = append(texts, retrieval.NormalizeText(query.Text))
		}
		embedded, err := embedder.Embed(texts)
		if err != nil || len(embedded) != len(texts) {
			t.Fatalf("embed queries %d..%d: %v", start, end, err)
		}
		for i, query := range queries[start:end] {
			vectors[query.QueryID] = vectorLiteralOf(embedded[i])
		}
	}
	db, err := pgx.Connect(ctx, businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)

	ks := []int{5, 10, 20}
	type observation struct {
		QueryID     string   `json:"query_id"`
		Kind        string   `json:"kind"`
		Type        string   `json:"document_type"`
		Department  string   `json:"department"`
		K           int      `json:"k"`
		Gateway     []string `json:"gateway_hits"`
		Direct      []string `json:"direct_hits"`
		Identical   bool     `json:"identical"`
		RankDoc     int      `json:"rank_doc"`     // 1-based rank of the first chunk of the target doc_id (any version), 0 = not in top k
		RankVersion int      `json:"rank_version"` // same for the exact target version
		Error       string   `json:"error,omitempty"`
	}
	type deploymentReport struct {
		Deployment   int           `json:"deployment"`
		ControlStore string        `json:"control_store_schema"`
		Tasks        int           `json:"tasks"`
		Observations []observation `json:"observations"`
		Seconds      float64       `json:"seconds"`
	}
	var reports []deploymentReport
	allScope := []any{"public", "internal", "confidential", "leadership"}
	for d := 1; d <= deployments; d++ {
		deploymentStarted := time.Now()
		harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
			return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
		}})
		harness.taskBudget = &domain.Budget{MaxQueries: 100000, MaxRows: 10000000, MaxDBTime: 24 * time.Hour, PerQueryTimeout: 20 * time.Second, TaskTTL: 12 * time.Hour}
		built, err := catalogschema.Build(harness.catalog)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 20 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 1000, MaxConnections: 4,
			ExpectedSchema: built.Entries, ExpectedSchemaDigest: built.Source.SchemaDigest,
			ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: built.Source.DatasourceID, Database: built.Source.Database, User: built.Source.User, PostgreSQLMajorVersion: built.Source.PostgreSQLMajorVersion}})
		if err != nil {
			t.Fatal(err)
		}
		att, err := connector.Attestation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		harness.connector.attestation = att
		harness.service.connector = connector
		installKnowledgeRegistry(t, harness, artifactDir)
		harness.service.kbEmbedder = embedder
		chunks, documents := product.Products.Chunks, product.Products.Documents
		columns := map[string][]string{
			chunks:    {"doc_id", "version", "chunk_index", "text", "kb_department", "classification", "job_role", "valid_until"},
			documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
		}
		tasks := map[string]string{}
		report := deploymentReport{Deployment: d, ControlStore: schemaOf(t, harness)}
		for i, query := range queries {
			taskID, ok := tasks[query.Department]
			if !ok {
				taskID = fmt.Sprintf("ekb4-d%d-%s", d, query.Department)
				harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
					control.ExposureLimits{ReleaseFacts: 10000000, InfluenceFacts: 100000000, OutcomeFacts: 1000000}, exposure.ProfileV5,
					[]string{chunks, documents}, columns, domain.SensitivityHigh,
					map[string]any{"kb_department": []any{query.Department}, "classification": allScope})
				tasks[query.Department] = taskID
			}
			for _, k := range ks {
				obs := observation{QueryID: query.QueryID, Kind: query.Kind, Type: query.DocumentType, Department: query.Department, K: k}
				result, err := callGatewayTool(harness.service, harness.alice, "search", map[string]any{
					"task_id": taskID, "request_id": fmt.Sprintf("%s-k%d", query.QueryID, k), "knowledge_product": product.Name,
					"query": map[string]any{"text": query.Text}, "k": k})
				if err != nil {
					obs.Error = err.Error()
					report.Observations = append(report.Observations, obs)
					continue
				}
				for _, hit := range searchHits(t, result) {
					obs.Gateway = append(obs.Gateway, hitID(hit))
				}
				for _, hit := range exactHitsScoped(t, ctx, db, product.Collection, vectors[query.QueryID], query.Department, k) {
					obs.Direct = append(obs.Direct, fmt.Sprintf("%s#%d#%d", hit.doc, hit.version, hit.chunk))
				}
				obs.Identical = strings.Join(obs.Gateway, ",") == strings.Join(obs.Direct, ",")
				for rank, id := range obs.Gateway {
					parts := strings.Split(id, "#")
					if parts[0] == query.DocID && obs.RankDoc == 0 {
						obs.RankDoc = rank + 1
					}
					if parts[0] == query.DocID && parts[1] == strconv.Itoa(query.Version) && obs.RankVersion == 0 {
						obs.RankVersion = rank + 1
					}
				}
				report.Observations = append(report.Observations, obs)
			}
			if (i+1)%50 == 0 {
				t.Logf("deployment %d: %d/%d queries, %.0fs", d, i+1, len(queries), time.Since(deploymentStarted).Seconds())
			}
		}
		report.Tasks = len(tasks)
		report.Seconds = time.Since(deploymentStarted).Seconds()
		reports = append(reports, report)
		connector.Close()
	}

	// Summary.
	type bucket struct{ hits, total int }
	summary := map[string]any{}
	identical := map[int]*bucket{}
	recallKind := map[string]*bucket{}
	recallType := map[string]*bucket{}
	recallVersion := map[int]*bucket{}
	var mrr = map[string]float64{}
	mrrCount := map[string]int{}
	errors := 0
	for _, report := range reports {
		for _, obs := range report.Observations {
			if obs.Error != "" {
				errors++
				continue
			}
			if identical[obs.K] == nil {
				identical[obs.K] = &bucket{}
			}
			identical[obs.K].total++
			if obs.Identical {
				identical[obs.K].hits++
			}
			keyKind := fmt.Sprintf("%s@%d", obs.Kind, obs.K)
			if recallKind[keyKind] == nil {
				recallKind[keyKind] = &bucket{}
			}
			recallKind[keyKind].total++
			if obs.RankDoc > 0 {
				recallKind[keyKind].hits++
			}
			keyType := fmt.Sprintf("%s@%d", obs.Type, obs.K)
			if recallType[keyType] == nil {
				recallType[keyType] = &bucket{}
			}
			recallType[keyType].total++
			if obs.RankDoc > 0 {
				recallType[keyType].hits++
			}
			if recallVersion[obs.K] == nil {
				recallVersion[obs.K] = &bucket{}
			}
			recallVersion[obs.K].total++
			if obs.RankVersion > 0 {
				recallVersion[obs.K].hits++
			}
			if obs.K == 20 {
				mrrCount[obs.Kind]++
				if obs.RankDoc > 0 {
					mrr[obs.Kind] += 1 / float64(obs.RankDoc)
				}
			}
		}
	}
	rate := func(b *bucket) float64 {
		if b == nil || b.total == 0 {
			return 0
		}
		return float64(b.hits) / float64(b.total)
	}
	identityByK := map[string]float64{}
	for k, b := range identical {
		identityByK[strconv.Itoa(k)] = rate(b)
	}
	recallByKind := map[string]float64{}
	for key, b := range recallKind {
		recallByKind[key] = rate(b)
	}
	recallByType := map[string]float64{}
	for key, b := range recallType {
		recallByType[key] = rate(b)
	}
	recallExactVersion := map[string]float64{}
	for k, b := range recallVersion {
		recallExactVersion[strconv.Itoa(k)] = rate(b)
	}
	mrrByKind := map[string]float64{}
	for kind, sum := range mrr {
		mrrByKind[kind] = sum / float64(mrrCount[kind])
	}
	// Cross-deployment identity of the gateway hit sets.
	crossIdentical, crossTotal := 0, 0
	if len(reports) > 1 {
		first := map[string]string{}
		for _, obs := range reports[0].Observations {
			first[obs.QueryID+"@"+strconv.Itoa(obs.K)] = strings.Join(obs.Gateway, ",")
		}
		for _, report := range reports[1:] {
			for _, obs := range report.Observations {
				crossTotal++
				if first[obs.QueryID+"@"+strconv.Itoa(obs.K)] == strings.Join(obs.Gateway, ",") {
					crossIdentical++
				}
			}
		}
	}
	summary["queries"] = len(queries)
	summary["deployments"] = deployments
	summary["errors"] = errors
	summary["gateway_vs_direct_identity_by_k"] = identityByK
	summary["recall_at_k_by_kind"] = recallByKind
	summary["recall_at_k_by_document_type"] = recallByType
	summary["recall_at_k_exact_version"] = recallExactVersion
	summary["mrr_at_20_by_kind"] = mrrByKind
	summary["cross_deployment_identical"] = crossIdentical
	summary["cross_deployment_total"] = crossTotal
	summary["seconds"] = time.Since(started).Seconds()
	out := map[string]any{
		"version":              "taskgate-kb-ekb4-report-v1",
		"campaign_class":       "kb-pilot",
		"publication_eligible": false,
		"generated_at":         time.Now().UTC().Format(time.RFC3339),
		"queryset_manifest":    querysetManifest,
		"publication_epoch":    epoch,
		"catalog_sha256":       "",
		"ks":                   ks,
		"summary":              summary,
		"deployments_detail":   reports,
	}
	encoded, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	t.Logf("E-KB4 report %s sha256 %s", reportPath, hex.EncodeToString(sum[:])[:16])
	t.Logf("identity by k %v; recall by kind %v; mrr@20 %v; cross-deployment %d/%d; errors %d", identityByK, recallByKind, mrrByKind, crossIdentical, crossTotal, errors)
	if errors > 0 {
		t.Fatalf("%d searches failed; see the report", errors)
	}
	for k, v := range identityByK {
		if v != 1 {
			t.Fatalf("gateway and direct exact retrieval differ at k=%s: identity %.4f (pre-registered expectation: 100%%)", k, v)
		}
	}
	if crossTotal > 0 && crossIdentical != crossTotal {
		t.Fatalf("deployments disagree on %d of %d hit sets", crossTotal-crossIdentical, crossTotal)
	}
}

func loadQueryset(t *testing.T, dir string) ([]kbqueryset.Query, kbqueryset.Manifest) {
	t.Helper()
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest kbqueryset.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "queries.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != manifest.QueriesSHA256 {
		t.Fatalf("queries.jsonl sha256 does not match the frozen manifest")
	}
	var queries []kbqueryset.Query
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		var query kbqueryset.Query
		if err := json.Unmarshal(scanner.Bytes(), &query); err != nil {
			t.Fatal(err)
		}
		queries = append(queries, query)
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].QueryID < queries[j].QueryID })
	return queries, manifest
}

func hitID(hit [3]any) string {
	return fmt.Sprintf("%v#%v#%v", hit[0], hit[1], hit[2])
}

func exactHitsScoped(t *testing.T, ctx context.Context, db *pgx.Conn, collection, vector, department string, k int) []directHit {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT doc_id, version, chunk_index FROM kb.chunks
		WHERE collection = $1 AND department = $2 AND classification IN ('public','internal','confidential','leadership')
		ORDER BY embedding <=> $3::vector, doc_id, version, chunk_index LIMIT $4`, collection, department, vector, k)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var hits []directHit
	for rows.Next() {
		var hit directHit
		if err := rows.Scan(&hit.doc, &hit.version, &hit.chunk); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, hit)
	}
	return hits
}

func schemaOf(t *testing.T, harness *gatewayHarness) string {
	t.Helper()
	return fmt.Sprintf("%p", harness.store)
}
