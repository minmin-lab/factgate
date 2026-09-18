// Command rls-trace-dump prints the frozen adaptive RLS trace (the 100
// statements of the sealed publication campaign's RLS profile) as JSON, with
// each statement's direct SQL, expected rows and oracle fact counts, so that
// an external per-query control (P10.B8: Passant) can replay exactly the same
// statements against the same fixture. It reads the embedded corpus and writes
// nothing else.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
)

type dumpStep struct {
	Index        int        `json:"index"`
	ID           string     `json:"id"`
	Family       string     `json:"family"`
	Variant      string     `json:"variant"`
	DirectSQL    string     `json:"direct_sql"`
	ExpectedRows [][]string `json:"expected_rows"`
	Scalar       *int64     `json:"scalar,omitempty"`
	Release      int        `json:"release_facts"`
	Dependency   int        `json:"dependency_facts"`
	Outcome      int        `json:"outcome_facts"`
}

func main() {
	manifest, err := finalv5rls.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	steps, err := manifest.Trace()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out := struct {
		CorpusID     string                 `json:"corpus_id"`
		CorpusSHA256 string                 `json:"corpus_sha256"`
		Department   string                 `json:"policy_department"`
		Rows         []finalv5rls.FixtureRow `json:"rows"`
		Steps        []dumpStep             `json:"steps"`
	}{CorpusID: manifest.CorpusID, CorpusSHA256: finalv5rls.SHA256(finalv5rls.Bytes()), Department: manifest.PolicyDepartment, Rows: manifest.Rows}
	for _, s := range steps {
		out.Steps = append(out.Steps, dumpStep{Index: s.Index, ID: s.ID, Family: s.Family, Variant: s.Variant, DirectSQL: s.DirectSQL,
			ExpectedRows: s.ExpectedRows, Scalar: s.Scalar, Release: len(s.Oracle.Release), Dependency: len(s.Oracle.Dependency), Outcome: len(s.Oracle.Outcome)})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		os.Exit(1)
	}
}
