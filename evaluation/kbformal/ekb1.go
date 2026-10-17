package kbformal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbqueryset"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// The E-KB1 and E-KB2(b) ports below follow the pilot runners
// (internal/gateway/kb_experiment_ekb1_test.go, kb_experiment_ekb2_test.go)
// step for step: the same vocabulary, partitions, seeds, k and stop rules.
// Only the transport differs: tasks come from the real request/OA path, the
// FactGate arm's rows from the delivered canonical artifact, and the TTL of
// E-KB2(b) is waited out on the wall clock instead of a test clock.

var ekbWords = []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更",
	"季度", "年度", "评估", "审计", "合同", "审批", "报告", "制度", "定价", "折扣", "渠道", "续约", "招标", "付款", "发票", "资产", "库存", "事故", "值班", "上线"}
var ekbTemplates = []string{"%s%s制度", "%s审批流程", "关于%s的规定", "%s报告", "%s合同金额", "%s%s管理办法", "%s的责任人", "%s季度总结", "%s申请条件", "%s相关合同"}
var departmentZH = map[string]string{"sales": "销售部", "finance": "财务部", "hr": "人力资源部"}

func vocabulary(department string) []string {
	var texts []string
	for _, template := range ekbTemplates {
		for _, word := range ekbWords {
			if strings.Count(template, "%s") == 2 {
				texts = append(texts, fmt.Sprintf(template, departmentZH[department], word))
			} else {
				texts = append(texts, fmt.Sprintf(template, word))
			}
		}
	}
	return texts
}

type partition struct {
	role     string
	from, to string
	hasDates bool
}

func (d *Deployment) partitions(ctx context.Context, department string) ([]partition, error) {
	var minDate, maxDate *time.Time
	if err := d.Business.QueryRow(ctx, `SELECT min(valid_until), max(valid_until) FROM kb.chunks WHERE collection = $1 AND department = $2 AND valid_until IS NOT NULL`,
		d.Product.Collection, department).Scan(&minDate, &maxDate); err != nil {
		return nil, err
	}
	var out []partition
	for _, role := range []string{"staff", "manager", "director", "executive", "hr"} {
		if minDate == nil || maxDate == nil || !maxDate.After(*minDate) {
			out = append(out, partition{role: role})
			continue
		}
		span := maxDate.Sub(*minDate) / 6
		for b := 0; b < 6; b++ {
			from, to := minDate.Add(time.Duration(b)*span), minDate.Add(time.Duration(b+1)*span)
			if b == 5 {
				to = maxDate.AddDate(0, 0, 1)
			}
			out = append(out, partition{role: role, from: from.Format("2006-01-02"), to: to.Format("2006-01-02"), hasDates: true})
		}
	}
	return out, nil
}

// vectors embeds texts in batches of 32 exactly as the pilot runner's direct
// arm did, and renders pgvector literals.
type vectorCache struct {
	embedder kbpublish.Embedder
	literals map[string]string
}

func (cache *vectorCache) embed(texts []string) error {
	var missing []string
	for _, text := range texts {
		if _, ok := cache.literals[text]; !ok {
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
		vectors, err := cache.embedder.Embed(normalized)
		if err != nil || len(vectors) != len(normalized) {
			return fmt.Errorf("embed: %v", err)
		}
		for i, text := range missing[start:end] {
			parts := make([]string, 0, len(vectors[i]))
			for _, value := range vectors[i] {
				parts = append(parts, fmt.Sprintf("%g", value))
			}
			cache.literals[text] = "[" + strings.Join(parts, ",") + "]"
		}
	}
	return nil
}

func hitIDOf(row []any) string { return fmt.Sprintf("%v#%v#%v", row[0], row[1], row[2]) }

func (d *Deployment) directSearch(ctx context.Context, department, vector string, k int, role, from, to string) ([]string, error) {
	sql := `SELECT doc_id, version, chunk_index FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal')`
	args := []any{d.Product.Collection, department, vector, k}
	if role != "" {
		sql += " AND job_role = $5"
		args = append(args, role)
	}
	if from != "" {
		sql += fmt.Sprintf(" AND valid_until >= $%d AND valid_until < $%d", len(args)+1, len(args)+2)
		args = append(args, from, to)
	}
	sql += " ORDER BY embedding <=> $3::vector, doc_id, version, chunk_index LIMIT $4"
	return d.identities(ctx, sql, args...)
}

func (d *Deployment) identities(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := d.Business.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var doc string
		var version, chunk int
		if err := rows.Scan(&doc, &version, &chunk); err != nil {
			return nil, err
		}
		ids = append(ids, fmt.Sprintf("%s#%d#%d", doc, version, chunk))
	}
	return ids, rows.Err()
}

