// Command kb-formal-finalize closes a knowledge-product round
// (docs/kb_experiment_design.md, 正式轮（方案 C）规程 §7): it verifies every
// deployment record of the round, locally and on the NAS, file by file;
// checks each driver report against the profile registry; combines the
// reports into one report per experiment, in the shape of the registered pilot
// reports (kbformal/combine.go); writes them and campaign-evidence.json,
// create-exclusive, under <root>/evidence/; and mirrors evidence/ to the NAS
// with per-file verification.
//
// A formal round needs, for each of the 12 profile and repetition slots,
// exactly one attempt, complete, formal, at one commit equal to the commit its
// binaries were built from. A pilot round takes each slot's latest complete
// attempt.
//
//	kb-formal-finalize -root ROOT -class formal|pilot [-nas-root DIR]
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
)

// The pre-registered fixed design (docs/kb_experiment_design.md): three
// repetitions, E-KB1 with three seeds of at most 400 steps, E-KB2(b) with a
// 120 s TTL, E-KB3 with 200 rounds per cell.
const (
	repetitions = 3
	ekb1Seeds   = 3
	ekb1Steps   = 400
	ekb2bTTL    = 120
	ekb3Rounds  = 200
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-finalize:", err)
		os.Exit(1)
	}
}

type record struct {
	Version        string                 `json:"version"`
	Campaign       string                 `json:"campaign"`
	Profile        string                 `json:"profile"`
	Repetition     int                    `json:"repetition"`
	Attempt        int                    `json:"attempt"`
	BinariesCommit string                 `json:"binaries_commit"`
	Class          string                 `json:"class"`
	Outcome        string                 `json:"outcome"`
	Commit         string                 `json:"commit"`
	Files          []kbformal.BindingFile `json:"files"`
}

type report struct {
	Version       string          `json:"version"`
	Experiment    string          `json:"experiment"`
	Label         string          `json:"label"`
	Tier          string          `json:"tier"`
	Deployment    int             `json:"deployment"`
	CatalogSHA256 string          `json:"catalog_sha256"`
	Epoch         string          `json:"epoch"`
	StartedAt     string          `json:"started_at"`
	Seconds       float64         `json:"seconds"`
	Result        json.RawMessage `json:"result"`
}

type deployment struct {
	Profile    string `json:"profile"`
	Repetition int    `json:"repetition"`
	Attempt    int    `json:"attempt"`
	Record     string `json:"record"`
	SHA256     string `json:"record_sha256"`
	NAS        string `json:"nas"`
	dir        string
}

func fileDigest(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// verifyFiles requires every listed file under base to have its recorded
// digest and size.
func verifyFiles(base string, files []kbformal.BindingFile) error {
	for _, file := range files {
		digest, size, err := fileDigest(filepath.Join(base, file.Path))
		if err != nil {
			return err
		}
		if digest != file.SHA256 || size != file.Bytes {
			return fmt.Errorf("%s: %s does not match its record", base, file.Path)
		}
	}
	return nil
}

// chooseAttempt returns the attempt directory a round's finalization uses
// for one slot.
func chooseAttempt(slot, class string) (int, error) {
	entries, err := os.ReadDir(slot)
	if err != nil {
		return 0, err
	}
	var attempts []int
	for _, entry := range entries {
		if n, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "attempt-")); err == nil && entry.IsDir() && strings.HasPrefix(entry.Name(), "attempt-") {
			attempts = append(attempts, n)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(attempts)))
	if class == "formal" {
		if len(attempts) != 1 || attempts[0] != 1 {
			return 0, fmt.Errorf("%s: a formal round has exactly one attempt per slot, found %v", slot, attempts)
		}
		return 1, nil
	}
	for _, n := range attempts {
		var r record
		data, err := os.ReadFile(filepath.Join(slot, fmt.Sprintf("attempt-%d", n), "deployment-record.json"))
		if err == nil && json.Unmarshal(data, &r) == nil && r.Outcome == "complete" {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%s has no complete attempt", slot)
}

func writeExclusive(path string, value any) (kbformal.BindingFile, error) {
	encoded, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return kbformal.BindingFile{}, err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return kbformal.BindingFile{}, err
	}
	if _, err := file.Write(encoded); err != nil {
		file.Close()
		return kbformal.BindingFile{}, err
	}
	if err := file.Close(); err != nil {
		return kbformal.BindingFile{}, err
	}
	sum := sha256.Sum256(encoded)
	return kbformal.BindingFile{Path: filepath.Base(path), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(encoded))}, nil
}

