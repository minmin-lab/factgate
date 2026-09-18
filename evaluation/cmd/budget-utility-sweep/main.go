// budget-utility-sweep derives, by admission arithmetic over the frozen
// corpora and their independent oracles, how benign task completion and
// adversarial extraction move with the owner recipe's budget multiplier.
// Nothing here is measured: the benign side unions the closed-form
// Dependency sets statement by statement under the set-ledger rule (a
// refused statement adds nothing), and the adversary side replays the
// data-independent per-step charges recorded in the adversary corpus. The
// executed pilots (benign 1x/2x/4x, adversary tightened/owner/loosened)
// validate the arithmetic at their points.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5adversary"
	"taskbound.local/agent-data-gateway/evaluation/finalv5benign"
)

type budget struct{ R, D, O, Q int64 }

func scale(b budget, m float64) budget {
	f := func(v int64) int64 { return int64(math.Ceil(float64(v) * m)) }
	return budget{f(b.R), f(b.D), f(b.O), f(b.Q)}
}

type benignPoint struct {
	Multiplier     float64 `json:"multiplier"`
	Budget         budget  `json:"budget"`
	Authorized     int     `json:"authorized"`
	Accepted       int     `json:"accepted"`
	BudgetRefusals int     `json:"budget_refusals"`
	PolicyRefusals int     `json:"policy_refusals"`
	CompletionPct  float64 `json:"completion_pct"`
	LedgerD        int     `json:"ledger_dependency"`
	FirstRefusal   string  `json:"first_budget_refusal,omitempty"`
	BindingDim     string  `json:"binding_dimension,omitempty"`
}

type adversaryPoint struct {
	Multiplier      float64 `json:"multiplier"`
	Budget          budget  `json:"budget"`
	BisectAccepted  int     `json:"bisection_accepted"`
	RecoveredBits   int     `json:"recovered_bits"`
	Recovered       bool    `json:"secret_recovered"`
	GreedyDistinctD int     `json:"greedy_distinct_dependency"`
}

