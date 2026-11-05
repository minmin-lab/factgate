package kbformal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/kbpublish"
	"taskbound.local/agent-data-gateway/internal/retrieval"
)

// ---- E-KB3: overhead of search / fetch / aggregate against direct execution ----

// The port follows the pilot runner (internal/gateway/kb_experiment_ekb3_test.go)
// step for step: the same 240 query texts, seed, k alternation, replay rule,
// direct-arm SQL, aggregate forms and summary cells. The task comes from the
// real request/OA path and the gateway arm runs over HTTP through Retrieve, so
// every response is receipt-verified and every delivered text digest-checked.

// EKB3Sample matches the pilot report's sample entries. ClientMS keeps the
// pilot meaning, the client time until it holds every row: the drained query
// for the direct arm, the tool response (rows inline) for the gateway arm.
// DeliveredMS, gateway arm only, adds receipt verification, the artifact
// delivery and the row checks.
type EKB3Sample struct {
	Op          string             `json:"op"`
	Arm         string             `json:"arm"`
	K           int                `json:"k,omitempty"`
	Replay      bool               `json:"replay"`
	ClientMS    float64            `json:"client_ms"`
	DeliveredMS float64            `json:"delivered_ms,omitempty"`
	Rows        int                `json:"rows"`
	Components  map[string]float64 `json:"components_ms,omitempty"`
	DatabaseMS  float64            `json:"database_ms,omitempty"`
	ChargedR    int64              `json:"charged_release,omitempty"`
	ChargedD    int64              `json:"charged_influence,omitempty"`
	ChargedO    int64              `json:"charged_outcome,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// EKB3Cell matches the pilot report's summary cells over ClientMS, with the
// gateway arm's verified-delivery percentiles added.
type EKB3Cell struct {
	N            int                `json:"n"`
	P50          float64            `json:"p50_ms"`
	P95          float64            `json:"p95_ms"`
	Mean         float64            `json:"mean_ms"`
	DeliveredP50 float64            `json:"delivered_p50_ms,omitempty"`
	DeliveredP95 float64            `json:"delivered_p95_ms,omitempty"`
	Components   map[string]float64 `json:"components_p50_ms,omitempty"`
	Errors       int                `json:"errors"`
}

// EKB3Deployment is one deployment's run of one tier.
type EKB3Deployment struct {
	Deployment int                 `json:"deployment"`
	Tier       string              `json:"tier"`
	Chunks     int                 `json:"chunks"`
	Department string              `json:"department"`
	Rounds     int                 `json:"rounds"`
	Tasks      int                 `json:"tasks"`
	Summary    map[string]EKB3Cell `json:"summary"`
	Samples    []EKB3Sample        `json:"samples"`
	Seconds    float64             `json:"seconds"`
}

func floatOf(value any) float64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case float64:
		return typed
	case int64:
		return float64(typed)
	}
	return 0
}

func intOf(value any) int {
	switch typed := value.(type) {
	case int64:
		return int(typed)
	case int32:
		return int(typed)
	case int:
		return typed
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	}
	parsed, _ := strconv.Atoi(fmt.Sprint(value))
	return parsed
}

// gatewaySample fills a gateway-arm sample from a verified retrieval, or
// records a tool refusal as the pilot did. Any other failure (receipt, digest,
// delivery or transport) ends the run: 正式轮规程 §8 counts it as a failed
// deployment, not as a sample.
func gatewaySample(sample *EKB3Sample, result Retrieval, err error) error {
	if err != nil {
		var toolErr *ToolError
		if !errors.As(err, &toolErr) {
			return err
		}
		sample.Error = err.Error()
		return nil
	}
	sample.ClientMS, sample.DeliveredMS, sample.Rows = result.CallMS, result.DeliveredMS, len(result.Rows)
	if raw, ok := result.Response["component_ms"].(map[string]any); ok {
		sample.Components = map[string]float64{}
		for name, value := range raw {
			sample.Components[name] = floatOf(value)
		}
	}
	sample.DatabaseMS = floatOf(result.Response["database_ms"])
	sample.ChargedR, sample.ChargedD, sample.ChargedO = result.Charge.ChargedRelease, result.Charge.ChargedInfluence, result.Charge.ChargedOutcome
	return nil
}

// RunEKB3 runs the pre-registered 200 rounds (rounds) of one tier on one
// deployment: per round a direct and a gateway search (k alternating 5 and 20,
// every tenth round from the tenth on replaying the round ten before), a direct
// and a gateway fetch of the gateway's hits, and a direct and a gateway
// aggregate over 24 distinct forms.
func RunEKB3(ctx context.Context, d *Deployment, embedder kbpublish.Embedder, tier string, rounds int) (EKB3Deployment, error) {
	started := time.Now()
	product := d.Product
	if err := embedder.Check(kbpublish.Embedding{Model: product.Embedding.Model, Dimension: product.Embedding.Dimension, WeightsSHA256: product.Embedding.WeightsSHA256}); err != nil {
		return EKB3Deployment{}, fmt.Errorf("embedding service does not match the knowledge product: %w", err)
	}
	report := EKB3Deployment{Deployment: d.Index, Tier: tier, Department: "sales", Rounds: rounds}
	if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1`, product.Collection).Scan(&report.Chunks); err != nil {
		return report, err
	}
	department := report.Department
	words := []string{"报销", "采购", "培训", "考核", "预算", "安全", "流程", "权限", "供应商", "客户", "项目", "绩效", "招聘", "薪酬", "差旅", "合规", "风险", "数据", "系统", "变更",
		"季度", "年度", "评估", "审计", "合同", "审批", "报告", "制度", "定价", "折扣", "渠道", "续约", "招标", "付款", "发票", "资产", "库存", "事故", "值班", "上线"}
	templates := []string{"销售部%s制度", "%s审批流程", "关于%s的规定", "%s报告", "%s合同金额", "销售部%s管理办法"}
	var texts []string
	for _, template := range templates {
		for _, word := range words {
			texts = append(texts, fmt.Sprintf(template, word))
		}
	}
	vectorsF := map[string][]float64{}
	vectorsL := map[string]string{}
	for start := 0; start < len(texts); start += 32 {
		end := min(start+32, len(texts))
		normalized := make([]string, 0, end-start)
		for _, text := range texts[start:end] {
			normalized = append(normalized, retrieval.NormalizeText(text))
		}
		vectors, err := embedder.Embed(normalized)
		if err != nil || len(vectors) != len(normalized) {
			return report, fmt.Errorf("embed %d..%d: %v", start, end, err)
		}
		for i, text := range texts[start:end] {
			parts := make([]string, 0, len(vectors[i]))
			vector := make([]float64, len(vectors[i]))
			for j, value := range vectors[i] {
				parts = append(parts, fmt.Sprintf("%g", value))
				vector[j] = float64(value)
			}
			vectorsF[text] = vector
			vectorsL[text] = "[" + strings.Join(parts, ",") + "]"
		}
	}
	drain := func(sql string, args ...any) (int, error) {
		rows, err := d.Business.Query(ctx, sql, args...)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		return n, rows.Err()
	}
	directSearch := func(vector string, k int) (int, error) {
		return drain(`SELECT doc_id, version, chunk_index, text FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') ORDER BY embedding <=> $3::vector, doc_id, version, chunk_index LIMIT $4`, product.Collection, department, vector, k)
	}
	directFetch := func(refs [][3]any) (int, error) {
		var clauses []string
		args := []any{product.Collection, department}
		for _, ref := range refs {
			clauses = append(clauses, fmt.Sprintf("(doc_id = $%d AND version = $%d AND chunk_index = $%d)", len(args)+1, len(args)+2, len(args)+3))
			args = append(args, fmt.Sprint(ref[0]), intOf(ref[1]), intOf(ref[2]))
		}
		return drain(`SELECT doc_id, version, chunk_index, text FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal') AND (`+strings.Join(clauses, " OR ")+`) ORDER BY doc_id, version, chunk_index`, args...)
	}
	directAggregate := func(field, function, classification string) error {
		expr := function + "(value_numeric)"
		if function == "count" {
			expr = "count(canonical_value)"
		}
		var out *string
		return d.Business.QueryRow(ctx, `SELECT `+expr+`::text FROM kb.extracted_values v JOIN kb.documents d ON d.collection = v.collection AND d.doc_id = v.doc_id AND d.version = v.version WHERE v.collection = $1 AND d.department = $2 AND d.classification = $3 AND v.field = $4 AND v.value_type = 'numeric'`, product.Collection, department, classification, field).Scan(&out)
	}
	chunks, documents, values := product.Products.Chunks, product.Products.Documents, product.Products.Values
	taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: "E-KB3 overhead " + tier, Products: []string{chunks, documents, values},
		Columns: map[string][]string{
			chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
			documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
			values:    {"doc_id", "version", "document_type", "field", "value_type", "canonical_value", "value_numeric", "value_date", "kb_department", "classification"},
		},
		Scope: map[string]any{"kb_department": []string{department}, "classification": []string{"public", "internal"}}})
	if err != nil {
		return report, fmt.Errorf("provision: %w", err)
	}
	report.Tasks = 1
	fields := []string{"amount", "approved_amount", "headline_metric"}
	functions := []string{"sum", "count", "min", "max"}
	classifications := []string{"internal", "public"}
	rng := rand.New(rand.NewPCG(uint64(d.Index), 20261016))
	order := rng.Perm(len(texts))
	seenForms := map[string]bool{}
	for round := 0; round < rounds; round++ {
		k := 5
		if round%2 == 1 {
			k = 20
		}
		text := texts[order[round%len(order)]]
		if round%10 == 9 && round >= 10 {
			text = texts[order[(round-10)%len(order)]]
		}
		formKey := fmt.Sprintf("search|%s|%d", text, k)
		replay := seenForms[formKey]
		seenForms[formKey] = true
		t0 := time.Now()
		n, err := directSearch(vectorsL[text], k)
		if err != nil {
			return report, fmt.Errorf("round %d direct search: %w", round, err)
		}
		report.Samples = append(report.Samples, EKB3Sample{Op: "search", Arm: "direct", K: k, Replay: replay, ClientMS: elapsedMS(t0), Rows: n})
		searched, err := d.Retrieve(ctx, d.Alice, "search", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-search-%d", taskID, round),
			"knowledge_product": product.Name, "query": map[string]any{"vector": vectorsF[text]}, "k": k})
		sample := EKB3Sample{Op: "search", Arm: "factgate", K: k, Replay: replay}
		if err := gatewaySample(&sample, searched, err); err != nil {
			return report, fmt.Errorf("round %d search: %w", round, err)
		}
		report.Samples = append(report.Samples, sample)
		if sample.Error == "" && len(searched.Rows) > 0 {
			hits := make([][3]any, 0, len(searched.Rows))
			refs := make([]map[string]any, 0, len(searched.Rows))
			ids := make([]string, 0, len(searched.Rows))
			for _, row := range searched.Rows {
				hits = append(hits, [3]any{row[0], row[1], row[2]})
				refs = append(refs, map[string]any{"doc_id": row[0], "version": row[1], "chunk_index": row[2]})
				ids = append(ids, hitIDOf(row))
			}
			fetchKey := "fetch|" + strings.Join(ids, ",")
			fetchReplay := seenForms[fetchKey]
			seenForms[fetchKey] = true
			t0 = time.Now()
			n, err := directFetch(hits)
			if err != nil {
				return report, fmt.Errorf("round %d direct fetch: %w", round, err)
			}
			report.Samples = append(report.Samples, EKB3Sample{Op: "fetch", Arm: "direct", K: k, Replay: fetchReplay, ClientMS: elapsedMS(t0), Rows: n})
			fetched, err := d.Retrieve(ctx, d.Alice, "fetch", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-fetch-%d", taskID, round),
				"knowledge_product": product.Name, "chunks": refs})
			fetchSample := EKB3Sample{Op: "fetch", Arm: "factgate", K: k, Replay: fetchReplay}
			if err := gatewaySample(&fetchSample, fetched, err); err != nil {
				return report, fmt.Errorf("round %d fetch: %w", round, err)
			}
			report.Samples = append(report.Samples, fetchSample)
		}
		field, function, classification := fields[round%3], functions[(round/3)%4], classifications[(round/12)%2]
		aggKey := fmt.Sprintf("aggregate|%s|%s|%s", field, function, classification)
		aggReplay := seenForms[aggKey]
		seenForms[aggKey] = true
		t0 = time.Now()
		if err := directAggregate(field, function, classification); err != nil {
			return report, fmt.Errorf("round %d direct aggregate: %w", round, err)
		}
		report.Samples = append(report.Samples, EKB3Sample{Op: "aggregate", Arm: "direct", Replay: aggReplay, ClientMS: elapsedMS(t0), Rows: 1})
		aggregated, err := d.Retrieve(ctx, d.Alice, "aggregate", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("%s-agg-%d", taskID, round),
			"knowledge_product": product.Name, "field": field, "aggregate": function,
			"filters": []map[string]any{{"attribute": "classification", "op": "=", "value": classification}}})
		aggSample := EKB3Sample{Op: "aggregate", Arm: "factgate", Replay: aggReplay}
		if err := gatewaySample(&aggSample, aggregated, err); err != nil {
			return report, fmt.Errorf("round %d aggregate: %w", round, err)
		}
		if aggSample.Error == "" {
			aggSample.Rows = 1
		}
		report.Samples = append(report.Samples, aggSample)
	}
	report.Summary = SummarizeEKB3(report.Samples)
	report.Seconds = time.Since(started).Seconds()
	return report, nil
}

