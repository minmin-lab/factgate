package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

var experimentIDs = [...]string{"baseline", "scale", "artifact", "rls", "attack", "provsql", "compiler", "concurrency", "rq5"}

// adapterSampleProfileBinder is resolved once at process startup. Artifact and
// Concurrency call it inside their execution path where Receipt evidence would
// otherwise be transformed before the common output gate sees it.
var adapterSampleProfileBinder *experiment.SampleProfileBinder

type sourceControlledAdapter interface {
	Execute(context.Context, experiment.AdapterOperation) experiment.Sample
	Close()
}

type adapterFactory func(context.Context) (sourceControlledAdapter, error)

// adapterFactories is the source-controlled execution registry. A constructor
// is necessary but not always sufficient for a formal capability: adapters
// that also serve a narrower Pilot must pass their publication profile gate.
var adapterFactories = map[string]adapterFactory{
	"baseline":    func(ctx context.Context) (sourceControlledAdapter, error) { return newBaselineAdapter(ctx) },
	"scale":       func(ctx context.Context) (sourceControlledAdapter, error) { return newScaleAdapter(ctx) },
	"artifact":    func(ctx context.Context) (sourceControlledAdapter, error) { return newArtifactAdapter(ctx) },
	"rls":         func(ctx context.Context) (sourceControlledAdapter, error) { return newRLSAdapter(ctx) },
	"footprint":   func(ctx context.Context) (sourceControlledAdapter, error) { return newFootprintAdapter(ctx) },
	"benign":      func(ctx context.Context) (sourceControlledAdapter, error) { return newBenignAdapter(ctx) },
	"scale7":      func(ctx context.Context) (sourceControlledAdapter, error) { return newScale7Adapter(ctx) },
	"compare7":    func(ctx context.Context) (sourceControlledAdapter, error) { return newCompare7Adapter(ctx) },
	"counter":     func(ctx context.Context) (sourceControlledAdapter, error) { return newCounterAdapter(ctx) },
	"adversary":   func(ctx context.Context) (sourceControlledAdapter, error) { return newAdversaryAdapter(ctx) },
	"attack":      func(ctx context.Context) (sourceControlledAdapter, error) { return newAttackAdapter(ctx) },
	"provsql":     func(ctx context.Context) (sourceControlledAdapter, error) { return newProvSQLAdapter(ctx) },
	"compiler":    func(ctx context.Context) (sourceControlledAdapter, error) { return newCompilerAdapter(ctx) },
	"concurrency": func(ctx context.Context) (sourceControlledAdapter, error) { return newConcurrencyAdapter(ctx) },
	"rq5":         func(ctx context.Context) (sourceControlledAdapter, error) { return newRQ5Adapter(ctx) },
}

func implementedCapabilities() map[string]bool {
	result := make(map[string]bool, len(experimentIDs))
	for _, experimentID := range experimentIDs {
		factory, registered := adapterFactories[experimentID]
		result[experimentID] = registered && factory != nil && publicationCoverageGateSatisfied(experimentID)
	}
	return result
}

func initializeAdapter(ctx context.Context, experimentID string) (sourceControlledAdapter, string) {
	factory, registered := adapterFactories[experimentID]
	if !registered || factory == nil {
		return nil, "source_controlled_experiment_not_implemented"
	}
	adapter, err := factory(ctx)
	if err != nil {
		writeAdapterInitializationDiagnostic(experimentID, err)
		return nil, "adapter_environment_invalid"
	}
	if nilSourceControlledAdapter(adapter) {
		return nil, "adapter_environment_invalid"
	}
	return adapter, ""
}

