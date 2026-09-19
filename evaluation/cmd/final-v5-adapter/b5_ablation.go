package main

// P10-R2.E1 B5 cost-ablation driver (docs/p10_r2_b5_ablation_design.md).
//
// Runs the frozen Baseline cells' governed statements as fresh novel queries
// against whatever deployment the adapter environment names, and keeps the
// Gateway's own disjoint timing maps (pipeline_ms, component_ms,
// diagnostic_ms) per sample. The same driver serves both end-to-end arms:
//
//   arm iv  the deployment serves the master (V5) Catalog: fact accounting on;
//   arm i   the deployment serves the exposure-free twin
//           (evaluation/config/b5/master-no-exposure.catalog.yaml): the same
//           routes and budgets, no snapshot publications, no fact limits, so
//           ExposureGrant.Enabled() is false and the Gateway takes its native
//           no-accounting path (internal/gateway/query.go:717).
//
// Each sample provisions a fresh Task through the real request/OA/grant path
// (so every query is novel, as the Baseline's novel mode is) and issues the
// cell's governed payload through the entrypoint its contract names. The
// record keeps what the response carried; on arm i the exposure and ledger
// fields are simply zero. No acceptance is applied here: the analysis
// (evaluation/b5-ablation/analyze.py) takes medians per cell and arm and
// composes arm ii from arm iv's ledger leaves.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const b5AblationVersion = 1

type b5SampleRecord struct {
	SchemaVersion    int                            `json:"schema_version"`
	CampaignClass    string                         `json:"campaign_class"`
	Arm              string                         `json:"arm"`
	DeploymentID     string                         `json:"deployment_id"`
	Cell             string                         `json:"cell"`
	WorkloadID       string                         `json:"workload_id"`
	Scale            string                         `json:"scale"`
	Sample           int                            `json:"sample"`
	TaskIDHash       string                         `json:"task_id_hash"`
	RequestIDHash    string                         `json:"request_id_hash"`
	Entrypoint       string                         `json:"entrypoint"`
	PayloadSHA256    string                         `json:"payload_sha256"`
	Products         []string                       `json:"products"`
	BudgetProfile    string                         `json:"budget_profile,omitempty"`
	ClientMS         float64                        `json:"client_ms"`
	Outcome          string                         `json:"outcome"` // settled | refused | error
	Code             string                         `json:"code,omitempty"`
	Message          string                         `json:"message,omitempty"`
	RowCount         int64                          `json:"row_count"`
	ColumnCount      int                            `json:"column_count"`
	ArtifactStatus   string                         `json:"artifact_status,omitempty"`
	SemanticReplay   bool                           `json:"semantic_replay"`
	IdempotentReplay bool                           `json:"idempotent_replay"`
	PipelineMS       map[string]float64             `json:"pipeline_ms,omitempty"`
	ComponentMS      map[string]float64             `json:"component_ms,omitempty"`
	DiagnosticMS     map[string]float64             `json:"diagnostic_ms,omitempty"`
	ExposureProfile  string                         `json:"exposure_profile_version,omitempty"`
	ActualRelease    int64                          `json:"actual_release_facts"`
	ActualDep        int64                          `json:"actual_dependency_facts"`
	ActualOutcome    int64                          `json:"actual_outcome_facts"`
	ChargedRelease   int64                          `json:"charged_release_facts"`
	ChargedDep       int64                          `json:"charged_dependency_facts"`
	ChargedOutcome   int64                          `json:"charged_outcome_facts"`
	CASAttempts      int64                          `json:"cas_attempts"`
	CASConflicts     int64                          `json:"cas_conflicts"`
	RootEpochAfter   int64                          `json:"root_epoch_after,omitempty"`
	LedgerBefore     *experiment.RootLedgerSnapshot `json:"ledger_before,omitempty"`
	LedgerAfter      *experiment.RootLedgerSnapshot `json:"ledger_after,omitempty"`
	Error            string                         `json:"error,omitempty"`
}

