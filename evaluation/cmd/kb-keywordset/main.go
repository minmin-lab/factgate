// Command kb-keywordset writes the frozen keyword set of the keyword
// experiments E-KB6..E-KB8 (docs/kb_experiment_design.md, 关键词检索实验):
// anchors from the E-KB4 targets, frequent bigrams of the source corpus, and
// absent strings checked against every listed corpus. It is deterministic.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
)

func main() {
	source := flag.String("source", "evaluation/kbcorpus/raw/v2/documents.jsonl", "corpus the anchors and frequent keywords come from")
	absentCheck := flag.String("absent-check", "evaluation/kbcorpus/raw/v1-20k/documents.jsonl", "comma-separated further corpora an absent keyword must not occur in")
	targets := flag.String("targets", "evaluation/kb-pilot/queryset-v2/targets.json", "E-KB4 query-set targets")
	window := flag.Int("window", 800, "segmentation window (section-window-v1), as in the publication spec")
	out := flag.String("out", "evaluation/kb-formal/keywordset-v1", "output directory")
	flag.Parse()
	if err := run(*source, *absentCheck, *targets, *window, *out); err != nil {
		fmt.Fprintln(os.Stderr, "kb-keywordset:", err)
		os.Exit(1)
	}
}

func run(sourcePath, absentPaths, targetsPath string, window int, out string) error {
	documents, sourceTexts, sourceSHA, err := kbformal.LoadCorpusChunks(sourcePath, window)
	if err != nil {
		return err
	}
	corpora := [][]string{sourceTexts}
	absentDigests := []string{sourceSHA}
	for _, path := range strings.Split(absentPaths, ",") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		_, texts, digest, err := kbformal.LoadCorpusChunks(path, window)
		if err != nil {
			return err
		}
		corpora = append(corpora, texts)
		absentDigests = append(absentDigests, digest)
	}
	rawTargets, err := os.ReadFile(targetsPath)
	if err != nil {
		return err
	}
	var targets []kbformal.KeywordTarget
	if err := json.Unmarshal(rawTargets, &targets); err != nil {
		return err
	}
	keywords, err := kbformal.GenerateKeywordset(documents, sourceTexts, corpora, targets, window)
	if err != nil {
		return err
	}
	targetsSum := sha256.Sum256(rawTargets)
	manifest := kbformal.KeywordsetManifest{Generator: "evaluation/cmd/kb-keywordset", SourceCorpusSHA256: sourceSHA,
		AbsentCorporaSHA256: absentDigests, QuerysetTargetSHA256: hex.EncodeToString(targetsSum[:]), WindowChars: window}
	if err := kbformal.WriteKeywordset(out, keywords, manifest); err != nil {
		return err
	}
	fmt.Printf("wrote %d keywords to %s\n", len(keywords), out)
	return nil
}
