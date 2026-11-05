// Command kb-formal-register registers the sealed evidence of a finalized
// formal knowledge-product round (docs/kb_experiment_design.md, 正式轮（方案 C）
// 规程 §7). It requires <root>/evidence/campaign-evidence.json of a formal
// round (campaign_class kb-publication, publication_eligible true), verifies
// every evidence file it lists against its digest locally and on the NAS,
// copies them into evaluation/kb-formal/evidence/<campaign>/, and writes
// evaluation/kb-formal/kb-publication-evidence-v1.json, both create-exclusive.
// It runs after the round is sealed; the author's signature on the evidence
// names the SHA-256 it prints, and the paper generator promotes nothing
// without that signature.
//
//	kb-formal-register -root ROOT [-repo .] [-nas-root DIR]
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
)

// RegistrationPath is the one formal registration of the knowledge-product
// evidence; a second complete round would need an explicit new version.
const RegistrationPath = "evaluation/kb-formal/kb-publication-evidence-v1.json"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-register:", err)
		os.Exit(1)
	}
}

func digest(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func run() error {
	rootFlag := flag.String("root", "", "finalized round root")
	repo := flag.String("repo", ".", "repository root")
	nasRoot := flag.String("nas-root", "/mnt/ctg-pto-dev/factgate-evidence", "NAS evidence root")
	roundName := flag.String("round", "kb", "round: kb | keyword (kbformal.Rounds); selects the registration file")
	flag.Parse()
	round, err := kbformal.LookupRound(*roundName)
	if err != nil {
		return err
	}
	registrationPath := round.RegistrationPath
	if *rootFlag == "" {
		return errors.New("-root is required")
	}
	root, err := filepath.Abs(*rootFlag)
	if err != nil {
		return err
	}
	evidenceDir := filepath.Join(root, "evidence")
	summaryPath := filepath.Join(evidenceDir, "campaign-evidence.json")
	summaryBytes, err := os.ReadFile(summaryPath)
	if err != nil {
		return err
	}
	var summary struct {
		Version             string                 `json:"version"`
		Campaign            string                 `json:"campaign"`
		Class               string                 `json:"class"`
		CampaignClass       string                 `json:"campaign_class"`
		PublicationEligible bool                   `json:"publication_eligible"`
		Commit              string                 `json:"commit"`
		BindingSHA256       string                 `json:"dataset_binding_sha256"`
		ApprovalSHA256      string                 `json:"approval_sha256"`
		Evidence            []kbformal.BindingFile `json:"evidence"`
	}
	if err := json.Unmarshal(summaryBytes, &summary); err != nil {
		return err
	}
	switch {
	case summary.Version != "taskgate-kb-campaign-evidence-v1" || summary.Campaign != filepath.Base(root):
		return fmt.Errorf("%s is not the campaign evidence of %s", summaryPath, filepath.Base(root))
	case summary.Class != "formal" || summary.CampaignClass != "kb-publication" || !summary.PublicationEligible:
		return fmt.Errorf("only a formal round registers: class %s, campaign_class %s", summary.Class, summary.CampaignClass)
	case summary.ApprovalSHA256 == "" || summary.BindingSHA256 == "":
		return errors.New("the campaign evidence lacks the binding or approval digest")
	}
	nasEvidence := filepath.Join(*nasRoot, "kb-formal", summary.Campaign, "evidence")
	target := filepath.Join(*repo, "evaluation/kb-formal/evidence", summary.Campaign)
	// The evidence directory may not exist yet; the campaign directory itself
	// is created exclusively, so a campaign is never registered twice.
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		return err
	}
	type entry struct {
		ID        string `json:"id"`
		Path      string `json:"path"`
		SHA256    string `json:"sha256"`
		Bytes     int64  `json:"bytes"`
		NASMirror string `json:"nas_mirror"`
	}
	var entries []entry
	files := append([]kbformal.BindingFile{}, summary.Evidence...)
	summarySum := sha256.Sum256(summaryBytes)
	files = append(files, kbformal.BindingFile{Path: "campaign-evidence.json", SHA256: hex.EncodeToString(summarySum[:]), Bytes: int64(len(summaryBytes))})
	for _, file := range files {
		for _, base := range []string{evidenceDir, nasEvidence} {
			sum, size, err := digest(filepath.Join(base, file.Path))
			if err != nil {
				return err
			}
			if sum != file.SHA256 || size != file.Bytes {
				return fmt.Errorf("%s/%s does not match the campaign evidence", base, file.Path)
			}
		}
		data, err := os.ReadFile(filepath.Join(evidenceDir, file.Path))
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
		id := file.Path[:len(file.Path)-len(filepath.Ext(file.Path))]
		entries = append(entries, entry{ID: id, Path: filepath.Join("evidence", summary.Campaign, file.Path), SHA256: file.SHA256, Bytes: file.Bytes,
			NASMirror: filepath.Join(nasEvidence, file.Path)})
	}
	registration := map[string]any{"version": "taskgate-kb-publication-evidence-v1", "campaign_class": "kb-publication", "publication_eligible": true,
		"campaign": summary.Campaign, "commit": summary.Commit, "dataset_binding_sha256": summary.BindingSHA256, "approval_sha256": summary.ApprovalSHA256,
		"entries": entries,
		"note":    "Formal knowledge-product evidence (正式轮规程 §7). The paper generator uses it only together with the author's signed approval naming this file's SHA-256."}
	encoded, err := json.MarshalIndent(registration, "", " ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(*repo, registrationPath)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := out.Write(encoded); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	fmt.Printf("registered campaign %s: %s sha256=%s (%d evidence files in %s)\n", summary.Campaign, registrationPath, hex.EncodeToString(sum[:]), len(entries), target)
	return nil
}
