package finalv5publication

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

// CurrentDatasetBindingOptions selects only owner-controlled output paths. The
// Catalog, Contract, oracle models, manifest closure, and SQL are fixed source
// inputs; credentials continue to arrive only through the established DSN
// environment used by the command wrapper.
type CurrentDatasetBindingOptions struct {
	RepositoryRoot  string
	OutputDirectory string
	ArtifactRoot    string
	BusinessDSN     string
}

// CurrentDatasetBindingSummary is the credential-free receipt for one
// create-exclusive private binding. It is verification material, not an
// author approval and not campaign evidence.
type CurrentDatasetBindingSummary struct {
	Version                    string `json:"version"`
	Status                     string `json:"status"`
	AuthorApproved             bool   `json:"author_approved"`
	OutputDirectory            string `json:"output_directory"`
	BindingPath                string `json:"binding_path"`
	BindingSHA256              string `json:"binding_sha256"`
	BindingBytes               int64  `json:"binding_bytes"`
	AdapterSectionSHA256       string `json:"adapter_section_sha256"`
	CatalogSHA256              string `json:"catalog_sha256"`
	DatasetSHA256              string `json:"dataset_sha256"`
	DatasetProbeSHA256         string `json:"dataset_probe_sha256"`
	LiveObservationSHA256      string `json:"live_observation_sha256"`
	PostgreSQLServerVersionNum string `json:"postgresql_server_version_num"`
	PreparedStatementsBefore   int64  `json:"prepared_statements_before"`
	PreparedStatementsAfter    int64  `json:"prepared_statements_after"`
	LiveQueryCount             int    `json:"live_query_count"`
	ScaleCells                 int    `json:"scale_cells"`
	ArtifactCells              int    `json:"artifact_cells"`
	ProvSQLCells               int    `json:"provsql_cells"`
}

const currentDatasetBindingVersion = "taskgate-final-v5-current-dataset-binding-generation-v1"

var currentDatasetBindingOutputNames = []string{BindingOutputName}

