package kbformal

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// ---- Keyword experiments E-KB6..E-KB8 (docs/kb_experiment_design.md, 关键词检索实验) ----
//
// Every gateway call is a search with query.keywords over HTTP through
// Retrieve (receipt-verified, delivered texts digest-checked). The direct arm
// is the statement the gateway prepares, written against kb.chunks by hand:
// the same scope filter, the same LIKE (wildcards escaped with LIKE's default
// escape character), the same matched-character rank and the same total order.

// keywordLikePattern makes a keyword literal under LIKE's default escape
// character and wraps it for substring containment.
func keywordLikePattern(keyword string) string {
	escaped := strings.ReplaceAll(keyword, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, "%", `\%`)
	escaped = strings.ReplaceAll(escaped, "_", `\_`)
	return "%" + escaped + "%"
}

const directKeywordSearchSQL = `SELECT doc_id, version, chunk_index FROM kb.chunks
	WHERE collection = $1 AND department = $2 AND classification = ANY($3) AND text LIKE $4
	ORDER BY (length(replace(text, $5, '')) - length(text)), doc_id, version, chunk_index LIMIT $6`

func (d *Deployment) directKeywordSearch(ctx context.Context, department string, classifications []string, keyword string, k int) ([]string, error) {
	return d.identities(ctx, directKeywordSearchSQL, d.Product.Collection, department, classifications, keywordLikePattern(keyword), keyword, k)
}

func (d *Deployment) keywordSearch(ctx context.Context, taskID, requestID, keyword string, k int) (Retrieval, error) {
	return d.Retrieve(ctx, d.Alice, "search", map[string]any{"task_id": taskID, "request_id": requestID,
		"knowledge_product": d.Product.Name, "query": map[string]any{"keywords": []string{keyword}}, "k": k})
}

var allClassifications = []string{"public", "internal", "confidential", "leadership"}