// runB5Ablation executes samples for each "workload/scale" cell in order and
// appends one JSON record per sample to outPath.
func runB5Ablation(ctx context.Context, outPath, arm, deploymentID string, cells []string, samples int) error {
	if arm != "i" && arm != "iv" {
		return fmt.Errorf("b5 arm must be i or iv, got %q", arm)
	}
	if samples < 1 {
		return errors.New("b5 samples must be >= 1")
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return err
	}
	defer real.Close()
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	encoder := json.NewEncoder(out)
	for _, cellName := range cells {
		workload, scale, ok := strings.Cut(cellName, "/")
		if !ok {
			return fmt.Errorf("cell %q must be workload/scale", cellName)
		}
		for sample := 1; sample <= samples; sample++ {
			operation := experiment.AdapterOperation{
				SchemaVersion: 1, CampaignClass: "pilot", DeploymentID: deploymentID, ExperimentID: "b5-ablation",
				CellID: cellName + "/novel", SampleID: fmt.Sprintf("b5-%s-%s-%s-%03d", arm, workload, scale, sample),
				PairID:     fmt.Sprintf("b5-%s-%s-%s-%03d", arm, workload, scale, sample),
				WorkloadID: workload, Scale: scale, Mode: "novel",
			}
			record := b5SampleRecord{SchemaVersion: b5AblationVersion, CampaignClass: "pilot", Arm: arm,
				DeploymentID: deploymentID, Cell: cellName, WorkloadID: workload, Scale: scale, Sample: sample}
			if err := b5RunSample(ctx, real, operation, &record); err != nil {
				record.Error = err.Error()
				if record.Outcome == "" {
					record.Outcome = "error"
				}
			}
			if err := encoder.Encode(record); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "b5-ablation arm=%s %s sample %d outcome=%s code=%s client=%.0fms server_total=%.0fms charged=%d/%d/%d %s\n",
				arm, cellName, sample, record.Outcome, record.Code, record.ClientMS, record.PipelineMS["server_total"],
				record.ChargedRelease, record.ChargedDep, record.ChargedOutcome, strings.TrimSpace(record.Error))
		}
	}
	return nil
}

func b5RunSample(ctx context.Context, real *realAdapter, operation experiment.AdapterOperation, record *b5SampleRecord) error {
	cell, err := resolveBaselineExecutionCell(operation)
	if err != nil {
		return err
	}
	record.Products = append([]string(nil), cell.Contract.ProductIDs...)
	record.PayloadSHA256 = sha(cell.BDGSQL)
	record.Entrypoint = "query_sql"
	if cell.PlanEntrypoint {
		record.Entrypoint = "execute_plan"
	}
	taskID, err := real.provisionBoundTask(ctx, operation, cell.Task)
	if err != nil {
		return fmt.Errorf("provision task: %w", err)
	}
	record.TaskIDHash = sha(taskID)
	if before, err := real.rootLedgerSnapshot(ctx, taskID); err == nil {
		record.LedgerBefore = &before
	}
	requestID := operation.PairID
	record.RequestIDHash = sha(requestID)
	started := time.Now()
	response, err := real.callGovernedArm(ctx, baselinePlan{planEntrypoint: cell.PlanEntrypoint}, taskID, requestID, cell.BDGSQL)
	record.ClientMS = durationMS(time.Since(started))
	if err != nil {
		var structured *mcpCallError
		if errors.As(err, &structured) {
			record.Outcome, record.Code, record.Message = "refused", structured.Code, structured.Message
			return nil
		}
		record.Outcome, record.Code = "error", "HARNESS_ERROR"
		return err
	}
	record.Outcome = "settled"
	record.RowCount, record.ColumnCount = response.RowCount, response.ColumnCount
	record.ArtifactStatus = response.ArtifactStatus
	record.SemanticReplay, record.IdempotentReplay = response.SemanticReplay, response.IdempotentReplay
	record.PipelineMS, record.ComponentMS, record.DiagnosticMS = response.PipelineMS, response.ComponentMS, response.DiagnosticMS
	record.ExposureProfile = response.Exposure.ProfileVersion
	record.ActualRelease, record.ActualDep, record.ActualOutcome = response.Exposure.ActualReleaseFacts, response.Exposure.ActualInfluenceFacts, response.Exposure.ActualOutcomeFacts
	record.ChargedRelease, record.ChargedDep, record.ChargedOutcome = response.Exposure.ChargedReleaseFacts, response.Exposure.ChargedInfluenceFacts, response.Exposure.ChargedOutcomeFacts
	record.CASAttempts, record.CASConflicts = response.OutcomeRadix.CASAttempts, response.OutcomeRadix.CASConflicts
	record.RootEpochAfter = response.Exposure.RootEpoch
	if after, err := real.rootLedgerSnapshot(ctx, taskID); err == nil {
		record.LedgerAfter = &after
	}
	return nil
}
