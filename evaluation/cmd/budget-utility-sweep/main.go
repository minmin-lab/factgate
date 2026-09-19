// budget-utility-sweep derives, by admission arithmetic over the frozen
// corpora and their independent oracles, how benign statement admission and
// adversarial extraction move with the owner recipe's budget multiplier.
// Nothing here is measured: the benign side replays the corpus's closed-form
// footprints statement by statement under the set-ledger rule (a refused
// statement adds nothing; each statement's Dependency novelty is recomputed
// against the history that the replay itself admitted), and the adversary
// side replays the data-independent per-step charges recorded in the
// adversary corpus. The executed pilots (benign 1x/2x/4x, adversary
// tightened/owner/loosened) validate the arithmetic at their points.
//
// Earlier revisions also emitted a second benign curve that replayed the
// per-statement charges the deployed system recorded in the executed 4x arm
// as fixed increments under smaller budgets. That replay does not update the
// history after a refusal, and the accepted-statement count it yields is not
// an upper bound (a refused large statement can make a later statement's true
// novelty larger than its recorded increment, and admitting a statement whose
// cost is understated can crowd out several later ones). The curve is
// withdrawn; see evaluation/budget-utility-sweep/README.md.
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
	Admitted       int     `json:"admitted"`
	BudgetRefusals int     `json:"budget_refusals"`
	PolicyRefusals int     `json:"policy_refusals"`
	AdmittedPct    float64 `json:"admitted_pct"`
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
			// Dependency: exact set difference against the history this
			// replay admitted so far (a refused statement adds nothing).
			novelD := int64(0)
			for _, h := range f.Dependency {
				if _, ok := ledger[h]; !ok {
					novelD++
				}
			}
			// Release and Outcome: the corpus records counts, not fact
			// sets, so they follow the recipe's own per-statement sums.
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
			p.Admitted++
		}
		p.LedgerD = len(ledger)
		p.AdmittedPct = 100 * float64(p.Admitted) / float64(p.Authorized)
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
		"schema_version": 2,
		"method":         "admission arithmetic over frozen corpora and independent oracles; not measured",
		"benign_recipe":  base, "adversary_owner_budget": owner,
		"benign_corpus_sha256":    finalv5benign.CorpusSHA256(),
		"adversary_corpus_sha256": finalv5adversary.CorpusSHA256(),
		"benign":                  benign,
		"benign_note":             "exact replay of the corpus's closed-form footprints in trace order: Dependency novelty is the set difference against the history this replay admitted (a refused statement adds nothing); Release and Outcome are the recipe's per-statement sums because the corpus records their counts, not their fact sets. The closed-form footprints over-approximate the production rule (scanned rows where the rule counts output cells), so at a given multiplier the replay admits no more than the deployed ledger would on the same trace; admitted_pct is admitted authorized statements over authorized statements, not business-task completion. Schema 1 also carried a fixed-increment replay of the executed 4x arm labelled an upper bound; that label was wrong and the curve is withdrawn.",
		"adversary":               adversary,
	}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sweep:", err)
		os.Exit(1)
	}
	for _, p := range benign {
		fmt.Printf("benign    x%-4g admitted %d/%d refusals %d first=%s dim=%s ledgerD=%d\n", p.Multiplier, p.Admitted, p.Authorized, p.BudgetRefusals, p.FirstRefusal, p.BindingDim, p.LedgerD)
	}
	for _, p := range adversary {
		fmt.Printf("adversary x%-4g bits %d recovered=%v greedyD=%d budget=%+v\n", p.Multiplier, p.RecoveredBits, p.Recovered, p.GreedyDistinctD, p.Budget)
	}
}
