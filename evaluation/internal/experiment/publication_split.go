package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
)

const SplitPublicationCampaignVersion = "taskgate-final-v5-split-publication-campaign-v1"

type publicationFileKey struct{ kind, experiment string }

type SplitPublicationSection struct {
	ExecutionModel   string   `json:"execution_model"`
	Cells            int      `json:"cells"`
	FreshExecutions  int      `json:"fresh_executions"`
	ProfileBinding   string   `json:"profile_binding"`
	StateInheritance bool     `json:"state_inheritance"`
	EvidenceSHA256   []string `json:"evidence_sha256"`
}

type SplitPublicationCampaignSummary struct {
	SchemaVersion       int                                `json:"schema_version"`
	Record              string                             `json:"record"`
	Status              string                             `json:"status"`
	CampaignClass       string                             `json:"campaign_class"`
	PublicationEligible bool                               `json:"publication_eligible"`
	FormalCampaign      bool                               `json:"formal_campaign"`
	CampaignID          string                             `json:"campaign_id"`
	SubmissionCommit    string                             `json:"submission_commit"`
	PlanSHA256          string                             `json:"plan_sha256"`
	ProfileCells        int                                `json:"profile_cells"`
	ScaleNonProfile     int                                `json:"scale_non_profile_cells"`
	CompilerNonProfile  int                                `json:"compiler_non_profile_cells"`
	TotalCells          int                                `json:"total_cells"`
	Profile             SplitPublicationSection            `json:"profile_campaign"`
	NonProfile          map[string]SplitPublicationSection `json:"non_profile_campaigns"`
}

// ValidateSplitPublicationPlan is the zero-deployment publication dry gate.
// The values are deliberately exact: changing any denominator or execution
// model requires a new source-controlled plan rather than a permissive reader.
func ValidateSplitPublicationPlan(plan finalv5profile.CampaignPlan) error {
	if plan.ContractRelease != "final-v5-contracts-v1.11" && plan.ContractRelease != "final-v5-contracts-v1.13" {
		return fmt.Errorf("publication plan names contract release %q, want a reviewed publication release (v1.11 or v1.13)", plan.ContractRelease)
	}
	profileCells := 0
	profileSeen := map[string]bool{}
	for _, deployment := range plan.Deployments {
		if !deployment.Ready || deployment.Alias == "" || len(deployment.Cells) == 0 {
			return errors.New("publication plan contains an absent or unready profile deployment")
		}
		for _, cell := range deployment.Cells {
			if profileSeen[cell] {
				return fmt.Errorf("publication profile cell %q is assigned more than once", cell)
			}
			profileSeen[cell] = true
			profileCells++
		}
	}
	if len(plan.Deployments) != 11 || profileCells != 125 || len(plan.PreregisteredAggregates) != 0 {
		return fmt.Errorf("publication profile plan is %d deployments/%d cells/%d pilot aggregates, want 11/125/0",
			len(plan.Deployments), profileCells, len(plan.PreregisteredAggregates))
	}
	nonProfileSeen := map[string]bool{}
	scaleCells, compilerCells := 0, 0
	wantGroups := map[string]struct {
		experiment, profile                string
		cells, processes, warmups, samples int
	}{
		"scale-outcome-merkle": {"scale", "scale", 36, 1, 5, 30},
		"compiler":             {"compiler", "compiler", 11, 5, 1, 100},
	}
	for _, campaign := range plan.NonProfileCampaigns {
		want, known := wantGroups[campaign.ID]
		if campaign.ExecutionModel != "deployment_free_process" || campaign.FreshExecutions != 3 ||
			campaign.StateInheritance || campaign.ProfileBinding != "forbidden" || len(campaign.Cells) == 0 {
			return fmt.Errorf("non-profile campaign %q has an invalid fresh execution model", campaign.ID)
		}
		if !known || campaign.ExperimentID != want.experiment || campaign.ProtocolProfile != want.profile ||
			len(campaign.Cells) != want.cells || campaign.ProcessReplicates != want.processes ||
			campaign.WarmupsPerCell != want.warmups || campaign.MeasuredSamplesPerCell != want.samples {
			return fmt.Errorf("non-profile campaign %q has invalid frozen replicate counts", campaign.ID)
		}
		for _, cell := range campaign.Cells {
			if profileSeen[cell] || nonProfileSeen[cell] {
				return fmt.Errorf("non-profile cell %q is duplicated or attached to a profile", cell)
			}
			nonProfileSeen[cell] = true
			if campaign.ExperimentID == "scale" {
				scaleCells++
			} else if campaign.ExperimentID == "compiler" {
				compilerCells++
			} else {
				return fmt.Errorf("non-profile campaign %q names unsupported experiment %q", campaign.ID, campaign.ExperimentID)
			}
		}
	}
	if len(plan.NonProfileCampaigns) != 2 || len(nonProfileSeen) != 47 || scaleCells != 36 || compilerCells != 11 ||
		len(plan.NonProfileCells) != len(nonProfileSeen) {
		return fmt.Errorf("publication non-profile plan is groups/cells/scale/compiler=%d/%d/%d/%d, want 2/47/36/11",
			len(plan.NonProfileCampaigns), len(nonProfileSeen), scaleCells, compilerCells)
	}
	for _, cell := range plan.NonProfileCells {
		if !nonProfileSeen[cell] {
			return fmt.Errorf("non-profile cell list contains unplanned identity %q", cell)
		}
	}
	return nil
}