// GenerateCurrentDatasetBinding constructs the exact 12/6/105 private
// binding against the source-controlled master Catalog. The sequence fixes
// the independent Outcome expectations before any live observation, then uses
// live PostgreSQL only as the actual comparison operand. It accepts no SQL and
// delegates every expected Result/Dependency/Outcome derivation to the same
// fixed oracle and BuildCompleteBinding implementations used by P4.0-E1.
func GenerateCurrentDatasetBinding(ctx context.Context,
	options CurrentDatasetBindingOptions) (CurrentDatasetBindingSummary, error) {
	var summary CurrentDatasetBindingSummary
	if ctx == nil {
		return summary, errors.New("current Dataset Binding generation requires a context")
	}
	root, err := cleanRepositoryRoot(options.RepositoryRoot)
	if err != nil {
		return summary, err
	}
	if strings.TrimSpace(options.BusinessDSN) == "" {
		return summary, errors.New("current Dataset Binding generation requires Business PostgreSQL DSN")
	}
	if err := requireAbsentOutput(options.OutputDirectory); err != nil {
		return summary, err
	}
	artifactRoot, err := requirePrivateArtifactDirectory(options.ArtifactRoot)
	if err != nil {
		return summary, err
	}
	materials, err := loadGenerationMaterials(root)
	if err != nil {
		return summary, err
	}
	currentCatalog, err := catalog.Parse(materials.baseCatalogBytes)
	if err != nil {
		return summary, fmt.Errorf("parse current source-controlled Catalog: %w", err)
	}
	if currentCatalog.SHA256 != materials.baseCatalog.SHA256 || len(currentCatalog.Sources) != 1 {
		return summary, errors.New("current source-controlled Catalog identity or datasource closure is invalid")
	}
	oracleOptions := finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: publicationOracleMaxInMemoryMembers,
		TempDir:            artifactRoot,
	}
	scaleOutcomes, err := finalv5binding.GenerateScaleOutcomeCandidateExpectations(
		currentCatalog.SHA256, oracleOptions)
	if err != nil {
		return summary, fmt.Errorf("generate pre-observation current-Catalog Scale Outcome expectations: %w", err)
	}
	catalogAttestation, err := AttestCatalogModel(ctx, options.BusinessDSN, currentCatalog)
	if err != nil {
		return summary, err
	}
	if catalogAttestation.Datasource.SchemaDigest != currentCatalog.Sources[0].SchemaDigest {
		return summary, fmt.Errorf("current source-controlled Catalog schema digest %s differs from live PostgreSQL %s",
			currentCatalog.Sources[0].SchemaDigest, catalogAttestation.Datasource.SchemaDigest)
	}
	datasetAgreement, err := finalv5dataset.VerifyBenchmarkPostgreSQL(ctx, options.BusinessDSN)
	if err != nil {
		return summary, fmt.Errorf("verify complete live benchmark Dataset: %w", err)
	}
	live, err := ObservePublicationClosure(ctx, options.BusinessDSN, materials.runtime,
		materials.scaleManifests, materials.provSQLManifests)
	if err != nil {
		return summary, err
	}
	if live.Database != catalogAttestation.Datasource.Database || live.User != catalogAttestation.Datasource.User ||
		catalogAttestation.Datasource.PostgreSQLMajorVersion != 16 ||
		live.PostgreSQLServerVersionNum != "160014" {
		return summary, errors.New("live query observation and current Catalog do not identify PostgreSQL 16.14")
	}
	datasetIdentity, err := materials.runtime.DatasetIdentitySHA256()
	if err != nil || datasetAgreement.Observed.SHA256 != datasetIdentity ||
		datasetAgreement.Reference.SHA256 != datasetIdentity {
		return summary, errors.New("live Dataset agreement differs from the Contract-reviewed Dataset identity")
	}
	binding, bindingBytes, err := finalv5binding.BuildCompleteBinding(finalv5binding.CompleteBindingInput{
		DatasetSHA256: datasetIdentity, DatasetProbeSHA256: live.DatasetProbe.ResultSHA256,
		Catalog: currentCatalog, ScaleOutcomes: scaleOutcomes,
		ScaleManifests: materials.scaleManifests, ArtifactRuntime: materials.runtime,
		ProvSQLManifests: materials.provSQLManifests,
		OracleOptions:    oracleOptions,
	})
	if err != nil {
		return summary, err
	}
	files := map[string][]byte{BindingOutputName: bindingBytes}
	sensitive, err := sensitiveDSNValues(options.BusinessDSN)
	if err != nil {
		return summary, err
	}
	if err := ValidateCredentialFree(files, sensitive); err != nil {
		return summary, err
	}
	if err := WriteClosedOutputDirectory(options.OutputDirectory, files, currentDatasetBindingOutputNames); err != nil {
		return summary, err
	}
	loaded, err := finalv5binding.LoadFile(filepath.Join(options.OutputDirectory, BindingOutputName))
	if err != nil || loaded.FileSHA256 != binding.FileSHA256 || loaded.SectionSHA256 != binding.SectionSHA256 {
		return summary, errors.New("fresh current Dataset Binding changed after create-exclusive write")
	}
	if err := finalv5binding.ValidateAgainstCatalog(loaded, currentCatalog); err != nil {
		return summary, fmt.Errorf("validate fresh binding against current Catalog: %w", err)
	}
	return CurrentDatasetBindingSummary{
		Version: currentDatasetBindingVersion, Status: ReviewStatus, AuthorApproved: false,
		OutputDirectory: options.OutputDirectory,
		BindingPath:     filepath.Join(options.OutputDirectory, BindingOutputName),
		BindingSHA256:   binding.FileSHA256, BindingBytes: int64(len(bindingBytes)),
		AdapterSectionSHA256: binding.SectionSHA256, CatalogSHA256: currentCatalog.SHA256,
		DatasetSHA256: binding.DatasetSHA256, DatasetProbeSHA256: binding.DatasetProbeSHA256,
		LiveObservationSHA256:      live.ObservationSHA256,
		PostgreSQLServerVersionNum: live.PostgreSQLServerVersionNum,
		PreparedStatementsBefore:   live.PreparedStatementsBefore,
		PreparedStatementsAfter:    live.PreparedStatementsAfter,
		LiveQueryCount:             len(live.Queries), ScaleCells: len(binding.Section.Scale.DependencyE2E),
		ArtifactCells: len(binding.Section.Artifact.ResultHeavy),
		ProvSQLCells:  len(binding.Section.ProvSQL.TaskGate),
	}, nil
}
