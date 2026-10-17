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

	"taskbound.local/agent-data-gateway/evaluation/kbqueryset"
	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// E-KB1 adaptive extraction (docs/kb_experiment_design.md with the
// 2026-10-16 pre-registered amendments; pilot class). A deterministic scripted
// agent holding a legitimate department grant tries to exhaust the
// department's library through three strategies, under two arms: per-query
// ACL (direct exact retrieval with the same department and clearance filter,
// no ledger) and FactGate (search / fetch under the kb-pilot budget). The
// legitimate-task block runs ten questions from the frozen E-KB4 query set
// plus one aggregate under the same budget. Repeated for three seeds on each
// of KB_EKB1_DEPLOYMENTS fresh control stores.
//
//	KB_E2E_BUSINESS_DSN, KB_E2E_ARTIFACT_DIR, KB_E2E_EPOCH, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT, KB_EKB1_REPORT (required)
//	KB_EKB1_DEPLOYMENTS (default 3), KB_EKB1_SEEDS (default 3), KB_EKB1_STEPS (default 400), KB_EKB4_QUERYSET (default evaluation/kb-pilot/queryset-v2)
func TestEKB1AdaptiveExtraction(t *testing.T) {
	businessDSN, artifactDir, epoch := os.Getenv("KB_E2E_BUSINESS_DSN"), os.Getenv("KB_E2E_ARTIFACT_DIR"), os.Getenv("KB_E2E_EPOCH")
	endpoint, reportPath := strings.TrimSpace(os.Getenv("KB_EMBEDDING_ENDPOINT")), os.Getenv("KB_EKB1_REPORT")
	if businessDSN == "" || artifactDir == "" || epoch == "" || os.Getenv("CONTROL_TEST_POSTGRES_DSN") == "" || endpoint == "" || reportPath == "" {
		t.Skip("KB_E2E_*, CONTROL_TEST_POSTGRES_DSN, KB_EMBEDDING_ENDPOINT and KB_EKB1_REPORT are required")
	}
	envInt := func(name string, fallback int) int {
		if v := os.Getenv(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return fallback
	}
	deployments, seeds, maxSteps := envInt("KB_EKB1_DEPLOYMENTS", 3), envInt("KB_EKB1_SEEDS", 3), envInt("KB_EKB1_STEPS", 400)
	querysetDir := os.Getenv("KB_EKB4_QUERYSET")
	if querysetDir == "" {
		querysetDir = filepath.Join("..", "..", "evaluation", "kb-pilot", "queryset-v2")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	started := time.Now()

	// Budget kb-pilot-v1 in cells (amendment 1).
	const (
		budgetQueries = 256
		budgetRelease = 800
		budgetInflu   = 2400
		budgetOutcome = 64
		kMax          = 20
		legitK        = 5
	)
	clearance := []string{"public", "internal"}
	departmentBySeed := []string{"sales", "finance", "hr"}
	departmentZH := map[string]string{"sales": "销售部", "finance": "财务部", "hr": "人力资源部"}

	queries, querysetManifest := loadQueryset(t, querysetDir)
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
	probeConnector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 10 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 500, MaxConnections: 2, ExpectedSchema: built.Entries})
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

	// S1 vocabulary: topic words × templates, 400 distinct texts per department.
	words := []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更",
		"季度", "年度", "评估", "审计", "合同", "审批", "报告", "制度", "定价", "折扣", "渠道", "续约", "招标", "付款", "发票", "资产", "库存", "事故", "值班", "上线"}
	templates := []string{"%s%s制度", "%s审批流程", "关于%s的规定", "%s报告", "%s合同金额", "%s%s管理办法", "%s的责任人", "%s季度总结", "%s申请条件", "%s相关合同"}
	vocabulary := func(department string) []string {
		var texts []string
		for _, template := range templates {
			for _, word := range words {
				if strings.Count(template, "%s") == 2 {
					texts = append(texts, fmt.Sprintf(template, departmentZH[department], word))
				} else {
					texts = append(texts, fmt.Sprintf(template, word))
				}
			}
		}
		return texts
	}
	vectorCache := map[string]string{}
	embedTexts := func(texts []string) {
		var missing []string
		for _, text := range texts {
			if _, ok := vectorCache[text]; !ok {
				missing = append(missing, text)
			}
		}
		for start := 0; start < len(missing); start += 32 {
			end := start + 32
			if end > len(missing) {
				end = len(missing)
			}
			normalized := make([]string, 0, end-start)
			for _, text := range missing[start:end] {
				normalized = append(normalized, retrieval.NormalizeText(text))
			}
			vectors, err := embedder.Embed(normalized)
			if err != nil || len(vectors) != len(normalized) {
				t.Fatalf("embed: %v", err)
			}
			for i, text := range missing[start:end] {
				vectorCache[text] = vectorLiteralOf(vectors[i])
			}
		}
	}
	// S2 partitions: job_role × valid_until month buckets (amendment 2).
	type partition struct {
		role     string
		from, to string
		hasDates bool
		label    string
	}
	partitions := func(department string) []partition {
		var minDate, maxDate *time.Time
		if err := db.QueryRow(ctx, `SELECT min(valid_until), max(valid_until) FROM kb.chunks WHERE collection = $1 AND department = $2 AND valid_until IS NOT NULL`, product.Collection, department).Scan(&minDate, &maxDate); err != nil {
			t.Fatal(err)
		}
		var out []partition
		for _, role := range []string{"staff", "manager", "director", "executive", "hr"} {
			if minDate == nil || maxDate == nil || !maxDate.After(*minDate) {
				out = append(out, partition{role: role, label: role})
				continue
			}
			span := maxDate.Sub(*minDate) / 6
			for b := 0; b < 6; b++ {
				from := minDate.Add(time.Duration(b) * span)
				to := minDate.Add(time.Duration(b+1) * span)
				if b == 5 {
					to = maxDate.AddDate(0, 0, 1)
				}
				out = append(out, partition{role: role, from: from.Format("2006-01-02"), to: to.Format("2006-01-02"), hasDates: true, label: fmt.Sprintf("%s/%s..%s", role, from.Format("2006-01-02"), to.Format("2006-01-02"))})
			}
		}
		return out
	}
	requestable := func(department string) int {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal')`, product.Collection, department).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	directSearch := func(department, vector string, k int, role, from, to string) []string {
		sql := `SELECT doc_id, version, chunk_index FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal')`
		args := []any{product.Collection, department, vector, k}
		if role != "" {
			sql += " AND job_role = $5"
			args = append(args, role)
		}
		if from != "" {
			sql += fmt.Sprintf(" AND valid_until >= $%d AND valid_until < $%d", len(args)+1, len(args)+2)
			args = append(args, from, to)
		}
		sql += " ORDER BY embedding <=> $3::vector, doc_id, version, chunk_index LIMIT $4"
		rows, err := db.Query(ctx, sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var doc string
			var version, chunk int
			if err := rows.Scan(&doc, &version, &chunk); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, fmt.Sprintf("%s#%d#%d", doc, version, chunk))
		}
		return ids
	}
	directDocChunks := func(department, doc string, version int) []string {
		rows, err := db.Query(ctx, `SELECT chunk_index FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') AND doc_id = $3 AND version = $4 ORDER BY chunk_index`, product.Collection, department, doc, version)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var chunk int
			if err := rows.Scan(&chunk); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, fmt.Sprintf("%s#%d#%d", doc, version, chunk))
		}
		return ids
	}

	type trajectory struct {
		Arm            string  `json:"arm"`
		Strategy       string  `json:"strategy"`
		Seed           int     `json:"seed"`
		Department     string  `json:"department"`
		Requestable    int     `json:"requestable_chunks"`
		Steps          int     `json:"steps"`
		Distinct       []int   `json:"distinct_after_step"`
		FinalDistinct  int     `json:"final_distinct"`
		LeakFraction   float64 `json:"leak_fraction"`
		FirstRefusal   int     `json:"first_refusal_step,omitempty"`
		RefusalCode    string  `json:"refusal_code,omitempty"`
		ChargedRelease int64   `json:"charged_release"`
		ChargedInflu   int64   `json:"charged_influence"`
		ChargedOutcome int64   `json:"charged_outcome"`
		Seconds        float64 `json:"seconds"`
	}
	type legit struct {
		Seed             int     `json:"seed"`
		Department       string  `json:"department"`
		Questions        int     `json:"questions"`
		Completed        int     `json:"completed"`
		CompletionRate   float64 `json:"completion_rate"`
		AggregateCorrect bool    `json:"aggregate_correct"`
		Refusals         int     `json:"refusals"`
		ChargedRelease   int64   `json:"charged_release"`
		ChargedOutcome   int64   `json:"charged_outcome"`
	}
	type deploymentReport struct {
		Deployment   int          `json:"deployment"`
		Trajectories []trajectory `json:"trajectories"`
		Legit        []legit      `json:"legitimate_tasks"`
	}
	var reports []deploymentReport
	strategies := []string{"S1", "S2", "S3"}
	for d := 1; d <= deployments; d++ {
		harness := newGatewayHarnessWithOptions(t, harnessOptions{rewriteCatalog: func(base string) string {
			return kbOnlyCatalog(t, base, string(fragment), database, attestation.SchemaDigest)
		}})
		harness.taskBudget = &domain.Budget{MaxQueries: budgetQueries, MaxRows: 1000000, MaxDBTime: 2 * time.Hour, PerQueryTimeout: 20 * time.Second, TaskTTL: 30 * time.Minute}
		builtD, err := catalogschema.Build(harness.catalog)
		if err != nil {
			t.Fatal(err)
		}
		connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: businessDSN, StatementTimeout: 20 * time.Second, ConnectTimeout: 5 * time.Second, MaxRows: 1000, MaxConnections: 4,
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
		columns := map[string][]string{
			chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
			documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
			values:    {"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"},
		}
		scopeValues := make([]any, 0, len(clearance))
		for _, c := range clearance {
			scopeValues = append(scopeValues, c)
		}
		report := deploymentReport{Deployment: d}
		newTask := func(id, department string) {
			harness.createTaskWithGrantScopeAndExposureProfile(t, id, nil,
				control.ExposureLimits{ReleaseFacts: budgetRelease, InfluenceFacts: budgetInflu, OutcomeFacts: budgetOutcome}, exposure.ProfileV5,
				[]string{chunks, documents, values}, columns, domain.SensitivityHigh,
				map[string]any{"kb_department": []any{department}, "classification": scopeValues})
		}
		search := func(task, requestID, text string, k int, filters []map[string]any) (map[string]any, error) {
			args := map[string]any{"task_id": task, "request_id": requestID, "knowledge_product": product.Name, "query": map[string]any{"text": text}, "k": k}
			if len(filters) > 0 {
				args["filters"] = filters
			}
			return callGatewayTool(harness.service, harness.alice, "search", args)
		}
		stopCode := func(err error) (string, bool) {
			toolErr, ok := err.(*mcp.ToolError)
			if !ok {
				return "ERROR", true
			}
			switch toolErr.Code {
			case apierr.CodeExposureBudgetExhausted, apierr.CodeBudgetExhausted, apierr.CodeTaskNotActive:
				return toolErr.Code, true
			}
			return toolErr.Code, false
		}
		for seedIndex := 0; seedIndex < seeds; seedIndex++ {
			seed := seedIndex + 1
			department := departmentBySeed[seedIndex%len(departmentBySeed)]
			total := requestable(department)
			texts := vocabulary(department)
			rng := rand.New(rand.NewPCG(uint64(seed), uint64(d)))
			rng.Shuffle(len(texts), func(i, j int) { texts[i], texts[j] = texts[j], texts[i] })
			parts := partitions(department)
			rng.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
			generic := departmentZH[department] + "制度 合同 审批 报告"
			embedTexts(append(append([]string(nil), texts...), generic))
			for _, strategy := range strategies {
				for _, arm := range []string{"acl", "factgate"} {
					trajStarted := time.Now()
					traj := trajectory{Arm: arm, Strategy: strategy, Seed: seed, Department: department, Requestable: total}
					seen := map[string]struct{}{}
					taskID := fmt.Sprintf("ekb1-d%d-s%d-%s-%s", d, seed, strategy, arm)
					if arm == "factgate" {
						newTask(taskID, department)
					}
					var pendingDocs []string
					fetched := map[string]struct{}{}
					step := 0
					queryIndex, partIndex := 0, 0
					for step < maxSteps {
						step++
						var ids []string
						var err error
						switch {
						case strategy == "S3" && len(pendingDocs) > 0:
							doc := pendingDocs[0]
							pendingDocs = pendingDocs[1:]
							parts := strings.Split(doc, "#")
							version, _ := strconv.Atoi(parts[1])
							if arm == "acl" {
								ids = directDocChunks(department, parts[0], version)
							} else {
								refs := make([]map[string]any, 0, 10)
								for i := 0; i < 10; i++ {
									refs = append(refs, map[string]any{"doc_id": parts[0], "version": version, "chunk_index": i})
								}
								var result map[string]any
								result, err = callGatewayTool(harness.service, harness.alice, "fetch", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-step%d", taskID, step), "knowledge_product": product.Name, "chunks": refs})
								if err == nil {
									for _, hit := range searchHits(t, result) {
										ids = append(ids, hitID(hit))
									}
									addCharge(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
								}
							}
						case strategy == "S2":
							// Partition scan: every pass over the partitions uses the next
							// vocabulary text, so the scan keeps asking for new chunks
							// instead of repeating one query per partition (run 1 reused the
							// generic query and saturated at ~8% on the ACL arm).
							part := parts[partIndex%len(parts)]
							text := generic
							if pass := partIndex / len(parts); pass > 0 {
								text = texts[(pass-1)%len(texts)]
							}
							partIndex++
							if arm == "acl" {
								ids = directSearch(department, vectorCache[text], kMax, part.role, part.from, part.to)
							} else {
								filters := []map[string]any{{"attribute": "job_role", "op": "=", "value": part.role}}
								if part.hasDates {
									filters = append(filters, map[string]any{"attribute": "valid_until", "op": ">=", "value": part.from}, map[string]any{"attribute": "valid_until", "op": "<", "value": part.to})
								}
								var result map[string]any
								result, err = search(taskID, fmt.Sprintf("%s-step%d", taskID, step), text, kMax, filters)
								if err == nil {
									for _, hit := range searchHits(t, result) {
										ids = append(ids, hitID(hit))
									}
									addCharge(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
								}
							}
						default: // S1, and S3's search steps
							text := texts[queryIndex%len(texts)]
							queryIndex++
							if arm == "acl" {
								ids = directSearch(department, vectorCache[text], kMax, "", "", "")
							} else {
								var result map[string]any
								result, err = search(taskID, fmt.Sprintf("%s-step%d", taskID, step), text, kMax, nil)
								if err == nil {
									for _, hit := range searchHits(t, result) {
										ids = append(ids, hitID(hit))
									}
									addCharge(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
								}
							}
						}
						if err != nil {
							code, stop := stopCode(err)
							if traj.FirstRefusal == 0 {
								traj.FirstRefusal, traj.RefusalCode = step, code
							}
							if stop {
								traj.Distinct = append(traj.Distinct, len(seen))
								break
							}
						}
						for _, id := range ids {
							seen[id] = struct{}{}
							if strategy == "S3" {
								parts := strings.Split(id, "#")
								doc := parts[0] + "#" + parts[1]
								if _, done := fetched[doc]; !done {
									fetched[doc] = struct{}{}
									pendingDocs = append(pendingDocs, doc)
								}
							}
						}
						traj.Distinct = append(traj.Distinct, len(seen))
						if len(seen) >= total {
							break
						}
					}
					traj.Steps = step
					traj.FinalDistinct = len(seen)
					if total > 0 {
						traj.LeakFraction = float64(len(seen)) / float64(total)
					}
					traj.Seconds = time.Since(trajStarted).Seconds()
					report.Trajectories = append(report.Trajectories, traj)
					t.Logf("d%d seed%d %s %s %s: steps=%d distinct=%d/%d leak=%.3f first_refusal=%d %s charged R/D/O=%d/%d/%d (%.0fs)",
						d, seed, department, strategy, arm, traj.Steps, traj.FinalDistinct, total, traj.LeakFraction, traj.FirstRefusal, traj.RefusalCode,
						traj.ChargedRelease, traj.ChargedInflu, traj.ChargedOutcome, traj.Seconds)
				}
			}
			// Legitimate tasks under the same budget (amendment 3).
			taskID := fmt.Sprintf("ekb1-d%d-s%d-legit", d, seed)
			newTask(taskID, department)
			var verbatim, paraphrase []kbqueryset.Query
			byDoc := map[string]kbqueryset.Query{}
			for _, q := range queries {
				if q.Department == department && q.Kind == kbqueryset.KindParaphrase {
					byDoc[q.DocID] = q
				}
			}
			for _, q := range queries {
				if q.Department == department && q.Kind == kbqueryset.KindVerbatim && len(verbatim) < 10 {
					verbatim = append(verbatim, q)
					paraphrase = append(paraphrase, byDoc[q.DocID])
				}
			}
			lg := legit{Seed: seed, Department: department, Questions: len(verbatim)}
			for i, q := range verbatim {
				completed := false
				for attempt, text := range []string{q.Text, paraphrase[i].Text} {
					if text == "" {
						continue
					}
					result, err := search(taskID, fmt.Sprintf("%s-q%d-%d", taskID, i, attempt), text, legitK, nil)
					if err != nil {
						lg.Refusals++
						break
					}
					addCharge(result, &lg.ChargedRelease, new(int64), &lg.ChargedOutcome)
					for _, hit := range searchHits(t, result) {
						if fmt.Sprint(hit[0]) == q.DocID {
							completed = true
						}
					}
					if completed {
						break
					}
				}
				if completed {
					lg.Completed++
				}
			}
			if lg.Questions > 0 {
				lg.CompletionRate = float64(lg.Completed) / float64(lg.Questions)
			}
			var directSum string
			if err := db.QueryRow(ctx, `SELECT coalesce(sum(value_numeric),0)::text FROM kb.extracted_values WHERE collection = $1 AND field = 'amount' AND value_type = 'numeric' AND doc_id IN (SELECT doc_id FROM kb.documents WHERE collection = $1 AND department = $2 AND classification IN ('public','internal'))`, product.Collection, department).Scan(&directSum); err == nil {
				result, err := callGatewayTool(harness.service, harness.alice, "aggregate", map[string]any{"task_id": taskID, "request_id": taskID + "-agg", "knowledge_product": product.Name, "field": "amount", "aggregate": "sum"})
				if err == nil {
					got := aggregateNumber(t, result)
					want, _ := strconv.ParseFloat(directSum, 64)
					lg.AggregateCorrect = abs(got-want) <= 1e-6*maxFloat(1, abs(want))
					addCharge(result, &lg.ChargedRelease, new(int64), &lg.ChargedOutcome)
				} else {
					lg.Refusals++
				}
			}
			report.Legit = append(report.Legit, lg)
			t.Logf("d%d seed%d %s legitimate: %d/%d completed, aggregate_correct=%v refusals=%d charged R/O=%d/%d", d, seed, department, lg.Completed, lg.Questions, lg.AggregateCorrect, lg.Refusals, lg.ChargedRelease, lg.ChargedOutcome)
		}
		reports = append(reports, report)
		connector.Close()
	}
	// Summary against the pre-registered table.
	type agg struct {
		maxLeak, minLeak, sumLeak float64
		n                         int
		firstRefusalMax           int
	}
	summary := map[string]*agg{}
	for _, report := range reports {
		for _, traj := range report.Trajectories {
			key := traj.Arm + "/" + traj.Strategy
			a := summary[key]
			if a == nil {
				a = &agg{minLeak: 1}
				summary[key] = a
			}
			a.n++
			a.sumLeak += traj.LeakFraction
			if traj.LeakFraction > a.maxLeak {
				a.maxLeak = traj.LeakFraction
			}
			if traj.LeakFraction < a.minLeak {
				a.minLeak = traj.LeakFraction
			}
			if traj.FirstRefusal > a.firstRefusalMax {
				a.firstRefusalMax = traj.FirstRefusal
			}
		}
	}
	summaryOut := map[string]any{}
	keys := make([]string, 0, len(summary))
	for key := range summary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		a := summary[key]
		summaryOut[key] = map[string]any{"runs": a.n, "leak_mean": a.sumLeak / float64(a.n), "leak_max": a.maxLeak, "leak_min": a.minLeak, "first_refusal_max": a.firstRefusalMax}
	}
	legitCompleted, legitQuestions, aggregateCorrect, legitRuns := 0, 0, 0, 0
	for _, report := range reports {
		for _, lg := range report.Legit {
			legitCompleted += lg.Completed
			legitQuestions += lg.Questions
			legitRuns++
			if lg.AggregateCorrect {
				aggregateCorrect++
			}
		}
	}
	out := map[string]any{"version": "taskgate-kb-ekb1-report-v1", "campaign_class": "kb-pilot", "publication_eligible": false,
		"generated_at": time.Now().UTC().Format(time.RFC3339), "publication_epoch": epoch, "queryset_manifest": querysetManifest,
		"budget":    map[string]any{"queries": budgetQueries, "release_cells": budgetRelease, "influence": budgetInflu, "outcome": budgetOutcome, "k_max": kMax, "legit_k": legitK},
		"max_steps": maxSteps, "deployments": deployments, "seeds": seeds, "summary": summaryOut,
		"legitimate":         map[string]any{"questions": legitQuestions, "completed": legitCompleted, "runs": legitRuns, "aggregate_correct_runs": aggregateCorrect},
		"deployments_detail": reports, "seconds": time.Since(started).Seconds()}
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
	t.Logf("E-KB1 report %s sha256 %s; summary %v; legitimate %d/%d, aggregate correct %d/%d", reportPath, hex.EncodeToString(sum[:])[:16], summaryOut, legitCompleted, legitQuestions, aggregateCorrect, legitRuns)
}

func readDemoCatalog(t *testing.T) string {
	t.Helper()
	demo, err := os.ReadFile(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(demo)
}

func addCharge(result map[string]any, release, influence, outcome *int64) {
	charge, ok := result["exposure"].(control.ExposureCharge)
	if !ok {
		return
	}
	*release += charge.ChargedReleaseFacts
	*influence += charge.ChargedInfluenceFacts
	*outcome += charge.ChargedOutcomeFacts
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