// FinalizeSplitPublicationCampaign conjunctively validates the profile-backed
// and deployment-free sections. Neither section can independently claim the
// complete 172-cell publication denominator.
func FinalizeSplitPublicationCampaign(root, planPath string) (SplitPublicationCampaignSummary, error) {
	var summary SplitPublicationCampaignSummary
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		return summary, err
	}
	var plan finalv5profile.CampaignPlan
	if err := StrictJSON(planBytes, &plan); err != nil {
		return summary, fmt.Errorf("campaign plan: %w", err)
	}
	if err := ValidateSplitPublicationPlan(plan); err != nil {
		return summary, err
	}
	planSHA := sha256Hex(planBytes)
	profileCells, campaignID, commit, profileDigests, err := finalizePublicationProfiles(root, plan)
	if err != nil {
		return summary, err
	}
	nonProfile := map[string]SplitPublicationSection{}
	scaleCells, compilerCells := 0, 0
	for _, campaign := range plan.NonProfileCampaigns {
		runDir := filepath.Join(root, "non-profile", campaign.ID)
		selectedPath := filepath.Join(runDir, "selected-cells.json")
		selectedBytes, err := os.ReadFile(selectedPath)
		if err != nil {
			return summary, fmt.Errorf("non-profile %s selection: %w", campaign.ID, err)
		}
		var selected []string
		if err := StrictJSON(selectedBytes, &selected); err != nil || !sameSortedStrings(selected, campaign.Cells) {
			return summary, fmt.Errorf("non-profile %s selection differs from the formal plan", campaign.ID)
		}
		config, _, err := LoadConfig(filepath.Join(runDir, "config.json"), campaign.ExperimentID)
		if err != nil {
			return summary, fmt.Errorf("non-profile %s config: %w", campaign.ID, err)
		}
		if config.CampaignClass != "publication" || config.CampaignID != campaignID ||
			config.SubmissionCommit != commit || config.ProtocolProfile != campaign.ProtocolProfile ||
			config.Deployments != campaign.FreshExecutions {
			return summary, fmt.Errorf("non-profile %s identity differs from the formal campaign", campaign.ID)
		}
		if err := validateNonProfileRawFiles(runDir, config, campaign); err != nil {
			return summary, fmt.Errorf("non-profile %s: %w", campaign.ID, err)
		}
		runSummary, err := FinalizeNonProfileRun(runDir, selected)
		if err != nil || runSummary.Status != "pass" || !runSummary.PublicationEligible {
			return summary, fmt.Errorf("non-profile %s did not pass the experiment finalizer: %w", campaign.ID, err)
		}
		manifest, err := SealRun(runDir)
		if err != nil {
			return summary, fmt.Errorf("seal non-profile %s: %w", campaign.ID, err)
		}
		manifestPath := filepath.Join(runDir, "evidence", "manifest.json")
		manifestSHA, err := FileSHA256(manifestPath)
		if err != nil || manifest.CampaignID != campaignID || manifest.SubmissionCommit != commit {
			return summary, fmt.Errorf("non-profile %s sealed evidence identity is invalid", campaign.ID)
		}
		nonProfile[campaign.ID] = SplitPublicationSection{
			ExecutionModel: campaign.ExecutionModel, Cells: len(campaign.Cells), FreshExecutions: 3,
			ProfileBinding: "forbidden", StateInheritance: false, EvidenceSHA256: []string{manifestSHA},
		}
		if campaign.ExperimentID == "scale" {
			scaleCells += len(campaign.Cells)
		} else {
			compilerCells += len(campaign.Cells)
		}
	}
	summary = SplitPublicationCampaignSummary{
		SchemaVersion: 1, Record: SplitPublicationCampaignVersion, Status: "pass",
		CampaignClass: "publication", PublicationEligible: true, FormalCampaign: true,
		CampaignID: campaignID, SubmissionCommit: commit, PlanSHA256: planSHA,
		ProfileCells: profileCells, ScaleNonProfile: scaleCells, CompilerNonProfile: compilerCells,
		TotalCells: profileCells + scaleCells + compilerCells,
		Profile: SplitPublicationSection{ExecutionModel: "fresh_profile_deployment", Cells: profileCells,
			FreshExecutions: 3, ProfileBinding: "required", StateInheritance: false, EvidenceSHA256: profileDigests},
		NonProfile: nonProfile,
	}
	if summary.ProfileCells != 125 || summary.ScaleNonProfile != 36 || summary.CompilerNonProfile != 11 || summary.TotalCells != 172 {
		return SplitPublicationCampaignSummary{}, errors.New("publication evidence does not close the 125+36+11 denominator")
	}
	return summary, nil
}