// keywordDepartments are the departments of the anchor keywords, sorted; a
// frequent or absent keyword (no target) is asked in departments[i mod n].
func keywordDepartments(keywords []KeywordQuery) []string {
	seen := map[string]struct{}{}
	for _, keyword := range keywords {
		if keyword.Department != "" {
			seen[keyword.Department] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for department := range seen {
		out = append(out, department)
	}
	sort.Strings(out)
	return out
}

// ---- E-KB6: keyword hit sets against direct LIKE ----

type EKB6Observation struct {
	KeywordID  string   `json:"keyword_id"`
	Class      string   `json:"class"`
	Keyword    string   `json:"keyword"`
	Department string   `json:"department"`
	K          int      `json:"k"`
	Gateway    []string `json:"gateway"`
	Direct     []string `json:"direct"`
	Identical  bool     `json:"identical"`
	SameOrder  bool     `json:"same_order"`
	// TargetHit is, for an anchor keyword, whether a chunk of its target
	// document is among the gateway hits.
	TargetHit bool   `json:"target_hit,omitempty"`
	Error     string `json:"error,omitempty"`
}

type EKB6Deployment struct {
	Deployment   int               `json:"deployment"`
	Keywords     int               `json:"keywords"`
	Observations []EKB6Observation `json:"observations"`
	Seconds      float64           `json:"seconds"`
}

// RunEKB6 asks every frozen keyword at k = 5, 10, 20 through the gateway and
// directly, under one large-budget task per department with every
// classification in scope.
func RunEKB6(ctx context.Context, d *Deployment, keywordsDir string) (EKB6Deployment, error) {
	started := time.Now()
	keywords, _, err := LoadKeywordset(keywordsDir)
	if err != nil {
		return EKB6Deployment{}, err
	}
	departments := keywordDepartments(keywords)
	if len(departments) == 0 {
		return EKB6Deployment{}, fmt.Errorf("the keyword set names no department")
	}
	report := EKB6Deployment{Deployment: d.Index, Keywords: len(keywords)}
	tasks := map[string]string{}
	product := d.Product
	unassigned := 0
	for _, keyword := range keywords {
		department := keyword.Department
		if department == "" {
			department = departments[unassigned%len(departments)]
			unassigned++
		}
		taskID, ok := tasks[department]
		if !ok {
			taskID, _, err = d.Provisioner.Provision(ctx, TaskRequest{Objective: "E-KB6 keyword hit sets " + department,
				Products: []string{product.Products.Chunks, product.Products.Documents}, Columns: d.chunkColumns(),
				Scope: map[string]any{"kb_department": []string{department}, "classification": allClassifications}})
			if err != nil {
				return report, fmt.Errorf("provision %s: %w", department, err)
			}
			tasks[department] = taskID
		}
		for _, k := range []int{5, 10, 20} {
			observation := EKB6Observation{KeywordID: keyword.KeywordID, Class: keyword.Class, Keyword: keyword.Keyword, Department: department, K: k}
			result, err := d.keywordSearch(ctx, taskID, fmt.Sprintf("%s-k%d", keyword.KeywordID, k), keyword.Keyword, k)
			if err != nil {
				if code, _ := stopCode(err); code == "ERROR" {
					return report, fmt.Errorf("%s k=%d: %w", keyword.KeywordID, k, err)
				}
				observation.Error = err.Error()
				report.Observations = append(report.Observations, observation)
				continue
			}
			for _, row := range result.Rows {
				id := hitIDOf(row)
				observation.Gateway = append(observation.Gateway, id)
				if keyword.Class == KeywordClassAnchor && strings.HasPrefix(id, fmt.Sprintf("%s#%d#", keyword.DocID, keyword.Version)) {
					observation.TargetHit = true
				}
			}
			direct, err := d.directKeywordSearch(ctx, department, allClassifications, keyword.Keyword, k)
			if err != nil {
				return report, err
			}
			observation.Direct = direct
			observation.SameOrder = strings.Join(observation.Gateway, ",") == strings.Join(observation.Direct, ",")
			observation.Identical = sameMembers(observation.Gateway, observation.Direct)
			report.Observations = append(report.Observations, observation)
		}
	}
	report.Seconds = time.Since(started).Seconds()
	return report, nil
}

// ---- E-KB7: keyword sweep extraction under the small budget ----

type EKB7Legit struct {
	Seed             int     `json:"seed"`
	Department       string  `json:"department"`
	Questions        int     `json:"questions"`
	Completed        int     `json:"completed"`
	CompletionRate   float64 `json:"completion_rate"`
	Visible          int     `json:"visible_targets"`
	CompletedVisible int     `json:"completed_visible"`
	Refusals         int     `json:"refusals"`
	ChargedRelease   int64   `json:"charged_release"`
	ChargedOutcome   int64   `json:"charged_outcome"`
}

type EKB7Deployment struct {
	Deployment   int              `json:"deployment"`
	Trajectories []EKB1Trajectory `json:"trajectories"`
	Legit        []EKB7Legit      `json:"legitimate_tasks"`
	Seconds      float64          `json:"seconds"`
}

// sweepOrder is the S4 order over n frequent keywords: a start and a stride
// coprime with n, both drawn from the seed's generator.
func sweepOrder(rng *rand.Rand, n, steps int) []int {
	var strides []int
	for stride := 1; stride < n; stride++ {
		if gcd(stride, n) == 1 {
			strides = append(strides, stride)
		}
	}
	start, stride := rng.IntN(n), strides[rng.IntN(len(strides))]
	order := make([]int, steps)
	for i := range order {
		order[i] = (start + i*stride) % n
	}
	return order
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// RunEKB7 runs strategy S4 in the ACL and FactGate arms for each seed, then
// the legitimate anchor questions under the same budget.
func RunEKB7(ctx context.Context, d *Deployment, keywordsDir string, seeds, maxSteps int) (EKB7Deployment, error) {
	const kMax, legitK = 20, 5
	started := time.Now()
	clearance := []string{"public", "internal"}
	departmentBySeed := []string{"sales", "finance", "hr"}
	keywords, _, err := LoadKeywordset(keywordsDir)
	if err != nil {
		return EKB7Deployment{}, err
	}
	var frequent []string
	for _, keyword := range keywords {
		if keyword.Class == KeywordClassFrequent {
			frequent = append(frequent, keyword.Keyword)
		}
	}
	if len(frequent) == 0 {
		return EKB7Deployment{}, fmt.Errorf("the keyword set has no frequent keywords")
	}
	product := d.Product
	report := EKB7Deployment{Deployment: d.Index}
	newTask := func(objective, department string) (string, error) {
		taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: objective,
			Products: []string{product.Products.Chunks, product.Products.Documents}, Columns: d.chunkColumns(),
			Scope: map[string]any{"kb_department": []string{department}, "classification": clearance}})
		return taskID, err
	}
	for seedIndex := 0; seedIndex < seeds; seedIndex++ {
		seed := seedIndex + 1
		department := departmentBySeed[seedIndex%len(departmentBySeed)]
		var total int
		if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND department = $2 AND classification IN ('public','internal')`,
			product.Collection, department).Scan(&total); err != nil {
			return report, err
		}
		order := sweepOrder(rand.New(rand.NewPCG(uint64(seed), uint64(d.Index))), len(frequent), maxSteps)
		for _, arm := range []string{"acl", "factgate"} {
			trajStarted := time.Now()
			traj := EKB1Trajectory{Arm: arm, Strategy: "S4", Seed: seed, Department: department, Requestable: total}
			seen := map[string]struct{}{}
			var taskID string
			if arm == "factgate" {
				if taskID, err = newTask(fmt.Sprintf("E-KB7 d%d seed%d S4", d.Index, seed), department); err != nil {
					return report, fmt.Errorf("provision: %w", err)
				}
			}
			step := 0
			for step < maxSteps {
				keyword := frequent[order[step]]
				step++
				var ids []string
				if arm == "acl" {
					if ids, err = d.directKeywordSearch(ctx, department, clearance, keyword, kMax); err != nil {
						return report, err
					}
				} else {
					result, stepErr := d.keywordSearch(ctx, taskID, fmt.Sprintf("%s-step%d", taskID, step), keyword, kMax)
					if stepErr != nil {
						code, stop := stopCode(stepErr)
						if code == "ERROR" {
							return report, fmt.Errorf("S4 step %d: %w", step, stepErr)
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
					} else {
						for _, row := range result.Rows {
							ids = append(ids, hitIDOf(row))
						}
						traj.ChargedRelease += result.Charge.ChargedRelease
						traj.ChargedInflu += result.Charge.ChargedInfluence
						traj.ChargedOutcome += result.Charge.ChargedOutcome
					}
				}
				for _, id := range ids {
					seen[id] = struct{}{}
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
		// Legitimate tasks: this department's first ten anchor keywords.
		taskID, err := newTask(fmt.Sprintf("E-KB7 d%d seed%d legitimate", d.Index, seed), department)
		if err != nil {
			return report, fmt.Errorf("provision legitimate: %w", err)
		}
		legit := EKB7Legit{Seed: seed, Department: department}
		for _, keyword := range keywords {
			if keyword.Class != KeywordClassAnchor || keyword.Department != department || legit.Questions == 10 {
				continue
			}
			legit.Questions++
			var visible int
			if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1 AND doc_id = $2 AND version = $3 AND classification IN ('public','internal')`,
				product.Collection, keyword.DocID, keyword.Version).Scan(&visible); err != nil {
				return report, err
			}
			if visible > 0 {
				legit.Visible++
			}
			result, err := d.keywordSearch(ctx, taskID, fmt.Sprintf("%s-legit-%s", taskID, keyword.KeywordID), keyword.Keyword, legitK)
			if err != nil {
				if code, _ := stopCode(err); code == "ERROR" {
					return report, fmt.Errorf("legitimate %s: %w", keyword.KeywordID, err)
				}
				legit.Refusals++
				continue
			}
			legit.ChargedRelease += result.Charge.ChargedRelease
			legit.ChargedOutcome += result.Charge.ChargedOutcome
			for _, row := range result.Rows {
				if fmt.Sprint(row[0]) == keyword.DocID && intOf(row[1]) == keyword.Version {
					legit.Completed++
					if visible > 0 {
						legit.CompletedVisible++
					}
					break
				}
			}
		}
		if legit.Questions > 0 {
			legit.CompletionRate = float64(legit.Completed) / float64(legit.Questions)
		}
		report.Legit = append(report.Legit, legit)
	}
	report.Seconds = time.Since(started).Seconds()
	return report, nil
}

