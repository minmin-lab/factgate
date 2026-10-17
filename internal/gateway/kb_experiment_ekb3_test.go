package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// E-KB3 overhead (docs/kb_experiment_design.md, pilot class): client latency
// of search / fetch / aggregate through the gateway against direct execution
// of the same retrieval on the business database, per tier (the database the
// DSN points at), with the gateway's settlement components taken from the
// response. Both arms submit the query vector, so the embedding service is
// outside both measurements. Rounds whose normal form repeats an earlier one
// are replay rounds and are reported separately. The run must not share the
// machine with other load; the report records the start time for the ledger.
//
//	KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT, KB_EKB3_REPORT, KB_EKB3_TIER (required)
//	KB_EKB3_ROUNDS (default 200), KB_EKB3_DEPLOYMENTS (default 3)
func TestEKB3Overhead(t *testing.T) {
	businessDSN, artifactDir, epoch := os.Getenv("KB_E2E_BUSINESS_DSN"), os.Getenv("KB_E2E_ARTIFACT_DIR"), os.Getenv("KB_E2E_EPOCH")
	endpoint, reportPath, tier := strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")), os.Getenv("KB_EKB3_REPORT"), os.Getenv("KB_EKB3_TIER")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || endpoint == "" || reportPath == "" || tier == "" {
		t.Skip("KB_E2E_*, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT, KB_EKB3_REPORT and KB_EKB3_TIER are required")
	}
	rounds, deployments := 200, 3
	if v := os.Getenv("KB_EKB3_ROUNDS"); v != "" {
		rounds, _ = strconv.Atoi(v)
	}
	if v := os.Getenv("KB_EKB3_DEPLOYMENTS"); v != "" {
		deployments, _ = strconv.Atoi(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()
	started := time.Now()
	fragment, err := os.ReadFile(filepath.Join(artifactDir, "kb", epoch, "catalog-fragment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := catalog.Parse([]byte(kbOnlyCatalog(t, readDemoCatalog(t), string(fragment), "", "")))
	if err != nil {
		t.Fatal(err)
	}
	built, err := catalogschema.Build(probe)
	if err != nil {
		t.Fatal(err)
	}
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 30 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
	if err != nil {
		t.Fatal(err)
	}
	attestation, err := probeConnector.Attestation(ctx)
	probeConnector.Close()
	if err != nil {
		t.Fatal(err)
	}
	parsedDSN, _ := url.Parse(businessDSN)
	database := strings.Trim(parsedDSN.Path, "/")
	product := probe.KnowledgeProducts[0]
	embedder, err := kbpublish.NewHTTPEmbedder(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(ctx, businessDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var chunkCount int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1`, product.Collection).Scan(&chunkCount); err != nil {
		t.Fatal(err)
	}
	department := "sales"
	words := []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更",
		"季度", "年度", "评估", "审计", "合同", "审批", "报告", "制度", "定价", "折扣", "渠道", "续约", "招标", "付款", "发票", "资产", "库存", "事故", "值班", "上线"}
	templates := []string{"销售部%s制度", "%s审批流程", "关于%s的规定", "%s报告", "%s合同金额", "销售部%s管理办法"}
	var texts []string
	for _, template := range templates {
		for _, word := range words {
			texts = append(texts, fmt.Sprintf(template, word))
		}
	}
	// Vectors once, as float64 for the tool and literal for the direct arm.
	vectorsF := map[string][]float64{}
	vectorsL := map[string]string{}
	for start := 0; start < len(texts); start += 32 {
		end := start + 32
		if end > len(texts) {
			end = len(texts)
		}
		normalized := make([]string, 0, end-start)
		for _, text := range texts[start:end] {
			normalized = append(normalized, retrieval.NormalizeText(text))
		}
		vectors, err := embedder.Embed(normalized)
		if err != nil || len(vectors) != len(normalized) {
			t.Fatalf("embed: %v", err)
		}
		for i, text := range texts[start:end] {
			f := make([]float64, len(vectors[i]))
			for j, v := range vectors[i] {
				f[j] = float64(v)
			}
			vectorsF[text] = f
			vectorsL[text] = vectorLiteralOf(vectors[i])
		}
	}
	type sample struct {
		Op         string             `json:"op"`
		Arm        string             `json:"arm"`
		K          int                `json:"k,omitempty"`
		Replay     bool               `json:"replay"`
		ClientMS   float64            `json:"client_ms"`
		Rows       int                `json:"rows"`
		Components map[string]float64 `json:"components_ms,omitempty"`
		DatabaseMS float64            `json:"database_ms,omitempty"`
		ChargedR   int64              `json:"charged_release,omitempty"`
		ChargedD   int64              `json:"charged_influence,omitempty"`
		ChargedO   int64              `json:"charged_outcome,omitempty"`
		Error      string             `json:"error,omitempty"`
	}
	var samples []sample
	components := func(result map[string]any) (map[string]float64, float64) {
		out := map[string]float64{}
		if raw, ok := result["component_ms"].(map[string]float64); ok {
			for k, v := range raw {
				out[k] = v
			}
		}
		dbms, _ := result["database_ms"].(float64)
		if dbms == 0 {
			if v, ok := result["database_ms"].(int64); ok {
				dbms = float64(v)
			}
		}
		return out, dbms
	}
	directSearch := func(vector string, k int) int {
		rows, err := db.Query(ctx, `SELECT doc_id, version, chunk_index, text FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') ORDER BY embedding <=> $3::vector, doc_id, version, chunk_index LIMIT $4`, product.Collection, department, vector, k)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		return n
	}
	directFetch := func(refs [][3]any) int {
		if len(refs) == 0 {
			return 0
		}
		var clauses []string
		args := []any{product.Collection, department}
		for _, ref := range refs {
			clauses = append(clauses, fmt.Sprintf("(doc_id = $%d AND version = $%d AND chunk_index = $%d)", len(args)+1, len(args)+2, len(args)+3))
			args = append(args, fmt.Sprint(ref[0]), toInt(ref[1]), toInt(ref[2]))
		}
		rows, err := db.Query(ctx, `SELECT doc_id, version, chunk_index, text FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') AND (`+strings.Join(clauses, " OR ")+`) ORDER BY doc_id, version, chunk_index`, args...)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		rows.Close()
		return n
	}
	directAggregate := func(field, function, classification string) {
		column := "value_numeric"
		expr := function + "(" + column + ")"
		if function == "count" {
			expr = "count(canonical_value)"
		}
		var out *string
		if err := db.QueryRow(ctx, `SELECT `+expr+`::text FROM kb.extracted_values v JOIN kb.documents d ON d.collection = v.collection AND d.doc_id = v.doc_id AND d.version = v.version WHERE v.collection = $1 AND d.department = $2 AND d.classification = $3 AND v.field = $4 AND v.value_type = 'numeric'`, product.Collection, department, classification, field).Scan(&out); err != nil {
			t.Fatal(err)
		}
	}
	fields := []string{"amount", "approved_amount", "headline_metric"}
	functions := []string{"sum", "count", "min", "max"}
	classifications := []string{"internal", "public"}
	for d := 1; d <= deployments; d++ {
		harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
			return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
		}})
		harness.taskBudget = &domain.Budget{MaxQueries: 100000, MaxRows: 10000000, MaxDBTime: 24 * time.Hour, PerQueryTimeout: 60 * time.Second, TaskTTL: 12 * time.Hour}
		builtD, err := catalogschema.Build(harness.catalog)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 60 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 1000, MaxConnections: 4,
			ExpectedSchema: builtD.Entries, ExpectedSchemaDigest: builtD.Source.SchemaDigest,
			ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: builtD.Source.DatasourceID, Database: builtD.Source.Database, User: builtD.Source.User, PostgreSQLMajorVersion: builtD.Source.PostgreSQLMajorVersion}})
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
		chunks, documents, values := product.Products.Chunks, product.Products.Documents, product.Products.Values
		taskID := fmt.Sprintf("ekb3-d%d", d)
		harness.createTaskWithGrantScopeAndExposureProfile(t, taskID, nil,
			control.ExposureLimits{ReleaseFacts: 100000000, InfluenceFacts: 1000000000, OutcomeFacts: 10000000}, exposure.ProfileV5,
			[]string{chunks, documents, values}, map[string][]string{
				chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
				documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
				values:    {"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"},
			}, domain.SensitivityHigh, map[string]any{"kb_department": []any{department}, "classification": []any{"public", "internal"}})
		rng := rand.New(rand.NewPCG(uint64(d), 20261016))
		order := rng.Perm(len(texts))
		seenForms := map[string]bool{}
		for round := 0; round < rounds; round++ {
			k := 5
			if round%2 == 1 {
				k = 20
			}
			text := texts[order[round%len(order)]]
			if round%10 == 9 && round >= 10 {
				text = texts[order[(round-10)%len(order)]] // replay of an earlier round
			}
			formKey := fmt.Sprintf("search|%s|%d", text, k)
			replay := seenForms[formKey]
			seenForms[formKey] = true
			// Direct search.
			t0 := time.Now()
			n := directSearch(vectorsL[text], k)
			samples = append(samples, sample{Op: "search", Arm: "direct", K: k, Replay: replay, ClientMS: ms(time.Since(t0)), Rows: n})
			// Gateway search (vector query).
			t0 = time.Now()
			result, err := callGatewayTool(harness.service, harness.alice, "search", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-search-%d", taskID, round),
				"knowledge_product": product.Name, "query": map[string]any{"vector": vectorsF[text]}, "k": k})
			s := sample{Op: "search", Arm: "factgate", K: k, Replay: replay, ClientMS: ms(time.Since(t0))}
			var hits [][3]any
			if err != nil {
				s.Error = err.Error()
			} else {
				hits = searchHits(t, result)
				s.Rows = len(hits)
				s.Components, s.DatabaseMS = components(result)
				addCharge(result, &s.ChargedR, &s.ChargedD, &s.ChargedO)
			}
			samples = append(samples, s)
			// Fetch the hits back (novel the first time this set is fetched).
			if len(hits) > 0 {
				refs := make([]map[string]any, 0, len(hits))
				for _, hit := range hits {
					refs = append(refs, map[string]any{"doc_id": hit[0], "version": hit[1], "chunk_index": hit[2]})
				}
				fetchKey := "fetch|" + strings.Join(hitIDs(hits), ",")
				fetchReplay := seenForms[fetchKey]
				seenForms[fetchKey] = true
				t0 = time.Now()
				n := directFetch(hits)
				samples = append(samples, sample{Op: "fetch", Arm: "direct", K: k, Replay: fetchReplay, ClientMS: ms(time.Since(t0)), Rows: n})
				t0 = time.Now()
				result, err := callGatewayTool(harness.service, harness.alice, "fetch", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-fetch-%d", taskID, round),
					"knowledge_product": product.Name, "chunks": refs})
				fs := sample{Op: "fetch", Arm: "factgate", K: k, Replay: fetchReplay, ClientMS: ms(time.Since(t0))}
				if err != nil {
					fs.Error = err.Error()
				} else {
					fs.Rows = len(searchHits(t, result))
					fs.Components, fs.DatabaseMS = components(result)
					addCharge(result, &fs.ChargedR, &fs.ChargedD, &fs.ChargedO)
				}
				samples = append(samples, fs)
			}
			// Aggregate: 24 distinct forms, then replays.
			field, function, classification := fields[round%3], functions[(round/3)%4], classifications[(round/12)%2]
			aggKey := fmt.Sprintf("aggregate|%s|%s|%s", field, function, classification)
			aggReplay := seenForms[aggKey]
			seenForms[aggKey] = true
			t0 = time.Now()
			directAggregate(field, function, classification)
			samples = append(samples, sample{Op: "aggregate", Arm: "direct", Replay: aggReplay, ClientMS: ms(time.Since(t0)), Rows: 1})
			t0 = time.Now()
			result, err = callGatewayTool(harness.service, harness.alice, "aggregate", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-agg-%d", taskID, round),
				"knowledge_product": product.Name, "field": field, "aggregate": function,
				"filters": []map[string]any{{"attribute": "classification", "op": "=", "value": classification}}})
			as := sample{Op: "aggregate", Arm: "factgate", Replay: aggReplay, ClientMS: ms(time.Since(t0))}
			if err != nil {
				as.Error = err.Error()
			} else {
				as.Rows = 1
				as.Components, as.DatabaseMS = components(result)
				addCharge(result, &as.ChargedR, &as.ChargedD, &as.ChargedO)
			}
			samples = append(samples, as)
			if (round+1)%50 == 0 {
				t.Logf("tier %s deployment %d: %d/%d rounds, %.0fs", tier, d, round+1, rounds, time.Since(started).Seconds())
			}
		}
		connector.Close()
	}
	// Summary.
	type cell struct {
		N          int                `json:"n"`
		P50        float64            `json:"p50_ms"`
		P95        float64            `json:"p95_ms"`
		Mean       float64            `json:"mean_ms"`
		Components map[string]float64 `json:"components_p50_ms,omitempty"`
		Errors     int                `json:"errors"`
	}
	groups := map[string][]sample{}
	for _, s := range samples {
		key := fmt.Sprintf("%s/%s/k%d/%s", s.Op, s.Arm, s.K, map[bool]string{false: "novel", true: "replay"}[s.Replay])
		if s.Op == "aggregate" {
			key = fmt.Sprintf("%s/%s/%s", s.Op, s.Arm, map[bool]string{false: "novel", true: "replay"}[s.Replay])
		}
		groups[key] = append(groups[key], s)
	}
	summary := map[string]cell{}
	for key, group := range groups {
		var lat []float64
		comp := map[string][]float64{}
		c := cell{N: len(group)}
		for _, s := range group {
			if s.Error != "" {
				c.Errors++
				continue
			}
			lat = append(lat, s.ClientMS)
			for name, v := range s.Components {
				comp[name] = append(comp[name], v)
			}
		}
		if len(lat) > 0 {
			sort.Float64s(lat)
			var sum float64
			for _, v := range lat {
				sum += v
			}
			c.P50, c.P95, c.Mean = lat[len(lat)/2], lat[(len(lat)*95)/100], sum/float64(len(lat))
		}
		if len(comp) > 0 {
			c.Components = map[string]float64{}
			for name, vs := range comp {
				sort.Float64s(vs)
				c.Components[name] = vs[len(vs)/2]
			}
		}
		summary[key] = c
	}
	out := map[string]any{"version": "taskgate-kb-ekb3-report-v1", "campaign_class": "kb-pilot", "publication_eligible": false,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "started_at": started.Format(time.RFC3339), "tier": tier, "chunks": chunkCount, "publication_epoch": epoch,
		"rounds": rounds, "deployments": deployments, "department": department, "summary": summary, "samples": samples, "seconds": time.Since(started).Seconds()}
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
	keys := make([]string, 0, len(summary))
	for key := range summary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c := summary[key]
		t.Logf("%-36s n=%3d p50=%7.1f p95=%7.1f ms errors=%d comp=%v", key, c.N, c.P50, c.P95, c.Errors, c.Components)
	}
	t.Logf("E-KB3 tier %s (%d chunks) report %s sha256 %s", tier, chunkCount, reportPath, hex.EncodeToString(sum[:])[:16])
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func toInt(v any) int {
	switch typed := v.(type) {
	case float64:
		return int(typed)
	case int64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		n, _ := typed.Int64()
		return int(n)
	default:
		n, _ := strconv.Atoi(fmt.Sprint(v))
		return n
	}
}

func hitIDs(hits [][3]any) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hitID(hit))
	}
	return ids
}