func finalizePublicationProfiles(root string, plan finalv5profile.CampaignPlan) (int, string, string, []string, error) {
	seenRecords := map[string]bool{}
	seenCells := map[string]bool{}
	campaignID, commit := "", ""
	var recordDigests []string
	for _, planned := range plan.Deployments {
		for repetition := 1; repetition <= 3; repetition++ {
			recordPath := filepath.Join(root, "deployments", planned.Alias, fmt.Sprintf("%03d", repetition), "deployment-record.json")
			payload, err := os.ReadFile(recordPath)
			if err != nil {
				return 0, "", "", nil, fmt.Errorf("profile record %s/%03d: %w", planned.Alias, repetition, err)
			}
			var record finalv5profile.ProfileCampaignDeploymentRecord
			if err := StrictJSON(payload, &record); err != nil {
				return 0, "", "", nil, err
			}
			key := fmt.Sprintf("%s/%03d", planned.Alias, repetition)
			if seenRecords[key] || record.SchemaVersion != 1 || record.CampaignClass != "publication" ||
				!record.PublicationEligible || !record.FormalCampaign || record.Repetition != repetition ||
				record.ProfileID != planned.ProfileID || record.ProfileAlias != planned.Alias ||
				record.CatalogPath != planned.CatalogPath || record.CatalogSHA256 != planned.CatalogSHA256 ||
				!sameSortedStrings(record.Cells, planned.Cells) {
				return 0, "", "", nil, fmt.Errorf("profile record %s differs from the formal plan", key)
			}
			seenRecords[key] = true
			if campaignID == "" {
				campaignID, commit = record.CampaignID, record.SubmissionCommit
			} else if record.CampaignID != campaignID || record.SubmissionCommit != commit {
				return 0, "", "", nil, errors.New("profile records differ in campaign or submission commit")
			}
			if err := validatePublicationProfileRecord(root, planned, repetition, record); err != nil {
				return 0, "", "", nil, fmt.Errorf("profile record %s: %w", key, err)
			}
			recordDigests = append(recordDigests, sha256Hex(payload))
			if repetition == 1 {
				for _, cell := range record.Cells {
					seenCells[cell] = true
				}
			}
		}
	}
	sort.Strings(recordDigests)
	if len(seenRecords) != 33 || len(seenCells) != 125 || campaignID == "" || !fullSHA.MatchString(commit) {
		return 0, "", "", nil, fmt.Errorf("profile publication evidence is records/cells=%d/%d, want 33/125", len(seenRecords), len(seenCells))
	}
	return len(seenCells), campaignID, commit, recordDigests, nil
}