// ---- E-KB8: keyword search overhead ----

// RunEKB8 runs the pre-registered rounds of one tier: per round a direct and a
// gateway keyword search (k alternating 5 and 20; every tenth round from the
// tenth on replays the round ten before), in the sales department with
// {public, internal}, as E-KB3 does for vector search.
func RunEKB8(ctx context.Context, d *Deployment, keywordsDir, tier string, rounds int) (EKB3Deployment, error) {
	started := time.Now()
	keywords, _, err := LoadKeywordset(keywordsDir)
	if err != nil {
		return EKB3Deployment{}, err
	}
	var pool []string
	for _, keyword := range keywords {
		if keyword.Class == KeywordClassAnchor || keyword.Class == KeywordClassFrequent {
			pool = append(pool, keyword.Keyword)
		}
	}
	product := d.Product
	report := EKB3Deployment{Deployment: d.Index, Tier: tier, Department: "sales", Rounds: rounds}
	if err := d.Business.QueryRow(ctx, `SELECT count(*) FROM kb.chunks WHERE collection = $1`, product.Collection).Scan(&report.Chunks); err != nil {
		return report, err
	}
	clearance := []string{"public", "internal"}
	taskID, _, err := d.Provisioner.Provision(ctx, TaskRequest{Objective: "E-KB8 keyword overhead " + tier,
		Products: []string{product.Products.Chunks, product.Products.Documents}, Columns: d.chunkColumns(),
		Scope: map[string]any{"kb_department": []string{report.Department}, "classification": clearance}})
	if err != nil {
		return report, fmt.Errorf("provision: %w", err)
	}
	report.Tasks = 1
	drain := func(keyword string, k int) (int, error) {
		rows, err := d.Business.Query(ctx, `SELECT doc_id, version, chunk_index, text FROM kb.chunks
			WHERE collection = $1 AND department = $2 AND classification = ANY($3) AND text LIKE $4
			ORDER BY (length(replace(text, $5, '')) - length(text)), doc_id, version, chunk_index LIMIT $6`,
			product.Collection, report.Department, clearance, keywordLikePattern(keyword), keyword, k)
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
	rng := rand.New(rand.NewPCG(uint64(d.Index), 20261018))
	order := rng.Perm(len(pool))
	seenForms := map[string]bool{}
	for round := 0; round < rounds; round++ {
		k := 5
		if round%2 == 1 {
			k = 20
		}
		keyword := pool[order[round%len(order)]]
		if round%10 == 9 && round >= 10 {
			keyword = pool[order[(round-10)%len(order)]]
		}
		formKey := fmt.Sprintf("keyword|%s|%d", keyword, k)
		replay := seenForms[formKey]
		seenForms[formKey] = true
		t0 := time.Now()
		n, err := drain(keyword, k)
		if err != nil {
			return report, fmt.Errorf("round %d direct keyword search: %w", round, err)
		}
		report.Samples = append(report.Samples, EKB3Sample{Op: "keyword", Arm: "direct", K: k, Replay: replay, ClientMS: elapsedMS(t0), Rows: n})
		searched, err := d.keywordSearch(ctx, taskID, fmt.Sprintf("%s-keyword-%d", taskID, round), keyword, k)
		sample := EKB3Sample{Op: "keyword", Arm: "factgate", K: k, Replay: replay}
		if err := gatewaySample(&sample, searched, err); err != nil {
			return report, fmt.Errorf("round %d keyword search: %w", round, err)
		}
		report.Samples = append(report.Samples, sample)
	}
	report.Summary = SummarizeEKB3(report.Samples)
	report.Seconds = time.Since(started).Seconds()
	return report, nil
}