// SummarizeEKB3 groups samples as the pilot report does (op/arm/k<k>/novel|replay;
// aggregate without k) and takes the same order statistics.
func SummarizeEKB3(samples []EKB3Sample) map[string]EKB3Cell {
	groups := map[string][]EKB3Sample{}
	for _, sample := range samples {
		kind := map[bool]string{false: "novel", true: "replay"}[sample.Replay]
		key := fmt.Sprintf("%s/%s/k%d/%s", sample.Op, sample.Arm, sample.K, kind)
		if sample.Op == "aggregate" {
			key = fmt.Sprintf("%s/%s/%s", sample.Op, sample.Arm, kind)
		}
		groups[key] = append(groups[key], sample)
	}
	summary := map[string]EKB3Cell{}
	for key, group := range groups {
		var latencies, delivered []float64
		components := map[string][]float64{}
		cell := EKB3Cell{N: len(group)}
		for _, sample := range group {
			if sample.Error != "" {
				cell.Errors++
				continue
			}
			latencies = append(latencies, sample.ClientMS)
			if sample.DeliveredMS > 0 {
				delivered = append(delivered, sample.DeliveredMS)
			}
			for name, value := range sample.Components {
				components[name] = append(components[name], value)
			}
		}
		if len(latencies) > 0 {
			sort.Float64s(latencies)
			var sum float64
			for _, value := range latencies {
				sum += value
			}
			cell.P50, cell.P95, cell.Mean = latencies[len(latencies)/2], latencies[(len(latencies)*95)/100], sum/float64(len(latencies))
		}
		if len(delivered) > 0 {
			sort.Float64s(delivered)
			cell.DeliveredP50, cell.DeliveredP95 = delivered[len(delivered)/2], delivered[(len(delivered)*95)/100]
		}
		if len(components) > 0 {
			cell.Components = map[string]float64{}
			for name, values := range components {
				sort.Float64s(values)
				cell.Components[name] = values[len(values)/2]
			}
		}
		summary[key] = cell
	}
	return summary
}