func validatePublicationProfileRecord(root string, planned finalv5profile.PlannedDeploy, repetition int,
	record finalv5profile.ProfileCampaignDeploymentRecord) error {
	files := map[publicationFileKey]finalv5profile.CampaignEvidenceFile{}
	plannedExperiments := map[string]bool{}
	for _, experimentID := range planned.Experiments {
		plannedExperiments[experimentID] = true
	}
	for _, file := range record.Files {
		if file.Path == "" || filepath.IsAbs(file.Path) || filepath.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "../") {
			return errors.New("evidence path is not a clean campaign-relative path")
		}
		path := filepath.Join(root, file.Path)
		info, err := os.Lstat(path)
		actual, hashErr := FileSHA256(path)
		if err != nil || hashErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
			actual != file.SHA256 || info.Size() != file.Bytes {
			return fmt.Errorf("evidence file %s is absent, unsafe, or changed", file.Path)
		}
		key := publicationFileKey{file.Kind, file.Experiment}
		if _, duplicate := files[key]; duplicate {
			return fmt.Errorf("duplicate evidence kind %s/%s", file.Kind, file.Experiment)
		}
		switch file.Kind {
		case "profile_binding", "activation_evidence", "environment", "fresh_proof", "gateway_image", "cleanup",
			"deployment_configuration", "formal_gateway_runtime", "formal_gateway_build_manifest",
			"formal_gateway_compose_override", "formal_gateway_build_log":
			if file.Experiment != "" {
				return fmt.Errorf("deployment evidence %s names an experiment", file.Kind)
			}
		case "config", "selected_cells", "raw_jsonl", "launcher_gate", "adapter_stderr", "adapter_stderr_credential_scan":
			if !plannedExperiments[file.Experiment] {
				return fmt.Errorf("evidence %s names unplanned experiment %q", file.Kind, file.Experiment)
			}
		case "runner_selected_cells", "rq5_cell_translation", "rq5_cell_map", "rq5_profile_binding",
			"rq5_catalog_family", "rq5_build_manifest":
			if file.Experiment != "rq5" || !plannedExperiments["rq5"] {
				return fmt.Errorf("evidence %s is not owned by a planned RQ5 experiment", file.Kind)
			}
		default:
			return fmt.Errorf("publication profile evidence has unknown kind %q", file.Kind)
		}
		files[key] = file
	}
	for _, kind := range []string{"profile_binding", "activation_evidence", "environment", "fresh_proof", "gateway_image",
		"cleanup", "deployment_configuration", "formal_gateway_runtime", "formal_gateway_build_manifest",
		"formal_gateway_compose_override", "formal_gateway_build_log"} {
		if files[publicationFileKey{kind, ""}].Path == "" {
			return fmt.Errorf("missing deployment evidence kind %s", kind)
		}
	}
	var binding ProfileBinding
	if err := readStrictFile(filepath.Join(root, files[publicationFileKey{"profile_binding", ""}].Path), &binding); err != nil ||
		binding.Validate() != nil || binding.ProfileID != planned.ProfileID || binding.CatalogSHA256 != planned.CatalogSHA256 {
		return errors.New("profile binding differs from the formal profile plan")
	}
	var activation finalv5profile.ActivationEvidence
	if err := readStrictFile(filepath.Join(root, files[publicationFileKey{"activation_evidence", ""}].Path), &activation); err != nil ||
		activation.SchemaVersion != 1 || activation.Record != finalv5profile.ActivationEvidenceVersion ||
		activation.CampaignClass != "pilot" || activation.PublicationEligible || activation.Status != "pass" ||
		!activation.ActivationSmokePassed || activation.WorkloadTargetedValidationPassed ||
		activation.DeploymentID != fmt.Sprintf("deployment-%02d", repetition) ||
		activation.ProfileID != planned.ProfileID || activation.ProfileAlias != planned.Alias ||
		activation.CatalogSHA256 != planned.CatalogSHA256 || activation.DatasetBindingSHA != binding.DatasetBindingSHA256 {
		return errors.New("profile activation observation differs from the formal execution")
	}
	var environment EnvironmentManifest
	if err := readStrictFile(filepath.Join(root, files[publicationFileKey{"environment", ""}].Path), &environment); err != nil ||
		environment.SchemaVersion != 1 || environment.CampaignID != record.CampaignID ||
		environment.DeploymentID != fmt.Sprintf("deployment-%02d", repetition) ||
		environment.GitCommit != record.SubmissionCommit || len(environment.GitStatus) != 0 || environment.PublicationEligible {
		return errors.New("profile environment observation is not clean and bound to the formal execution")
	}
	if datasetBinding, _ := environment.Datasets[datasetBindingFileSHAKey].(string); datasetBinding != binding.DatasetBindingSHA256 {
		return errors.New("profile environment observation differs from the ProfileBinding Dataset identity")
	}
	if err := finalv5profile.ValidateGatewayImageObservation(root, record.Files); err != nil {
		return err
	}
	if err := finalv5profile.ValidateProfileDeploymentConfigEvidence(root, planned, record.Files); err != nil {
		return err
	}
	var cleanup struct {
		SchemaVersion int             `json:"schema_version"`
		Status        string          `json:"status"`
		Containers    int             `json:"containers"`
		Volumes       int             `json:"volumes"`
		Networks      int             `json:"networks"`
		RQ5           json.RawMessage `json:"rq5,omitempty"`
	}
	if err := readStrictFile(filepath.Join(root, files[publicationFileKey{"cleanup", ""}].Path), &cleanup); err != nil ||
		cleanup.SchemaVersion != 1 || cleanup.Status != "pass" || cleanup.Containers != 0 || cleanup.Volumes != 0 || cleanup.Networks != 0 {
		return errors.New("profile execution cleanup proof is incomplete")
	}
	var fresh struct {
		SchemaVersion       int             `json:"schema_version"`
		Record              string          `json:"record"`
		Status              string          `json:"status"`
		CampaignClass       string          `json:"campaign_class"`
		PublicationEligible bool            `json:"publication_eligible"`
		FormalCampaign      bool            `json:"formal_campaign"`
		CampaignID          string          `json:"campaign_id"`
		SubmissionCommit    string          `json:"submission_commit"`
		ProfileAlias        string          `json:"profile_alias"`
		ProfileID           string          `json:"profile_id"`
		CatalogSHA256       string          `json:"catalog_sha256"`
		Repetition          int             `json:"repetition"`
		ComposeProject      string          `json:"compose_project"`
		FormalGatewayBuilt  bool            `json:"formal_gateway_built"`
		FormalGateway       json.RawMessage `json:"formal_gateway"`
		FinalizerMaterial   json.RawMessage `json:"finalizer_material_dispatch,omitempty"`
	}
	if err := readStrictFile(filepath.Join(root, files[publicationFileKey{"fresh_proof", ""}].Path), &fresh); err != nil ||
		fresh.SchemaVersion != 1 || fresh.Record != "taskgate-final-v5-fresh-profile-execution-v1" ||
		fresh.Status != "pass" || fresh.CampaignClass != "publication" || !fresh.PublicationEligible || !fresh.FormalCampaign ||
		fresh.CampaignID != record.CampaignID || fresh.SubmissionCommit != record.SubmissionCommit ||
		fresh.ProfileAlias != record.ProfileAlias || fresh.ProfileID != record.ProfileID ||
		fresh.CatalogSHA256 != record.CatalogSHA256 || fresh.Repetition != repetition ||
		fresh.ComposeProject == "" || !fresh.FormalGatewayBuilt || len(fresh.FormalGateway) == 0 {
		return errors.New("fresh profile-deployment proof differs from the formal record")
	}
	if err := validatePublicationFinalizerMaterial(fresh.FinalizerMaterial, planned.Experiments); err != nil {
		return err
	}
	if err := validatePublicationFormalGateway(root, files, binding, record, fresh.FormalGateway); err != nil {
		return err
	}
	for _, experimentID := range planned.Experiments {
		configFile, rawFile, gateFile := files[publicationFileKey{"config", experimentID}], files[publicationFileKey{"raw_jsonl", experimentID}], files[publicationFileKey{"launcher_gate", experimentID}]
		campaignSelectedFile := files[publicationFileKey{"selected_cells", experimentID}]
		selectedFile := campaignSelectedFile
		if experimentID == "rq5" {
			selectedFile = files[publicationFileKey{"runner_selected_cells", experimentID}]
		}
		stderrFile := files[publicationFileKey{"adapter_stderr", experimentID}]
		stderrScanFile := files[publicationFileKey{"adapter_stderr_credential_scan", experimentID}]
		if configFile.Path == "" || campaignSelectedFile.Path == "" || selectedFile.Path == "" || rawFile.Path == "" || gateFile.Path == "" ||
			stderrFile.Path == "" || stderrScanFile.Path == "" {
			return fmt.Errorf("experiment %s lacks config/selection/raw/gate/stderr/credential-scan evidence", experimentID)
		}
		if err := validatePublicationAdapterStderr(root, stderrFile, stderrScanFile); err != nil {
			return fmt.Errorf("experiment %s: %w", experimentID, err)
		}
		config, _, err := LoadConfig(filepath.Join(root, configFile.Path), experimentID)
		if err != nil || config.CampaignClass != "publication" || config.CampaignID != record.CampaignID ||
			config.SubmissionCommit != record.SubmissionCommit || config.Deployments != 3 {
			return fmt.Errorf("experiment %s publication config is invalid: %w", experimentID, err)
		}
		var campaignSelected, selected []string
		if err := readStrictFile(filepath.Join(root, campaignSelectedFile.Path), &campaignSelected); err != nil {
			return err
		}
		var plannedExperimentCells []string
		for _, cell := range planned.Cells {
			if strings.HasPrefix(cell, experimentID+"/") {
				plannedExperimentCells = append(plannedExperimentCells, cell)
			}
		}
		if !sameSortedStrings(campaignSelected, plannedExperimentCells) {
			return fmt.Errorf("experiment %s campaign selection differs from its profile partition", experimentID)
		}
		if err := readStrictFile(filepath.Join(root, selectedFile.Path), &selected); err != nil {
			return err
		}
		if experimentID == "rq5" {
			mapFile := files[publicationFileKey{"rq5_cell_map", "rq5"}]
			if mapFile.Path == "" {
				return errors.New("RQ5 publication record omits its coordinate map")
			}
			mapping, _, err := finalv5profile.LoadRQ5WorkloadCellMap(filepath.Join(root, mapFile.Path))
			if err != nil {
				return err
			}
			translated, err := mapping.TranslateCampaignCells(campaignSelected)
			if err != nil || !sameSortedStrings(translated, selected) {
				return errors.New("RQ5 publication selection differs from its source-controlled coordinate translation")
			}
		} else if !sameSortedStrings(campaignSelected, selected) {
			return fmt.Errorf("experiment %s runner selection differs from the campaign selection", experimentID)
		}
		records, err := ReadProfileCampaignSamples(filepath.Join(root, rawFile.Path))
		processes := config.ProcessReplicates
		if processes == 0 {
			processes = 1
		}
		expectedSamples := len(selected) * config.Samples * processes
		if err != nil || ValidateProfileCampaignExperimentGateForClass("publication", experimentID, selected, records, config.Samples*processes) != nil {
			return fmt.Errorf("experiment %s raw publication samples fail the terminal gate", experimentID)
		}
		var gate struct {
			SchemaVersion int    `json:"schema_version"`
			Status        string `json:"status"`
			ExperimentID  string `json:"experiment_id"`
			CampaignClass string `json:"campaign_class"`
			Samples       int    `json:"samples"`
			SelectedCells int    `json:"selected_cells"`
			InputSHA256   string `json:"input_sha256"`
		}
		if err := readStrictFile(filepath.Join(root, gateFile.Path), &gate); err != nil || gate.SchemaVersion != 1 ||
			gate.Status != "pass" || gate.ExperimentID != experimentID || gate.CampaignClass != "publication" ||
			gate.Samples != expectedSamples || gate.SelectedCells != len(selected) || gate.InputSHA256 != rawFile.SHA256 {
			return fmt.Errorf("experiment %s launcher gate does not bind its exact publication JSONL", experimentID)
		}
		deploymentID := fmt.Sprintf("deployment-%02d", repetition)
		expectedBinding := binding
		if experimentID == "rq5" {
			rq5BindingFile := files[publicationFileKey{"rq5_profile_binding", "rq5"}]
			if rq5BindingFile.Path == "" || readStrictFile(filepath.Join(root, rq5BindingFile.Path), &expectedBinding) != nil ||
				expectedBinding.Validate() != nil || expectedBinding.ProfileID != binding.ProfileID ||
				expectedBinding.DatasetBindingSHA256 != binding.DatasetBindingSHA256 {
				return errors.New("RQ5 publication binding is absent or differs from its profile execution")
			}
		}
		for _, envelope := range records {
			sample := envelope.Sample
			if sample.DeploymentID != deploymentID || sample.ProfileBinding == nil ||
				!sample.ProfileBinding.Equal(expectedBinding) {
				return fmt.Errorf("experiment %s sample differs from the fresh profile execution", experimentID)
			}
		}
	}
	return nil
}

