// Command kb-queryset generates and freezes the E-KB4 query set from the
// frozen corpus v2: stratified target documents and two model-written
// questions per target (evaluation/kbqueryset). It resumes from an existing
// queries.jsonl and writes a manifest binding the corpus digest.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
	"taskbound.local/agent-data-gateway/evaluation/kbqueryset"
)

type claudeEnvelope struct {
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func main() {
	corpusPath := flag.String("corpus", "evaluation/kbcorpus/raw/v2/documents.jsonl", "frozen corpus JSONL")
	manifestPath := flag.String("corpus-manifest", "evaluation/kbcorpus/manifest-v2.json", "frozen corpus manifest (documents_sha256 is checked)")
	out := flag.String("out", "evaluation/kb-pilot/queryset-v2", "output directory for queries.jsonl and manifest.json")
	count := flag.Int("targets", 300, "number of target documents")
	seed := flag.Uint64("seed", 20261016, "sampling seed")
	batchSize := flag.Int("batch", 10, "targets per claude invocation")
	model := flag.String("model", "claude-haiku-5-5", "model for claude -p")
	claudeBin := flag.String("claude-bin", os.Getenv("CLAUDE_CODE_EXECPATH"), "claude executable")
	budget := flag.Float64("budget-usd", 10, "stop when the nominal cost exceeds this")
	flag.Parse()
	if *claudeBin == "" {
		fatal(fmt.Errorf("claude executable is required (-claude-bin or CLAUDE_CODE_EXECPATH)"))
	}
	corpusBytes, err := os.ReadFile(*corpusPath)
	if err != nil {
		fatal(err)
	}
	var manifest kbcorpus.ManifestV2
	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		fatal(err)
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		fatal(err)
	}
	sum := sha256.Sum256(corpusBytes)
	corpusDigest := hex.EncodeToString(sum[:])
	if corpusDigest != manifest.DocumentsSHA256 {
		fatal(fmt.Errorf("corpus sha256 %s does not match manifest %s", corpusDigest, manifest.DocumentsSHA256))
	}
	var documents []kbcorpus.Document
	scanner := bufio.NewScanner(bytes.NewReader(corpusBytes))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scanner.Scan() {
		var document kbcorpus.Document
		if err := json.Unmarshal(scanner.Bytes(), &document); err != nil {
			fatal(err)
		}
		documents = append(documents, document)
	}
	byKey := map[string]kbcorpus.Document{}
	for _, document := range documents {
		byKey[kbqueryset.Key(document.DocID, document.Version)] = document
	}
	targets, err := kbqueryset.Sample(documents, *count, *seed)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	queriesPath := filepath.Join(*out, "queries.jsonl")
	existing := loadExisting(queriesPath)
	var pending []kbqueryset.Target
	for _, target := range targets {
		if _, done := existing[kbqueryset.Key(target.DocID, target.Version)]; !done {
			pending = append(pending, target)
		}
	}
	fmt.Printf("targets=%d existing=%d pending=%d\n", len(targets), len(existing), len(pending))
	var cost float64
	for start := 0; start < len(pending); start += *batchSize {
		end := start + *batchSize
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		queries, batchCost, err := generate(*claudeBin, *model, batch, byKey)
		cost += batchCost
		if err != nil {
			fmt.Fprintf(os.Stderr, "kb-queryset: batch %d..%d: %v (cost so far %.4f)\n", start, end, err, cost)
			if cost > *budget {
				fatal(fmt.Errorf("budget exceeded"))
			}
			continue
		}
		for _, query := range queries {
			existing[kbqueryset.Key(query.DocID, query.Version)] = append(existing[kbqueryset.Key(query.DocID, query.Version)], query)
		}
		if err := writeQueries(queriesPath, existing); err != nil {
			fatal(err)
		}
		fmt.Printf("batch %d..%d ok cost=%.4f total=%.4f\n", start, end, batchCost, cost)
		if cost > *budget {
			fatal(fmt.Errorf("budget exceeded"))
		}
	}
	var all []kbqueryset.Query
	for _, target := range targets {
		all = append(all, existing[kbqueryset.Key(target.DocID, target.Version)]...)
	}
	if len(all) != 2*len(targets) {
		fmt.Printf("INCOMPLETE queries=%d of %d; rerun to resume\n", len(all), 2*len(targets))
		os.Exit(3)
	}
	encoded, digest, err := kbqueryset.Encode(all)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(queriesPath, encoded, 0o644); err != nil {
		fatal(err)
	}
	targetsJSON, _ := json.Marshal(targets)
	targetsSum := sha256.Sum256(targetsJSON)
	frozen := kbqueryset.Summarize(kbqueryset.Manifest{Version: kbqueryset.Version, Generator: "evaluation/cmd/kb-queryset", Model: *model,
		CorpusSHA256: corpusDigest, Seed: *seed, Targets: len(targets), TotalCostUSD: cost + priorCost(*out), QueriesBytes: len(encoded),
		QueriesSHA256: digest, TargetsSHA256: hex.EncodeToString(targetsSum[:])}, all)
	manifestOut, _ := json.MarshalIndent(frozen, "", " ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), append(manifestOut, '\n'), 0o644); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "targets.json"), append(targetsJSON, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("QUERYSET_DONE queries=%d bytes=%d sha256=%s cost_usd=%.4f\n", len(all), len(encoded), digest, frozen.TotalCostUSD)
}

func generate(claudeBin, model string, batch []kbqueryset.Target, documents map[string]kbcorpus.Document) ([]kbqueryset.Query, float64, error) {
	prompt, err := kbqueryset.Prompt(batch, documents)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, claudeBin, "-p", "--model", model, "--output-format", "json", prompt)
	command.Dir = os.TempDir()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, 0, fmt.Errorf("claude -p: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var envelope claudeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		return nil, 0, fmt.Errorf("decode claude envelope: %w", err)
	}
	if envelope.IsError {
		return nil, envelope.TotalCostUSD, fmt.Errorf("claude reported an error: %.200s", envelope.Result)
	}
	generated, err := kbqueryset.ParseResponse(envelope.Result)
	if err != nil {
		return nil, envelope.TotalCostUSD, err
	}
	byKey := map[string]kbqueryset.Generated{}
	for _, item := range generated {
		byKey[kbqueryset.Key(item.DocID, item.Version)] = item
	}
	var queries []kbqueryset.Query
	for _, target := range batch {
		item, ok := byKey[kbqueryset.Key(target.DocID, target.Version)]
		if !ok {
			return nil, envelope.TotalCostUSD, fmt.Errorf("reply lacks %s", kbqueryset.Key(target.DocID, target.Version))
		}
		pair, err := kbqueryset.Apply(target, item)
		if err != nil {
			return nil, envelope.TotalCostUSD, err
		}
		queries = append(queries, pair...)
	}
	return queries, envelope.TotalCostUSD, nil
}

func loadExisting(path string) map[string][]kbqueryset.Query {
	existing := map[string][]kbqueryset.Query{}
	file, err := os.Open(path)
	if err != nil {
		return existing
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var query kbqueryset.Query
		if err := json.Unmarshal(scanner.Bytes(), &query); err != nil {
			continue
		}
		existing[kbqueryset.Key(query.DocID, query.Version)] = append(existing[kbqueryset.Key(query.DocID, query.Version)], query)
	}
	return existing
}

func writeQueries(path string, existing map[string][]kbqueryset.Query) error {
	var all []kbqueryset.Query
	for _, queries := range existing {
		all = append(all, queries...)
	}
	encoded, _, err := kbqueryset.Encode(all)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// priorCost reads the cost a previous, interrupted run recorded, if any.
func priorCost(dir string) float64 {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0
	}
	var manifest kbqueryset.Manifest
	if json.Unmarshal(data, &manifest) != nil {
		return 0
	}
	return manifest.TotalCostUSD
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "kb-queryset:", err)
	os.Exit(1)
}