// stopCode mirrors the pilot runner: budget exhaustion and an inactive task end
// a trajectory; any other refusal is recorded and the trajectory continues.
func stopCode(err error) (string, bool) {
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		return "ERROR", true
	}
	switch toolErr.Code {
	case "EXPOSURE_BUDGET_EXHAUSTED", "BUDGET_EXHAUSTED", "TASK_NOT_ACTIVE":
		return toolErr.Code, true
	}
	return toolErr.Code, false
}

// LoadQueryset reads the frozen query set and checks queries.jsonl against its manifest.
func LoadQueryset(dir string) ([]kbqueryset.Query, kbqueryset.Manifest, error) {
	var manifest kbqueryset.Manifest
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, manifest, err
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, manifest, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "queries.jsonl"))
	if err != nil {
		return nil, manifest, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != manifest.QueriesSHA256 {
		return nil, manifest, errors.New("queries.jsonl sha256 does not match the frozen manifest")
	}
	var queries []kbqueryset.Query
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var query kbqueryset.Query
		if err := json.Unmarshal(scanner.Bytes(), &query); err != nil {
			return nil, manifest, err
		}
		queries = append(queries, query)
	}
	sort.Slice(queries, func(i, j int) bool { return queries[i].QueryID < queries[j].QueryID })
	return queries, manifest, scanner.Err()
}

func (d *Deployment) chunkColumns() map[string][]string {
	return map[string][]string{
		d.Product.Products.Chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
		d.Product.Products.Documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
	}
}

// ---- E-KB1: adaptive extraction ----

// EKB1Trajectory matches the pilot report's trajectory entries.
type EKB1Trajectory struct {
	Arm            string         `json:"arm"`
	Strategy       string         `json:"strategy"`
	Seed           int            `json:"seed"`
	Department     string         `json:"department"`
	Requestable    int            `json:"requestable_chunks"`
	Steps          int            `json:"steps"`
	Distinct       []int          `json:"distinct_after_step"`
	FinalDistinct  int            `json:"final_distinct"`
	LeakFraction   float64        `json:"leak_fraction"`
	FirstRefusal   int            `json:"first_refusal_step,omitempty"`
	RefusalCode    string         `json:"refusal_code,omitempty"`
	StopCode       string         `json:"stop_code,omitempty"`
	Refusals       map[string]int `json:"refusals,omitempty"`
	ChargedRelease int64          `json:"charged_release"`
	ChargedInflu   int64          `json:"charged_influence"`
	ChargedOutcome int64          `json:"charged_outcome"`
	Seconds        float64        `json:"seconds"`
}

