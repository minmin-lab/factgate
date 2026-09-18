package finalv5benign

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// StatementFootprint is one frozen benign statement with the exact
// a-priori Dependency fact hashes the closed-form dataset model derives for
// it. It exists for admission arithmetic (budget sweeps) over the same
// oracle that built the corpus; it is not part of the corpus encoding.
type StatementFootprint struct {
	Index          int
	ID             string
	Classification Classification
	ReleaseFacts   int64
	PredicateAtoms int64
	Dependency     []string
}

// StatementFootprints replays BuildManifest's classification and closed-form
// evaluation with a fresh collector per statement, so each statement's
// Dependency set is available for set-valued admission arithmetic.
func StatementFootprints(input BuildInput) ([]StatementFootprint, error) {
	reportBytes, err := os.ReadFile(filepath.Join(input.AgentWorkloadDir, "results.json"))
	if err != nil {
		return nil, err
	}
	var report lowerabilityReport
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		return nil, err
	}
	if len(report.Passes) == 0 || report.Passes[0].Variant != "as-written" {
		return nil, errors.New("agent-workload lowerability report lacks the as-written pass")
	}
	live, err := catalog.Load(input.LiveCatalogPath)
	if err != nil {
		return nil, err
	}
	var out []StatementFootprint
	index := 0
	for _, entry := range report.Passes[0].Results {
		if !entry.Lowerable {
			continue
		}
		index++
		raw, err := os.ReadFile(filepath.Join(input.AgentWorkloadDir, "queries", entry.Query+".sql"))
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != entry.SQLSHA256 {
			return nil, fmt.Errorf("statement %s drifted from the frozen lowerability report", entry.Query)
		}
		statement := Statement{Index: index, ID: entry.Query, SQL: string(raw), SQLSHA256: entry.SQLSHA256}
		collector := newFactCollector()
		if err := classifyAndEvaluate(&statement, live, collector); err != nil {
			return nil, fmt.Errorf("statement %s: %w", entry.Query, err)
		}
		out = append(out, StatementFootprint{Index: index, ID: statement.ID,
			Classification: statement.Classification, ReleaseFacts: statement.ReleaseFacts,
			PredicateAtoms: statement.PredicateAtoms,
			Dependency:     append([]string(nil), collector.hashes...)})
	}
	return out, nil
}