func main() {
	workload := flag.String("agent-workload", "evaluation/agentworkload", "frozen benign workload directory")
	executed := flag.String("executed-x4", "evaluation/final-v5-wsl2/raw/pilot-benign-06/deployments/benign-x4/001/raw/benign.jsonl", "executed x4 arm sample (all statements accepted) whose charged increments give the system's own novelty per statement")
	liveCatalog := flag.String("catalog", "config/catalog.yaml", "live catalog path")
	out := flag.String("out", "evaluation/budget-utility-sweep/results.json", "output path")
	flag.Parse()

	// --- benign side -----------------------------------------------------
	feet, err := finalv5benign.StatementFootprints(finalv5benign.BuildInput{AgentWorkloadDir: *workload, LiveCatalogPath: *liveCatalog})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	manifest, err := finalv5benign.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	recipe := manifest.Budgets[0]
	base := budget{recipe.MaxReleaseFacts, recipe.MaxInfluence, recipe.MaxOutcome, recipe.MaxQueries}
	multipliers := []float64{0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4}
	charged, err := executedIncrements(*executed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	var benignExecuted []benignPoint
	for _, m := range multipliers {
		b := scale(base, m)
		var usedR, usedD, usedO, queries int64
		p := benignPoint{Multiplier: m, Budget: b}
		for _, f := range feet {
			if f.Classification == finalv5benign.ClassPolicyRefused {
				p.PolicyRefusals++
				continue
			}
			p.Authorized++
			c := charged[f.ID]
			queries++
			dim := ""
			switch {
			case queries > b.Q:
				dim = "Q"
			case usedD+c.D > b.D:
				dim = "D"
			case usedR+c.R > b.R:
				dim = "R"
			case usedO+c.O > b.O:
				dim = "O"
			}
			if dim != "" {
				p.BudgetRefusals++
				if p.FirstRefusal == "" {
					p.FirstRefusal, p.BindingDim = f.ID, dim
				}
				continue
			}
			usedR, usedD, usedO = usedR+c.R, usedD+c.D, usedO+c.O
			p.Accepted++
		}
		p.LedgerD = int(usedD)
		p.CompletionPct = 100 * float64(p.Accepted) / float64(p.Authorized)
		benignExecuted = append(benignExecuted, p)
	}
	var benign []benignPoint
	for _, m := range multipliers {
		b := scale(base, m)
		ledger := map[string]struct{}{}
		var usedR, usedO, queries int64
		p := benignPoint{Multiplier: m, Budget: b}
		for _, f := range feet {
			if f.Classification == finalv5benign.ClassPolicyRefused {
				p.PolicyRefusals++
				continue
			}
			p.Authorized++
			novelD := int64(0)
			for _, h := range f.Dependency {
				if _, ok := ledger[h]; !ok {
					novelD++
				}
			}
			// R and O follow the recipe's own accounting (per-statement sums);
			// D is the exact set union of closed-form facts.
			novelR := f.ReleaseFacts
			novelO := int64(1) + f.PredicateAtoms
			queries++
			dim := ""
			switch {
			case queries > b.Q:
				dim = "Q"
			case int64(len(ledger))+novelD > b.D:
				dim = "D"
			case usedR+novelR > b.R:
				dim = "R"
			case usedO+novelO > b.O:
				dim = "O"
			}
			if dim != "" {
				p.BudgetRefusals++
				if p.FirstRefusal == "" {
					p.FirstRefusal, p.BindingDim = f.ID, dim
				}
				continue
			}
			for _, h := range f.Dependency {
				ledger[h] = struct{}{}
			}
			usedR += novelR
			usedO += novelO
			p.Accepted++
		}
		p.LedgerD = len(ledger)
		p.CompletionPct = 100 * float64(p.Accepted) / float64(p.Authorized)
		benign = append(benign, p)
	}

	// --- adversary side --------------------------------------------------
	corpus, err := finalv5adversary.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	var bisect, greedyRows []finalv5adversary.StepOutcome
	var greedyFull finalv5adversary.StepOutcome
	for _, t := range corpus.Traces {
		if t.Strategy == "bisection" && t.Tier == "loosened" {
			bisect = t.Steps // the complete 11-probe ladder with its charges
		}
		if t.Strategy == "greedy" && t.Tier == "tightened" {
			greedyFull = t.Steps[0] // full listing: 6 release, 18 dependency, 2 outcome
			for _, s := range t.Steps[1:6] {
				greedyRows = append(greedyRows, s) // one row per step: 1/3/2
			}
		}
	}
	owner := budget{10, 18, 12, 24}
	var adversary []adversaryPoint
	for _, m := range multipliers {
		b := scale(owner, m)
		p := adversaryPoint{Multiplier: m, Budget: b}
		var r, d, o, q int64
		for _, s := range bisect {
			q++
			if q > b.Q || r+s.NovelRelease > b.R || d+s.NovelDep > b.D || o+s.NovelOutcome > b.O {
				break
			}
			r += s.NovelRelease
			d += s.NovelDep
			o += s.NovelOutcome
			p.BisectAccepted++
		}
		p.RecoveredBits = p.BisectAccepted
		p.Recovered = p.BisectAccepted >= len(bisect)
		// greedy: full listing if it fits, else row by row while it fits
		r, d, o, q = 0, 0, 0, 0
		if greedyFull.NovelRelease <= b.R && greedyFull.NovelDep <= b.D && greedyFull.NovelOutcome <= b.O && b.Q >= 1 {
			p.GreedyDistinctD = int(greedyFull.NovelDep)
		} else {
			for _, s := range greedyRows {
				q++
				if q > b.Q || r+s.NovelRelease > b.R || d+s.NovelDep > b.D || o+s.NovelOutcome > b.O {
					break
				}
				r += s.NovelRelease
				d += s.NovelDep
				o += s.NovelOutcome
			}
			p.GreedyDistinctD = int(d)
		}
		adversary = append(adversary, p)
	}

	result := map[string]any{
		"schema_version": 1,
		"method":         "admission arithmetic over frozen corpora and independent oracles; not measured",
		"benign_recipe":  base, "adversary_owner_budget": owner,
		"benign_corpus_sha256":       finalv5benign.CorpusSHA256(),
		"adversary_corpus_sha256":    finalv5adversary.CorpusSHA256(),
		"benign_corpus_model":        benign,
		"benign_executed_increments": benignExecuted,
		"benign_note":                "corpus_model: exact set union of the corpus's closed-form footprints (a conservative over-approximation of the production rule, see ledger); executed_increments: the system's own charged novelty per statement from the executed x4 arm in natural order, an upper bound on acceptance under smaller budgets because a refused statement's facts leave later novelty at least as large",
		"adversary":                  adversary,
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	for _, p := range benignExecuted {
		fmt.Printf("executed x%-4g accepted %d/%d refusals %d first=%s dim=%s ledgerD=%d\n", p.Multiplier, p.Accepted, p.Authorized, p.BudgetRefusals, p.FirstRefusal, p.BindingDim, p.LedgerD)
	}
	for _, p := range benign {
		fmt.Printf("corpus   x%-4g accepted %d/%d refusals %d first=%s dim=%s ledgerD=%d\n", p.Multiplier, p.Accepted, p.Authorized, p.BudgetRefusals, p.FirstRefusal, p.BindingDim, p.LedgerD)
	}
	for _, p := range adversary {
		fmt.Printf("adversary x%-4g bits %d recovered=%v greedyD=%d budget=%+v\n", p.Multiplier, p.RecoveredBits, p.Recovered, p.GreedyDistinctD, p.Budget)
	}
}

type increment struct{ R, D, O int64 }

// executedIncrements reads one executed benign sample (an arm in which every
// authorized statement was accepted) and returns each statement's charged
// Release/Dependency/Outcome novelty as the system settled it.
func executedIncrements(path string) (map[string]increment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var line struct {
		Sample struct {
			BenignVerification struct {
				Steps []struct {
					ID       string `json:"id"`
					Accepted bool   `json:"accepted"`
					R        int64  `json:"charged_release_facts"`
					D        int64  `json:"charged_dependency_facts"`
					O        int64  `json:"charged_outcome_facts"`
					Class    string `json:"classification"`
				} `json:"steps"`
			} `json:"benign_verification"`
		} `json:"sample"`
	}
	first := raw
	if i := indexByte(raw, '\n'); i >= 0 {
		first = raw[:i]
	}
	if err := json.Unmarshal(first, &line); err != nil {
		return nil, err
	}
	out := map[string]increment{}
	for _, st := range line.Sample.BenignVerification.Steps {
		if st.Class == "policy_refused" {
			continue
		}
		if !st.Accepted {
			return nil, fmt.Errorf("executed sample refused %s; need an arm that accepted every statement", st.ID)
		}
		out[st.ID] = increment{st.R, st.D, st.O}
	}
	return out, nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