func nilSourceControlledAdapter(adapter sourceControlledAdapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func bindAdapterOutputSample(operation experiment.AdapterOperation, sample experiment.Sample) experiment.Sample {
	if adapterSampleProfileBinder == nil {
		return sample
	}
	if err := adapterSampleProfileBinder.BindSample(&sample); err != nil {
		fmt.Fprintf(os.Stderr, "sample %s profile binding: %v\n", operation.SampleID, err)
		sample.Status = "fail"
		sample.ErrorCode = "profile_binding_catalog_mismatch"
		sample.Reason = "an observed Gateway Receipt Catalog differs from the independently resolved deployment profile"
	}
	return sample
}

func main() {
	experimentID := flag.String("experiment", "", "source-controlled experiment implementation")
	capabilities := flag.Bool("capabilities", false, "print implemented experiment capabilities")
	validateBinding := flag.Bool("validate-binding", false, "strictly validate the complete private deployment binding")
	validateObserver := flag.Bool("validate-observer-runtime", false, "validate the frozen observer executable and build manifest")
	agentPilot := flag.Bool("agent-pilot", false, "P10.B2: run the LLM-agent pilot against the live deployment (outside the campaign plan)")
	agentOut := flag.String("agent-out", "", "agent pilot JSONL output path")
	agentDeployment := flag.String("agent-deployment", "deployment-01", "agent pilot deployment id")
	agentArms := flag.String("agent-arms", "rls,factgate", "agent pilot arms")
	agentObjectiveIDs := flag.String("agent-objectives", "benign,probe", "agent pilot objectives")
	agentSamples := flag.Int("agent-samples", 3, "agent pilot samples per arm and objective")
	flag.Parse()
	if *agentPilot {
		if *agentOut == "" {
			fmt.Fprintln(os.Stderr, "-agent-out is required")
			os.Exit(2)
		}
		if err := runAgentPilot(context.Background(), *agentOut, *agentDeployment,
			strings.Split(*agentArms, ","), strings.Split(*agentObjectiveIDs, ","), *agentSamples); err != nil {
			fmt.Fprintln(os.Stderr, "agent-pilot:", err)
			os.Exit(1)
		}
		return
	}
	selectedModes := 0
	for _, selected := range []bool{*capabilities, *validateBinding, *validateObserver} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 || (selectedModes > 0 && *experimentID != "") {
		fmt.Fprintln(os.Stderr, "adapter inspection modes are mutually exclusive")
		os.Exit(2)
	}
	if *capabilities {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "adapter": "final-v5-adapter", "experiments": implementedCapabilities()})
		return
	}
	if *validateBinding {
		report, err := validateAdapterBindingInput()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return
	}
	if *validateObserver {
		if _, err := validateObserverRuntimeBinding(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "status": "valid"})
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	var profileBindingCode string
	{
		var err error
		if *experimentID == "rq5" {
			adapterSampleProfileBinder, err = experiment.ResolveRQ5SampleProfileBinderFromEnvironment()
		} else {
			adapterSampleProfileBinder, err = experiment.ResolveSampleProfileBinderFromEnvironment()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			profileBindingCode = "adapter_profile_binding_invalid"
		}
	}
	var adapter sourceControlledAdapter
	initializationCode := profileBindingCode
	if initializationCode == "" {
		adapter, initializationCode = initializeAdapter(context.Background(), *experimentID)
	}
	if adapter != nil {
		defer adapter.Close()
	}
	for scanner.Scan() {
		var operation experiment.AdapterOperation
		if experiment.StrictJSON(scanner.Bytes(), &operation) != nil {
			os.Exit(1)
		}
		var sample experiment.Sample
		if initializationCode != "" || operation.ExperimentID != *experimentID {
			code := initializationCode
			if code == "" {
				code = "experiment_identity_mismatch"
			}
			sample = invalidSample(operation, code)
		} else {
			operationContext := withTaskMigrationOperation(context.Background(), operation)
			sample = adapter.Execute(operationContext, operation)
		}
		sample = bindAdapterOutputSample(operation, sample)
		if encoder.Encode(sample) != nil {
			os.Exit(1)
		}
	}
	if scanner.Err() != nil {
		os.Exit(1)
	}
}