// EKB1Legit matches the pilot report's legitimate-task entries.
type EKB1Legit struct {
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

// EKB1Deployment matches the pilot report's deployments_detail entries.
type EKB1Deployment struct {
	Deployment   int              `json:"deployment"`
	Trajectories []EKB1Trajectory `json:"trajectories"`
	Legit        []EKB1Legit      `json:"legitimate_tasks"`
}

// RunEKB1 runs the pre-registered seeds, strategies and arms, then the
// legitimate tasks, under the profile's kb-pilot budget.
func RunEKB1(ctx context.Context, d *Deployment, embedder kbpublish.Embedder, querysetDir string, seeds, maxSteps int) (EKB1Deployment, error) {
	const kMax, legitK = 20, 5
	clearance := []string{"public", "internal"}
	departmentBySeed := []string{"sales", "finance", "hr"}
	queries, _, err := LoadQueryset(querysetDir)
	if err != nil {
		return EKB1Deployment{}, err
	}
	cache := &vectorCache{embedder: embedder, literals: map[string]string{}}
	report := EKB1Deployment{Deployment: d.Index}
	product := d.Product
	newTask := func(objective, department string) (string, error) {
		taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: objective,
			Products: []string{product.Products.Chunks, product.Products.Documents, product.Products.Values},
			Columns: func() map[string][]string {
				columns := d.chunkColumns()
				columns[product.Products.Values] = []string{"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"}
				return columns
			}(),
			Scope: map[string]any{"kb_department": []string{department}, "classification": clearance}})
		return taskID, err
	}
	search := func(taskID, requestID, text string, k int, filters []map[string]any) (Retrieval, error) {
		arguments := map[string]any{"task_id": taskID, "request_id": requestID, "knowledge_product": product.Name, "query": map[string]any{"text": text}, "k": k}
		if len(filters) > 0 {
			arguments["filters"] = filters
		}
		return d.Retrieve(ctx, d.Alice, "search", arguments)
	}
	add := func(r Retrieval, release, influence, outcome *int64) {
		*release += r.Charge.ChargedRelease
		*influence += r.Charge.ChargedInfluence
		*outcome += r.Charge.ChargedOutcome
	}
	for seedIndex := 0; seedIndex < seeds; seedIndex++ {
		seed := seedIndex + 1
		department := departmentBySeed[seedIndex%len(departmentBySeed)]
		var total int
		if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal')`,
			product.Collection, department).Scan(&total); err != nil {
			return report, err
		}
		texts := vocabulary(department)
		rng := rand.New(rand.NewPCG(uint64(seed), uint64(d.Index)))
		rng.Shuffle(len(texts), func(i, j int) { texts[i], texts[j] = texts[j], texts[i] })
		parts, err := d.partitions(ctx, department)
		if err != nil {
			return report, err
		}
		rng.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
		generic := departmentZH[department] + "制度 合同 审批 报告"
		if err := cache.embed(append(append([]string(nil), texts...), generic)); err != nil {
			return report, err
		}
		for _, strategy := range []string{"S1", "S2", "S3"} {
			for _, arm := range []string{"acl", "factgate"} {
				trajStarted := time.Now()
				traj := EKB1Trajectory{Arm: arm, Strategy: strategy, Seed: seed, Department: department, Requestable: total}
				seen := map[string]struct{}{}
				var taskID string
				if arm == "factgate" {
					if taskID, err = newTask(fmt.Sprintf("E-KB1 d%d seed%d %s", d.Index, seed, strategy), department); err != nil {
						return report, fmt.Errorf("provision: %w", err)
					}
				}
				var pendingDocs []string
				fetched := map[string]struct{}{}
				step, queryIndex, partIndex := 0, 0, 0
				for step < maxSteps {
					step++
					var ids []string
					var stepErr error
					requestID := fmt.Sprintf("%s-step%d", taskID, step)
					switch {
					case strategy == "S3" && len(pendingDocs) > 0:
						doc := pendingDocs[0]
						pendingDocs = pendingDocs[1:]
						fields := strings.Split(doc, "#")
						version, _ := strconv.Atoi(fields[1])
						if arm == "acl" {
							ids, stepErr = d.identities(ctx, `SELECT doc_id, version, chunk_index FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') AND doc_id = $3 AND version = $4 ORDER BY chunk_index`,
								product.Collection, department, fields[0], version)
							if stepErr != nil {
								return report, stepErr
							}
						} else {
							refs := make([]map[string]any, 0, 10)
							for i := 0; i < 10; i++ {
								refs = append(refs, map[string]any{"doc_id": fields[0], "version": version, "chunk_index": i})
							}
							var result Retrieval
							result, stepErr = d.Retrieve(ctx, d.Alice, "fetch", map[string]any{"task_id": taskID, "request_id": requestID, "knowledge_product": product.Name, "chunks": refs})
							if stepErr == nil {
								for _, row := range result.Rows {
									ids = append(ids, hitIDOf(row))
								}
								add(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
							}
						}
					case strategy == "S2":
						part := parts[partIndex%len(parts)]
						text := generic
						if pass := partIndex / len(parts); pass > 0 {
							text = texts[(pass-1)%len(texts)]
						}
						partIndex++
						if arm == "acl" {
							if ids, stepErr = d.directSearch(ctx, department, cache.literals[text], kMax, part.role, part.from, part.to); stepErr != nil {
								return report, stepErr
							}
						} else {
							filters := []map[string]any{{"attribute": "job_role", "op": "=", "value": part.role}}
							if part.hasDates {
								filters = append(filters, map[string]any{"attribute": "valid_until", "op": ">=", "value": part.from}, map[string]any{"attribute": "valid_until", "op": "<", "value": part.to})
							}
							var result Retrieval
							result, stepErr = search(taskID, requestID, text, kMax, filters)
							if stepErr == nil {
								for _, row := range result.Rows {
									ids = append(ids, hitIDOf(row))
								}
								add(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
							}
						}
					default:
						text := texts[queryIndex%len(texts)]
						queryIndex++
						if arm == "acl" {
							if ids, stepErr = d.directSearch(ctx, department, cache.literals[text], kMax, "", "", ""); stepErr != nil {
								return report, stepErr
							}
						} else {
							var result Retrieval
							result, stepErr = search(taskID, requestID, text, kMax, nil)
							if stepErr == nil {
								for _, row := range result.Rows {
									ids = append(ids, hitIDOf(row))
								}
								add(result, &traj.ChargedRelease, &traj.ChargedInflu, &traj.ChargedOutcome)
							}
						}
					}
					if stepErr != nil {
						code, stop := stopCode(stepErr)
						if code == "ERROR" {
							return report, fmt.Errorf("%s %s step %d: %w", strategy, arm, step, stepErr)
						}
						if traj.FirstRefusal == 0 {
							traj.FirstRefusal, traj.RefusalCode = step, code
						}
						if traj.Refusals == nil {
							traj.Refusals = map[string]int{}
						}
						traj.Refusals[code]++
						if stop {
							traj.StopCode = code
							traj.Distinct = append(traj.Distinct, len(seen))
							break
						}
					}
					for _, id := range ids {
						seen[id] = struct{}{}
						if strategy == "S3" {
							fields := strings.Split(id, "#")
							doc := fields[0] + "#" + fields[1]
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
			}
		}
		// Legitimate tasks under the same budget (amendment 3).
		taskID, err := newTask(fmt.Sprintf("E-KB1 d%d seed%d legitimate", d.Index, seed), department)
		if err != nil {
			return report, fmt.Errorf("provision legitimate: %w", err)
		}
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
		lg := EKB1Legit{Seed: seed, Department: department, Questions: len(verbatim)}
		for i, q := range verbatim {
			completed := false
			for attempt, text := range []string{q.Text, paraphrase[i].Text} {
				if text == "" {
					continue
				}
				result, err := search(taskID, fmt.Sprintf("%s-q%d-%d", taskID, i, attempt), text, legitK, nil)
				if err != nil {
					if code, _ := stopCode(err); code == "ERROR" {
						return report, err
					}
					lg.Refusals++
					break
				}
				var influence int64
				add(result, &lg.ChargedRelease, &influence, &lg.ChargedOutcome)
				for _, row := range result.Rows {
					if fmt.Sprint(row[0]) == q.DocID {
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
		if err := d.Business.QueryRow(ctx, `SELECT coalesce(sum(value_numeric),0)::text FROM kb.extracted_values WHERE collection = $1 AND field = 'amount' AND value_type = 'numeric' AND doc_id IN (SELECT doc_id FROM kb.documents WHERE collection = $1 AND department = $2 AND classification IN ('public','internal'))`,
			product.Collection, department).Scan(&directSum); err == nil {
			result, err := d.Retrieve(ctx, d.Alice, "aggregate", map[string]any{"task_id": taskID, "request_id": taskID + "-agg", "knowledge_product": product.Name, "field": "amount", "aggregate": "sum"})
			if err == nil && len(result.Rows) == 1 && len(result.Rows[0]) > 0 {
				got, _ := strconv.ParseFloat(fmt.Sprint(result.Rows[0][len(result.Rows[0])-1]), 64)
				want, _ := strconv.ParseFloat(directSum, 64)
				lg.AggregateCorrect = math.Abs(got-want) <= 1e-6*math.Max(1, math.Abs(want))
				var influence int64
				add(result, &lg.ChargedRelease, &influence, &lg.ChargedOutcome)
			} else if err != nil {
				if code, _ := stopCode(err); code == "ERROR" {
					return report, err
				}
				lg.Refusals++
			}
		}
		report.Legit = append(report.Legit, lg)
	}
	return report, nil
}

// ---- E-KB2 (b): the misapproval arm ----

// EKB2bArm matches the pilot report's arm entries.
type EKB2bArm struct {
	Deployment      int    `json:"deployment"`
	Department      string `json:"department"`
	Requestable     int    `json:"requestable_chunks"`
	Steps           int    `json:"steps"`
	Leaked          int    `json:"leaked_chunks"`
	LeakBound       int    `json:"leak_bound_chunks"`
	FirstRefusal    int    `json:"first_refusal_step"`
	RefusalCode     string `json:"refusal_code"`
	AfterExpiryCode string `json:"after_expiry_code"`
	AfterExpiryNew  int    `json:"after_expiry_new_chunks"`
	ChargedRelease  int64  `json:"charged_release"`
	// ExpiryWaitSeconds is the wall-clock wait from activation to the
	// after-expiry request (the pilot advanced a test clock instead).
	ExpiryWaitSeconds float64 `json:"expiry_wait_seconds"`
}

// RunEKB2b grants Alice's own-department request up to confidential with the
// approver narrowing the TTL to 120 s, runs E-KB1's S2 then S1 texts at k=20
// until the first refusal, then waits past the TTL and asks once more.
func RunEKB2b(ctx context.Context, d *Deployment) (EKB2bArm, error) {
	const kMax, budgetRelease = 20, 800
	ttl := 2 * time.Minute
	departments := []string{"sales", "finance", "hr"}
	department := departments[(d.Index-1)%len(departments)]
	product := d.Product
	granted := []string{"public", "internal", "confidential"}
	taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: "E-KB2(b) misapproved grant", Products: []string{product.Products.Chunks, product.Products.Documents},
		Columns: d.chunkColumns(), Scope: map[string]any{"kb_department": []string{department}, "classification": granted}, TTL: ttl})
	if err != nil {
		return EKB2bArm{}, fmt.Errorf("provision: %w", err)
	}
	activated := time.Now()
	var requestable int
	if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal','confidential')`,
		product.Collection, department).Scan(&requestable); err != nil {
		return EKB2bArm{}, err
	}
	arm := EKB2bArm{Deployment: d.Index, Department: department, Requestable: requestable, LeakBound: budgetRelease / 4}
	if requestable < arm.LeakBound {
		arm.LeakBound = requestable
	}
	generic := departmentZH[department] + "制度 合同 审批 报告"
	var minDate, maxDate *time.Time
	if err := d.Business.QueryRow(ctx, `SELECT min(valid_until), max(valid_until) FROM kb.chunks WHERE collection = $1 AND department = $2 AND valid_until IS NOT NULL`,
		product.Collection, department).Scan(&minDate, &maxDate); err != nil {
		return arm, err
	}
	type part struct{ role, from, to string }
	var parts []part
	for _, role := range []string{"staff", "manager", "director", "executive", "hr"} {
		span := maxDate.Sub(*minDate) / 6
		for b := 0; b < 6; b++ {
			from, to := minDate.Add(time.Duration(b)*span), minDate.Add(time.Duration(b+1)*span)
			if b == 5 {
				to = maxDate.AddDate(0, 0, 1)
			}
			parts = append(parts, part{role, from.Format("2006-01-02"), to.Format("2006-01-02")})
		}
	}
	words := []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更"}
	seen := map[string]struct{}{}
	step := 0
	for step < 400 {
		step++
		arguments := map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-step%d", taskID, step), "knowledge_product": product.Name, "k": kMax}
		if step <= len(parts) {
			p := parts[step-1]
			arguments["query"] = map[string]any{"text": generic}
			arguments["filters"] = []map[string]any{{"attribute": "job_role", "op": "=", "value": p.role},
				{"attribute": "valid_until", "op": ">=", "value": p.from}, {"attribute": "valid_until", "op": "<", "value": p.to}}
		} else {
			word := words[(step-len(parts)-1)%len(words)]
			template := []string{"%s%s制度", "%s审批流程", "关于%s的规定", "%s报告"}[((step-len(parts)-1)/len(words))%4]
			text := fmt.Sprintf(template, word)
			if strings.Count(template, "%s") == 2 {
				text = fmt.Sprintf(template, departmentZH[department], word)
			}
			arguments["query"] = map[string]any{"text": text}
		}
		result, err := d.Retrieve(ctx, d.Alice, "search", arguments)
		if err != nil {
			code, _ := stopCode(err)
			if code == "ERROR" {
				return arm, fmt.Errorf("step %d: %w", step, err)
			}
			arm.FirstRefusal, arm.RefusalCode = step, code
			break
		}
		for _, row := range result.Rows {
			seen[hitIDOf(row)] = struct{}{}
		}
		arm.ChargedRelease += result.Charge.ChargedRelease
	}
	arm.Steps, arm.Leaked = step, len(seen)
	// Expiry: wait past the TTL on the wall clock; a further request must be
	// refused and add nothing.
	if wait := time.Until(activated.Add(ttl + 2*time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	arm.ExpiryWaitSeconds = time.Since(activated).Seconds()
	before := len(seen)
	result, err := d.Retrieve(ctx, d.Alice, "search", map[string]any{"task_id": taskID, "request_id": taskID + "-after-expiry",
		"knowledge_product": product.Name, "query": map[string]any{"text": generic}, "k": kMax})
	if err != nil {
		code, _ := stopCode(err)
		arm.AfterExpiryCode = code
	} else {
		for _, row := range result.Rows {
			seen[hitIDOf(row)] = struct{}{}
		}
	}
	arm.AfterExpiryNew = len(seen) - before
	return arm, nil
}