func validatePublicationFinalizerMaterial(raw json.RawMessage, experiments []string) error {
	type dispatch struct {
		Experiments                    []string `json:"experiments"`
		AttestationQualificationPath   string   `json:"attestation_qualification_path"`
		AttestationQualificationSHA256 string   `json:"attestation_qualification_sha256"`
		PostgreSQLIdentityPath         string   `json:"postgresql_identity_path"`
		PostgreSQLIdentitySHA256       string   `json:"postgresql_identity_sha256"`
	}
	want := map[string]bool{}
	for _, experiment := range experiments {
		if experiment == "artifact" || experiment == "provsql" || experiment == "scale" {
			want[experiment] = true
		}
	}
	if len(want) == 0 {
		if len(raw) != 0 {
			return errors.New("ordinary publication profile carries finalizer material dispatch")
		}
		return nil
	}
	var values []dispatch
	if len(raw) == 0 || StrictJSON(raw, &values) != nil || len(values) != len(want) {
		return errors.New("publication profile finalizer material dispatch is absent or incomplete")
	}
	seen := map[string]bool{}
	for _, value := range values {
		if len(value.Experiments) != 1 || !want[value.Experiments[0]] || seen[value.Experiments[0]] {
			return errors.New("publication profile finalizer material dispatch is duplicated or misrouted")
		}
		seen[value.Experiments[0]] = true
		for _, file := range []struct{ path, digest string }{
			{value.AttestationQualificationPath, value.AttestationQualificationSHA256},
			{value.PostgreSQLIdentityPath, value.PostgreSQLIdentitySHA256},
		} {
			info, err := os.Lstat(file.path)
			actual, hashErr := FileSHA256(file.path)
			if file.path == "" || !filepath.IsAbs(file.path) || !validSHA256(file.digest) ||
				err != nil || hashErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || actual != file.digest {
				return errors.New("publication profile finalizer material is absent, unsafe, or changed")
			}
		}
	}
	return nil
}