func run() error {
	rootFlag := flag.String("root", "", "round root (holds deployments/)")
	class := flag.String("class", "", "formal | pilot")
	nasRoot := flag.String("nas-root", "/mnt/ctg-pto-dev/factgate-evidence", "NAS evidence root")
	repo := flag.String("repo", ".", "repository root (profiles, publications, query set)")
	flag.Parse()
	if *rootFlag == "" || (*class != "formal" && *class != "pilot") {
		return errors.New("-root and -class formal|pilot are required")
	}
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return err
	}
	campaign := filepath.Base(root)
	registryData, err := os.ReadFile(filepath.Join(kbformal.ProfilesDir(*repo), "registry.json"))
	if err != nil {
		return err
	}
	var registry struct {
		Profiles []kbformal.RegistryEntry `json:"profiles"`
	}
	if err := json.Unmarshal(registryData, &registry); err != nil {
		return err
	}
	reports := map[string][]report{} // experiment name in the registry -> repetitions 1..3
	entries := map[string]kbformal.RegistryEntry{}
	var deployments []deployment
	commit := ""
	for _, entry := range registry.Profiles {
		entries[entry.Name] = entry
		for rep := 1; rep <= repetitions; rep++ {
			slot := filepath.Join(root, "deployments", entry.Name, fmt.Sprintf("%03d", rep))
			attempt, err := chooseAttempt(slot, *class)
			if err != nil {
				return err
			}
			dir := filepath.Join(slot, fmt.Sprintf("attempt-%d", attempt))
			recordPath := filepath.Join(dir, "deployment-record.json")
			recordBytes, err := os.ReadFile(recordPath)
			if err != nil {
				return err
			}
			var r record
			if err := json.Unmarshal(recordBytes, &r); err != nil {
				return fmt.Errorf("%s: %w", recordPath, err)
			}
			switch {
			case r.Version != "taskgate-kb-formal-deployment-record-v1" || r.Campaign != campaign || r.Profile != entry.Name || r.Repetition != rep || r.Attempt != attempt:
				return fmt.Errorf("%s does not describe %s/%s/%d attempt %d", recordPath, campaign, entry.Name, rep, attempt)
			case r.Class != *class || r.Outcome != "complete":
				return fmt.Errorf("%s: class %s outcome %s", recordPath, r.Class, r.Outcome)
			case commit != "" && r.Commit != commit:
				return fmt.Errorf("%s ran commit %s, other deployments %s", recordPath, r.Commit, commit)
			case *class == "formal" && r.BinariesCommit != r.Commit:
				return fmt.Errorf("%s: binaries from %s, scripts at %s", recordPath, r.BinariesCommit, r.Commit)
			}
			commit = r.Commit
			nas := filepath.Join(*nasRoot, "kb-formal", campaign, "deployments", entry.Name, fmt.Sprintf("%03d", rep), fmt.Sprintf("attempt-%d", attempt))
			if err := verifyFiles(dir, r.Files); err != nil {
				return err
			}
			if err := verifyFiles(nas, r.Files); err != nil {
				return fmt.Errorf("NAS copy: %w", err)
			}
			if nasRecord, err := os.ReadFile(filepath.Join(nas, "deployment-record.json")); err != nil || !bytes.Equal(nasRecord, recordBytes) {
				return fmt.Errorf("%s: the NAS record differs", recordPath)
			}
			sum := sha256.Sum256(recordBytes)
			relative, _ := filepath.Rel(root, recordPath)
			deployments = append(deployments, deployment{Profile: entry.Name, Repetition: rep, Attempt: attempt, Record: relative, SHA256: hex.EncodeToString(sum[:]), NAS: nas, dir: dir})
			for _, experiment := range entry.Experiments {
				var rp report
				data, err := os.ReadFile(filepath.Join(dir, "reports", experiment+".json"))
				if err != nil {
					return err
				}
				if err := json.Unmarshal(data, &rp); err != nil {
					return err
				}
				name, qualifier, _ := strings.Cut(experiment, "-")
				switch {
				case rp.Version != "taskgate-kb-formal-deployment-report-v1" || rp.Experiment != name || rp.Deployment != rep:
					return fmt.Errorf("%s/%s: report is %s deployment %d", dir, experiment, rp.Experiment, rp.Deployment)
				case name == "ekb5" && rp.Label != qualifier, name == "ekb3" && rp.Tier != qualifier:
					return fmt.Errorf("%s/%s: report label %q tier %q", dir, experiment, rp.Label, rp.Tier)
				case rp.CatalogSHA256 != entry.CatalogSHA256 || rp.Epoch != entry.Epoch:
					return fmt.Errorf("%s/%s: report names Catalog %s epoch %s", dir, experiment, rp.CatalogSHA256, rp.Epoch)
				}
				reports[experiment] = append(reports[experiment], rp)
			}
		}
	}
	decode := func(experiment string, into func(json.RawMessage) error) error {
		for _, rp := range reports[experiment] {
			if err := into(rp.Result); err != nil {
				return fmt.Errorf("%s deployment %d: %w", experiment, rp.Deployment, err)
			}
		}
		return nil
	}
	seconds := func(experiment string) float64 {
		total := 0.0
		for _, rp := range reports[experiment] {
			total += rp.Seconds
		}
		return total
	}
	campaignClass := map[string]string{"formal": "kb-publication", "pilot": "kb-pilot"}[*class]
	generated := time.Now().UTC().Format(time.RFC3339)
	meta := func(profile string) kbformal.CombineMeta {
		return kbformal.CombineMeta{CampaignClass: campaignClass, PublicationEligible: *class == "formal", GeneratedAt: generated, Epoch: entries[profile].Epoch}
	}
	querysetDir := filepath.Join(*repo, "evaluation/kb-pilot/queryset-v2")
	queries, _, err := kbformal.LoadQueryset(querysetDir)
	if err != nil {
		return err
	}
	var querysetManifest map[string]any
	if data, err := os.ReadFile(filepath.Join(querysetDir, "manifest.json")); err != nil {
		return err
	} else if err := json.Unmarshal(data, &querysetManifest); err != nil {
		return err
	}
	combined := map[string]any{}

	var ekb1 []kbformal.EKB1Deployment
	if err := decode("ekb1", func(raw json.RawMessage) error {
		var detail kbformal.EKB1Deployment
		if err := json.Unmarshal(raw, &detail); err != nil {
			return err
		}
		seeds := map[int]bool{}
		for _, trajectory := range detail.Trajectories {
			seeds[trajectory.Seed] = true
			if trajectory.Steps > ekb1Steps {
				return fmt.Errorf("a trajectory ran %d steps, more than %d", trajectory.Steps, ekb1Steps)
			}
		}
		if len(seeds) != ekb1Seeds {
			return fmt.Errorf("%d seeds, want %d", len(seeds), ekb1Seeds)
		}
		ekb1 = append(ekb1, detail)
		return nil
	}); err != nil {
		return err
	}
	combined["ekb1"] = kbformal.CombineEKB1(meta("kb-v2-pilot"), ekb1, querysetManifest, kbformal.PilotBudget, ekb1Seeds, ekb1Steps, seconds("ekb1"))

	var ekb2a []kbformal.EKB2Deployment
	if err := decode("ekb2a", func(raw json.RawMessage) error {
		var detail kbformal.EKB2Deployment
		ekb2a = append(ekb2a, detail)
		return json.Unmarshal(raw, &ekb2a[len(ekb2a)-1])
	}); err != nil {
		return err
	}
	catalogText, err := os.ReadFile(filepath.Join(kbformal.ProfilesDir(*repo), entries["kb-v2-pilot"].Catalog))
	if err != nil {
		return err
	}
	combined["ekb2a"] = kbformal.CombineEKB2a(meta("kb-v2-pilot"), ekb2a, entries["kb-v2-pilot"].AttributesSHA256, kbformal.EligibilityBlock(string(catalogText)), seconds("ekb2a"))

	var arms []kbformal.EKB2bArm
	if err := decode("ekb2b", func(raw json.RawMessage) error {
		var arm kbformal.EKB2bArm
		arms = append(arms, arm)
		return json.Unmarshal(raw, &arms[len(arms)-1])
	}); err != nil {
		return err
	}
	combined["ekb2b"] = kbformal.CombineEKB2b(meta("kb-v2-pilot"), arms, kbformal.PilotBudget, ekb2bTTL)

	for _, tier := range []struct{ experiment, profile string }{{"ekb3-48k", "kb-v2-large"}, {"ekb3-96k", "kb-20k-large"}} {
		var details []kbformal.EKB3Deployment
		if err := decode(tier.experiment, func(raw json.RawMessage) error {
			var detail kbformal.EKB3Deployment
			if err := json.Unmarshal(raw, &detail); err != nil {
				return err
			}
			if detail.Rounds != ekb3Rounds {
				return fmt.Errorf("%d rounds, want %d", detail.Rounds, ekb3Rounds)
			}
			details = append(details, detail)
			return nil
		}); err != nil {
			return err
		}
		report, err := kbformal.CombineEKB3(meta(tier.profile), details, reports[tier.experiment][0].StartedAt)
		if err != nil {
			return err
		}
		combined[tier.experiment] = report
	}

	var ekb4 []kbformal.EKB4Deployment
	if err := decode("ekb4", func(raw json.RawMessage) error {
		var detail kbformal.EKB4Deployment
		ekb4 = append(ekb4, detail)
		return json.Unmarshal(raw, &ekb4[len(ekb4)-1])
	}); err != nil {
		return err
	}
	combined["ekb4"] = kbformal.CombineEKB4(meta("kb-v2-large"), ekb4, querysetManifest, len(queries), entries["kb-v2-large"].CatalogSHA256)

	var observations []kbformal.EKB5Observation
	for _, experiment := range []string{"ekb5-document", "ekb5-collection"} {
		if err := decode(experiment, func(raw json.RawMessage) error {
			var observation kbformal.EKB5Observation
			observations = append(observations, observation)
			return json.Unmarshal(raw, &observations[len(observations)-1])
		}); err != nil {
			return err
		}
	}
	combined["ekb5"] = kbformal.CombineEKB5(meta("kb-v2-pilot"), observations, entries["kb-v2-pilot"].Epoch, entries["kb-v2c-pilot"].Epoch, repetitions)

	binding, err := exec.Command("git", "-C", *repo, "show", commit+":"+kbformal.BindingPath).Output()
	if err != nil {
		return fmt.Errorf("binding at %s: %w", commit, err)
	}
	bindingSum := sha256.Sum256(binding)
	evidence := filepath.Join(root, "evidence")
	if err := os.Mkdir(evidence, 0o755); err != nil {
		return err
	}
	var files []kbformal.BindingFile
	names := make([]string, 0, len(combined))
	for name := range combined {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := writeExclusive(filepath.Join(evidence, name+".json"), combined[name])
		if err != nil {
			return err
		}
		files = append(files, file)
	}
	campaignEvidence := map[string]any{"version": "taskgate-kb-campaign-evidence-v1", "campaign": campaign, "class": *class, "campaign_class": campaignClass,
		"publication_eligible": *class == "formal", "commit": commit, "dataset_binding_sha256": hex.EncodeToString(bindingSum[:]),
		"profile_registry_sha256": func() string { s := sha256.Sum256(registryData); return hex.EncodeToString(s[:]) }(),
		"deployments":             deployments, "evidence": files, "generated_at": generated}
	summary, err := writeExclusive(filepath.Join(evidence, "campaign-evidence.json"), campaignEvidence)
	if err != nil {
		return err
	}
	files = append(files, summary)
	target := filepath.Join(*nasRoot, "kb-formal", campaign, "evidence")
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(evidence, file.Path))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(target, file.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := out.Write(data); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	if err := verifyFiles(target, files); err != nil {
		return fmt.Errorf("NAS evidence: %w", err)
	}
	fmt.Printf("campaign %s (%s): %d deployments verified locally and on the NAS; evidence %s sha256=%s mirrored to %s\n",
		campaign, *class, len(deployments), filepath.Join(evidence, "campaign-evidence.json"), summary.SHA256, target)
	return nil
}