func validatePublicationAdapterStderr(root string, stderrFile, scanFile finalv5profile.CampaignEvidenceFile) error {
	stderrPath := filepath.Join(root, stderrFile.Path)
	info, err := os.Lstat(stderrPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return errors.New("adapter stderr is absent, unsafe, or not mode 0600")
	}
	value, err := os.ReadFile(stderrPath)
	if err != nil {
		return err
	}
	var scan finalv5publication.AdapterStderrCredentialScan
	if err := readStrictFile(filepath.Join(root, scanFile.Path), &scan); err != nil {
		return fmt.Errorf("adapter stderr credential scan: %w", err)
	}
	if err := finalv5publication.ValidateAdapterStderrCredentialScan(value, scan); err != nil ||
		scan.InputSHA256 != stderrFile.SHA256 || scan.InputBytes != stderrFile.Bytes {
		return errors.New("adapter stderr credential scan is incomplete or differs from the retained stderr")
	}
	return nil
}

// publicationFormalBuildManifest is the strict wire copy of the formal Gateway
// build manifest (formalbuild.BuildManifest). Importing formalbuild would create
// a package cycle because formalbuild already uses experiment.GatewayRuntimeIdentityV1,
// so every field the builder writes must be mirrored here: readStrictFile
// rejects unknown fields, and a field added on the builder side without a mirror
// here refuses an otherwise valid publication campaign at its very last step
// (formal-v111-publication-03, 2026-08-29: module_proxy, added by d87fafe as a
// transport-only record, was missing).
type publicationFormalBuildManifest struct {
	SchemaVersion        int    `json:"schema_version"`
	SubmissionCommit     string `json:"submission_commit"`
	CleanTreeAtBuild     bool   `json:"clean_tree_at_build"`
	BuildContextSHA256   string `json:"build_context_sha256"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	ImageID              string `json:"image_id"`
	ImageTag             string `json:"image_tag"`
	Platform             string `json:"platform"`
	BuildTarget          string `json:"build_target"`
	BuilderBaseImage     string `json:"builder_base_image"`
	RuntimeBaseImage     string `json:"runtime_base_image"`
	DatasetBindingSHA256 string `json:"dataset_binding_sha256,omitempty"`
	ProfileRegistrySHA   string `json:"profile_registry_sha256,omitempty"`
	// ModuleProxy records the GOPROXY the builder was pointed at. It is
	// transport only (go.sum decides the bytes) and takes no part in validation.
	ModuleProxy string `json:"module_proxy,omitempty"`
}

func validatePublicationFormalGateway(root string, files map[publicationFileKey]finalv5profile.CampaignEvidenceFile,
	binding ProfileBinding, record finalv5profile.ProfileCampaignDeploymentRecord, rawSummary json.RawMessage) error {
	var manifest publicationFormalBuildManifest
	manifestFile := files[publicationFileKey{"formal_gateway_build_manifest", ""}]
	runtimeFile := files[publicationFileKey{"formal_gateway_runtime", ""}]
	overrideFile := files[publicationFileKey{"formal_gateway_compose_override", ""}]
	manifestPath := filepath.Join(root, manifestFile.Path)
	runtimePath := filepath.Join(root, runtimeFile.Path)
	overridePath := filepath.Join(root, overrideFile.Path)
	if err := readStrictFile(manifestPath, &manifest); err != nil || manifest.SchemaVersion != 1 ||
		manifest.SubmissionCommit != record.SubmissionCommit || !manifest.CleanTreeAtBuild ||
		manifest.BuildTarget != "gateway" || !validSHA256(manifest.BuildContextSHA256) ||
		!validSHA256(manifest.SourceManifestSHA256) || !validSHA256(manifest.ProfileRegistrySHA) ||
		manifest.DatasetBindingSHA256 != binding.DatasetBindingSHA256 || manifest.ImageID == "" {
		return errors.New("formal Gateway build manifest differs from the fixed publication inputs")
	}
	var runtime GatewayRuntimeIdentityV1
	if err := readStrictFile(runtimePath, &runtime); err != nil || runtime.Validate() != nil ||
		runtime.SubmissionCommit != record.SubmissionCommit || runtime.BuildContextSHA256 != manifest.BuildContextSHA256 ||
		runtime.SourceManifestSHA256 != manifest.SourceManifestSHA256 || runtime.LocalImageID != manifest.ImageID ||
		runtime.ContainerImageID != manifest.ImageID || runtime.BuilderBaseImage != manifest.BuilderBaseImage ||
		runtime.RuntimeBaseImage != manifest.RuntimeBaseImage {
		return errors.New("formal Gateway runtime identity differs from its build manifest")
	}
	var summary struct {
		ImageID               string `json:"image_id"`
		BuildManifestPath     string `json:"build_manifest_path"`
		BuildManifestSHA256   string `json:"build_manifest_sha256"`
		ComposeOverridePath   string `json:"compose_override_path"`
		ComposeOverrideSHA256 string `json:"compose_override_sha256"`
		RuntimeIdentityPath   string `json:"runtime_identity_path"`
		RuntimeIdentitySHA256 string `json:"runtime_identity_sha256"`
		BuildContextSHA256    string `json:"build_context_sha256"`
		SourceManifestSHA256  string `json:"source_manifest_sha256"`
		DatasetBindingSHA256  string `json:"dataset_binding_sha256"`
		ProfileRegistrySHA256 string `json:"profile_registry_sha256"`
	}
	if err := StrictJSON(rawSummary, &summary); err != nil || summary.ImageID != manifest.ImageID ||
		filepath.Clean(summary.BuildManifestPath) != filepath.Clean(manifestPath) ||
		summary.BuildManifestSHA256 != manifestFile.SHA256 ||
		filepath.Clean(summary.ComposeOverridePath) != filepath.Clean(overridePath) ||
		summary.ComposeOverrideSHA256 != overrideFile.SHA256 ||
		filepath.Clean(summary.RuntimeIdentityPath) != filepath.Clean(runtimePath) ||
		summary.RuntimeIdentitySHA256 != runtimeFile.SHA256 ||
		summary.BuildContextSHA256 != manifest.BuildContextSHA256 ||
		summary.SourceManifestSHA256 != manifest.SourceManifestSHA256 ||
		summary.DatasetBindingSHA256 != manifest.DatasetBindingSHA256 ||
		summary.ProfileRegistrySHA256 != manifest.ProfileRegistrySHA {
		return errors.New("fresh proof does not bind the formal Gateway build/runtime/input identities")
	}
	override, err := os.ReadFile(overridePath)
	if err != nil || string(override) != fmt.Sprintf("services:\n  gateway:\n    image: %q\n    pull_policy: never\n    build: !reset null\n", manifest.ImageID) {
		return errors.New("formal Gateway Compose override does not name the immutable manifest image")
	}
	return nil
}

func validateNonProfileRawFiles(runDir string, config Config, campaign finalv5profile.PlannedNonProfileCampaign) error {
	adapterDigestBytes, err := os.ReadFile(filepath.Join(runDir, "adapter.sha256"))
	if err != nil {
		return errors.New("deployment-free campaign omits its adapter digest")
	}
	adapterDigest := strings.TrimSpace(string(adapterDigestBytes))
	campaignRoot := filepath.Dir(filepath.Dir(runDir))
	actualAdapterDigest, err := FileSHA256(filepath.Join(campaignRoot, "source", "final-v5-adapter"))
	if err != nil || !validSHA256(adapterDigest) || actualAdapterDigest != adapterDigest {
		return errors.New("deployment-free campaign adapter digest differs from the sealed binary")
	}
	var build struct {
		SchemaVersion    int    `json:"schema_version"`
		SubmissionCommit string `json:"submission_commit"`
		BinarySHA256     string `json:"binary_sha256"`
		SourceSHA256     string `json:"source_sha256"`
		GoVersion        string `json:"go_version"`
		BuildCommand     string `json:"build_command"`
		SourceFiles      string `json:"source_files"`
	}
	if err := readStrictFile(filepath.Join(runDir, "adapter-build.json"), &build); err != nil ||
		build.SchemaVersion != 1 || build.SubmissionCommit != config.SubmissionCommit ||
		build.BinarySHA256 != adapterDigest || !validSHA256(build.SourceSHA256) ||
		strings.TrimSpace(build.GoVersion) == "" ||
		build.BuildCommand != "go build -buildvcs=false -trimpath -o final-v5-adapter ./evaluation/cmd/final-v5-adapter" ||
		strings.TrimSpace(build.SourceFiles) == "" {
		return errors.New("deployment-free campaign adapter build identity is invalid")
	}
	paths, err := filepath.Glob(filepath.Join(runDir, "raw", "execution-*.jsonl"))
	if err != nil || len(paths) != 3 {
		return fmt.Errorf("fresh execution files=%d, want 3", len(paths))
	}
	sort.Strings(paths)
	backendSystems := map[string]bool{}
	for index, path := range paths {
		wantName := fmt.Sprintf("execution-%02d.jsonl", index+1)
		if filepath.Base(path) != wantName {
			return fmt.Errorf("fresh execution file %q is not %q", filepath.Base(path), wantName)
		}
		samples, err := ReadSamples([]string{path})
		if err != nil || len(samples) == 0 {
			return fmt.Errorf("fresh execution %d is unreadable or empty", index+1)
		}
		wantDeployment := fmt.Sprintf("deployment-%02d", index+1)
		for _, sample := range samples {
			if sample.DeploymentID != wantDeployment || sample.ProfileBinding != nil || !sample.PublicationEligible {
				return fmt.Errorf("fresh execution %d carries a deployment/ProfileBinding fiction", index+1)
			}
		}
		var proof struct {
			SchemaVersion           int    `json:"schema_version"`
			Record                  string `json:"record"`
			Status                  string `json:"status"`
			CampaignClass           string `json:"campaign_class"`
			PublicationEligible     bool   `json:"publication_eligible"`
			FormalCampaign          bool   `json:"formal_campaign"`
			CampaignID              string `json:"campaign_id"`
			SubmissionCommit        string `json:"submission_commit"`
			Group                   string `json:"group"`
			ExecutionID             string `json:"execution_id"`
			Repetition              int    `json:"repetition"`
			ExecutionModel          string `json:"execution_model"`
			FreshRunnerProcess      bool   `json:"fresh_runner_process"`
			FreshAdapterProcess     bool   `json:"fresh_adapter_process"`
			StateInheritance        bool   `json:"state_inheritance"`
			ProfileBinding          string `json:"profile_binding"`
			AdapterSHA256           string `json:"adapter_sha256"`
			BackendProcess          string `json:"backend_process"`
			BackendImage            string `json:"backend_image"`
			BackendSystemIdentifier string `json:"backend_system_identifier"`
			BackendCleanup          bool   `json:"backend_cleanup"`
		}
		proofPath := filepath.Join(runDir, fmt.Sprintf("execution-%02d.json", index+1))
		if err := readStrictFile(proofPath, &proof); err != nil || proof.SchemaVersion != 1 ||
			proof.Record != "taskgate-final-v5-non-profile-execution-v1" || proof.Status != "pass" ||
			proof.CampaignClass != "publication" || !proof.PublicationEligible || !proof.FormalCampaign ||
			proof.CampaignID != config.CampaignID || proof.SubmissionCommit != config.SubmissionCommit ||
			proof.Group != campaign.ID || proof.ExecutionID != fmt.Sprintf("execution-%02d", index+1) ||
			proof.Repetition != index+1 || proof.ExecutionModel != "deployment_free_process" ||
			!proof.FreshRunnerProcess || !proof.FreshAdapterProcess || proof.StateInheritance ||
			proof.ProfileBinding != "forbidden" || proof.AdapterSHA256 != adapterDigest || !proof.BackendCleanup {
			return fmt.Errorf("fresh execution proof %d is invalid", index+1)
		}
		if campaign.ID == "scale-outcome-merkle" || campaign.ID == "compiler" {
			if proof.BackendProcess != "fresh_postgresql_process" ||
				proof.BackendImage != "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55" ||
				proof.BackendSystemIdentifier == "" || backendSystems[proof.BackendSystemIdentifier] {
				return fmt.Errorf("fresh PostgreSQL backend proof %d is reused or invalid", index+1)
			}
			backendSystems[proof.BackendSystemIdentifier] = true
		} else if proof.BackendProcess != "none" || proof.BackendImage != "" || proof.BackendSystemIdentifier != "" {
			return fmt.Errorf("backend-free execution %d fabricates a backend process", index+1)
		}
	}
	return nil
}

func readStrictFile(path string, target any) error {
	payload, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return StrictJSON(payload, target)
}

func sameSortedStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	one, two := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(one)
	sort.Strings(two)
	for index := range one {
		if one[index] != two[index] {
			return false
		}
	}
	return true
}
