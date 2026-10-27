// Command kb-corpus-v2 writes the realistic knowledge-product corpus: the v1
// generator's deterministic metadata plan is sent in batches to a headless
// Claude Code invocation (claude -p) that writes Chinese prose; every document
// is validated against its plan before it is appended to documents.jsonl, and
// the run's cost is recorded. The result is frozen by digest, not regenerable.
//
// -plan v3 sends the five-grade v3 plan (kbcorpus.PlanV3) instead and stamps
// the manifest with the v3 version and generator. A run holds <out>/.lock
// (created with O_EXCL, holding its pid) so two runs never append to the same
// output directory.
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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbcorpus"
)

type batchRecord struct {
	Batch      int      `json:"batch"`
	DocIDs     []string `json:"doc_ids"`
	Attempt    int      `json:"attempt"`
	CostUSD    float64  `json:"cost_usd"`
	DurationMS int64    `json:"duration_ms"`
	Error      string   `json:"error,omitempty"`
	Model      string   `json:"model"`
	At         string   `json:"at"`
}

type claudeEnvelope struct {
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	IsError      bool    `json:"is_error"`
}

func main() {
	seed := flag.Uint64("seed", 20261015, "plan seed (shared with corpus v1)")
	documents := flag.Int("documents", 10000, "documents in the plan")
	start := flag.Int("start", 0, "first plan index to generate (inclusive)")
	limit := flag.Int("limit", 0, "number of plan entries to generate (0 = all remaining)")
	batchSize := flag.Int("batch", 10, "documents per claude invocation")
	out := flag.String("out", "evaluation/kbcorpus/raw/v2", "output directory")
	model := flag.String("model", "claude-haiku-5-5", "model for claude -p")
	claudeBin := flag.String("claude-bin", os.Getenv("CLAUDE_CODE_EXECPATH"), "claude executable")
	maxAttempts := flag.Int("attempts", 3, "attempts per batch")
	budgetUSD := flag.Float64("budget-usd", 120, "stop when cumulative cost exceeds this")
	freeze := flag.String("freeze", "", "write manifest-v2.json here at the end (also written to -out)")
	parallel := flag.Int("parallel", 1, "batches generated concurrently")
	planName := flag.String("plan", "v2", "metadata plan: v2 (four grades, kbcorpus.Generate) or v3 (five grades, kbcorpus.PlanV3)")
	flag.Parse()
	if *parallel < 1 {
		*parallel = 1
	}
	if *claudeBin == "" {
		fatal(fmt.Errorf("claude executable is required (-claude-bin or CLAUDE_CODE_EXECPATH)"))
	}
	spec := kbcorpus.Spec{Seed: *seed, Documents: *documents}
	var plans []kbcorpus.Document
	var err error
	manifestVersion, manifestGenerator := kbcorpus.VersionV2, kbcorpus.GeneratorV2
	prompt, apply := kbcorpus.BatchPrompt, kbcorpus.ApplyGenerated
	switch *planName {
	case "v2":
		plans, err = kbcorpus.Generate(spec)
	case "v3":
		plans, err = kbcorpus.PlanV3(spec)
		manifestVersion, manifestGenerator = kbcorpus.VersionV3, kbcorpus.GeneratorV3
		prompt, apply = kbcorpus.BatchPromptV3, kbcorpus.ApplyGeneratedV3
	default:
		err = fmt.Errorf("-plan must be v2 or v3, got %q", *planName)
	}
	if err != nil {
		fatal(err)
	}
	planJSONL, err := kbcorpus.EncodeJSONL(plans)
	if err != nil {
		fatal(err)
	}
	planDigest := sha256.Sum256(planJSONL)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	acquireLock(filepath.Join(*out, ".lock"))
	outputPath := filepath.Join(*out, "documents.jsonl")
	applied := loadExisting(outputPath)
	if err := checkSamePlan(applied, plans); err != nil {
		fatal(fmt.Errorf("%s was generated from a different plan than -plan %s -seed %d: %w", outputPath, *planName, *seed, err))
	}
	batchesPath := filepath.Join(*out, "batches.jsonl")
	batchesFile, err := os.OpenFile(batchesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(err)
	}
	defer batchesFile.Close()
	outputFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(err)
	}
	defer outputFile.Close()

	end := len(plans)
	if *limit > 0 && *start+*limit < end {
		end = *start + *limit
	}
	// Batches keep every version of a document together so a revision can copy
	// its predecessor's sections.
	var batches [][]kbcorpus.Document
	for index := *start; index < end; {
		var batch []kbcorpus.Document
		for index < end && len(batch) < *batchSize {
			doc := plans[index]
			if _, done := applied[versionKey(doc.DocID, doc.Version)]; done {
				index++
				continue
			}
			batch = append(batch, doc)
			index++
			for index < end && plans[index].DocID == doc.DocID {
				batch = append(batch, plans[index])
				index++
			}
		}
		if len(batch) > 0 {
			batches = append(batches, batch)
		}
	}
	// Workers generate batches concurrently; one goroutine owns both files so
	// appends never interleave. Revisions always sit in the same batch as their
	// first version, so a batch never depends on another batch's output.
	type outcome struct {
		number  int
		docs    []kbcorpus.Document
		records []batchRecord
		err     error
	}
	results := make(chan outcome)
	jobs := make(chan int)
	appliedSnapshot := make(map[string]kbcorpus.Document, len(applied))
	for key, doc := range applied {
		appliedSnapshot[key] = doc
	}
	for worker := 0; worker < *parallel; worker++ {
		go func() {
			for number := range jobs {
				batch := batches[number]
				var result outcome
				result.number = number
				for attempt := 1; attempt <= *maxAttempts; attempt++ {
					record := batchRecord{Batch: number, Attempt: attempt, Model: *model, At: time.Now().UTC().Format(time.RFC3339)}
					for _, doc := range batch {
						record.DocIDs = append(record.DocIDs, versionKey(doc.DocID, doc.Version))
					}
					started := time.Now()
					docs, cost, err := generateBatch(*claudeBin, *model, prompt, apply, batch, appliedSnapshot)
					record.DurationMS = time.Since(started).Milliseconds()
					record.CostUSD = cost
					if err != nil {
						record.Error = err.Error()
					}
					result.records = append(result.records, record)
					if err == nil {
						result.docs, result.err = docs, nil
						break
					}
					result.err = err
				}
				results <- result
			}
		}()
	}
	go func() {
		for number := range batches {
			jobs <- number
		}
		close(jobs)
	}()
	var totalCost float64
	retries, generated := 0, 0
	stop := false
	for completed := 0; completed < len(batches); completed++ {
		result := <-results
		for _, record := range result.records {
			totalCost += record.CostUSD
			if record.Error != "" {
				retries++
			}
			encoded, _ := json.Marshal(record)
			_, _ = batchesFile.Write(append(encoded, '\n'))
		}
		if result.err != nil {
			fmt.Fprintf(os.Stderr, "batch %d abandoned after %d attempts: %v\n", result.number, *maxAttempts, result.err)
		} else {
			for _, doc := range result.docs {
				line, _ := json.Marshal(doc)
				if _, err := outputFile.Write(append(line, '\n')); err != nil {
					fatal(err)
				}
				generated++
			}
		}
		fmt.Printf("batch %d done (%d/%d) generated=%d cost_usd=%.4f\n", result.number, completed+1, len(batches), generated, totalCost)
		if totalCost > *budgetUSD && !stop {
			fmt.Fprintf(os.Stderr, "budget %.2f USD exceeded (%.4f); draining in-flight batches and stopping\n", *budgetUSD, totalCost)
			stop = true
			// Drain: stop handing out new jobs by consuming the remaining job
			// numbers ourselves, then collect in-flight results.
			remaining := 0
			for range jobs {
				remaining++
			}
			for completed+1 < len(batches)-remaining {
				completed++
				extra := <-results
				for _, record := range extra.records {
					totalCost += record.CostUSD
					encoded, _ := json.Marshal(record)
					_, _ = batchesFile.Write(append(encoded, '\n'))
				}
				if extra.err == nil {
					for _, doc := range extra.docs {
						line, _ := json.Marshal(doc)
						_, _ = outputFile.Write(append(line, '\n'))
						generated++
					}
				}
			}
			break
		}
	}
	_ = outputFile.Sync()
	data, err := os.ReadFile(outputPath)
	if err != nil {
		fatal(err)
	}
	docs, err := kbcorpus.DecodeJSONL(data)
	if err != nil {
		fatal(err)
	}
	counts := kbcorpus.CountDocuments(docs)
	sum := sha256.Sum256(data)
	manifest := kbcorpus.ManifestV2{Version: manifestVersion, Generator: manifestGenerator,
		PlanSpec: spec, PlanSHA256: hex.EncodeToString(planDigest[:]),
		Model: *model, BatchSize: *batchSize, Batches: countLines(batchesPath), Retries: retries, TotalCostUSD: sumCost(batchesPath),
		Counts: counts, DocumentsBytes: int64(len(data)), DocumentsSHA256: hex.EncodeToString(sum[:])}
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), encoded, 0o644); err != nil {
		fatal(err)
	}
	if *freeze != "" {
		if err := os.WriteFile(*freeze, encoded, 0o644); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("documents=%d versions=%d planned=%d generated_now=%d cost_usd=%.4f sha256=%s\n",
		counts.Documents, counts.Versions, len(plans), generated, manifest.TotalCostUSD, manifest.DocumentsSHA256)
	releaseLock()
}

// lockPath is the held <out>/.lock, removed on normal exit, on fatal and on
// SIGINT/SIGTERM. A SIGKILL leaves it behind; the refusal message says how to
// clear it.
var lockPath string

func acquireLock(path string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			holder, _ := os.ReadFile(path)
			fatal(fmt.Errorf("%s exists (pid %s): another run is writing this output directory; if no such process is running, remove the file and retry",
				path, strings.TrimSpace(string(holder))))
		}
		fatal(err)
	}
	lockPath = path
	_, writeErr := file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	closeErr := file.Close()
	if writeErr != nil {
		fatal(writeErr)
	}
	if closeErr != nil {
		fatal(closeErr)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		received := <-signals
		releaseLock()
		fmt.Fprintf(os.Stderr, "kb-corpus-v2: %v received, lock released\n", received)
		os.Exit(1)
	}()
}

func releaseLock() {
	if lockPath != "" {
		_ = os.Remove(lockPath)
		lockPath = ""
	}
}

func versionKey(docID string, version int) string { return fmt.Sprintf("%s v%d", docID, version) }

// loadExisting reads the documents already generated. A run interrupted mid
// write (WSL restart, power loss) can leave one truncated final line; that
// line is dropped from the file and regenerated rather than treated as
// corruption. Any earlier malformed line is still fatal.
func loadExisting(path string) map[string]kbcorpus.Document {
	applied := map[string]kbcorpus.Document{}
	data, err := os.ReadFile(path)
	if err != nil {
		return applied
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		cut := bytes.LastIndexByte(data, '\n') + 1
		fmt.Fprintf(os.Stderr, "dropping truncated final line (%d bytes) from %s\n", len(data)-cut, path)
		if err := os.WriteFile(path, data[:cut], 0o644); err != nil {
			fatal(err)
		}
		data = data[:cut]
	}
	docs, err := kbcorpus.DecodeJSONL(data)
	if err != nil {
		fatal(fmt.Errorf("existing %s is corrupt: %w", path, err))
	}
	for _, doc := range docs {
		applied[versionKey(doc.DocID, doc.Version)] = doc
	}
	return applied
}

// checkSamePlan refuses to resume into an output directory whose documents
// disagree with the current plan on any planned attribute (a v2 directory
// resumed with -plan v3, or another seed): resuming would skip their keys as
// done and stamp the manifest with the wrong plan. Keys outside the plan are
// left alone, as before.
func checkSamePlan(applied map[string]kbcorpus.Document, plans []kbcorpus.Document) error {
	byKey := make(map[string]kbcorpus.Document, len(plans))
	for _, plan := range plans {
		byKey[versionKey(plan.DocID, plan.Version)] = plan
	}
	for key, doc := range applied {
		plan, ok := byKey[key]
		if !ok {
			continue
		}
		if doc.Classification != plan.Classification || doc.JobRole != plan.JobRole || doc.DocumentType != plan.DocumentType ||
			doc.Department != plan.Department || len(doc.Sections) != len(plan.Sections) {
			return fmt.Errorf("%s: document attributes differ from the plan", key)
		}
		for index, section := range doc.Sections {
			if section.Heading != plan.Sections[index].Heading || section.Classification != plan.Sections[index].Classification {
				return fmt.Errorf("%s: section %d is %q [%s], plan has %q [%s]", key, index, section.Heading, section.Classification,
					plan.Sections[index].Heading, plan.Sections[index].Classification)
			}
		}
	}
	return nil
}

// generateBatch renders the batch with render and validates each reply with
// apply (ApplyGenerated for v2, ApplyGeneratedV3 for v3); any rejection fails
// the whole batch, which the caller retries.
func generateBatch(claudeBin, model string, render func([]kbcorpus.GenerationItem) (string, error),
	apply func(kbcorpus.Document, kbcorpus.GeneratedDocument, *kbcorpus.Document) (kbcorpus.Document, error),
	batch []kbcorpus.Document, applied map[string]kbcorpus.Document) ([]kbcorpus.Document, float64, error) {
	prompt, err := render(kbcorpus.PlanItems(batch))
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
		return nil, envelope.TotalCostUSD, fmt.Errorf("claude reported an error: %s", truncate(envelope.Result))
	}
	generated, err := kbcorpus.ParseBatchResponse(envelope.Result)
	if err != nil {
		return nil, envelope.TotalCostUSD, err
	}
	if len(generated) != len(batch) {
		return nil, envelope.TotalCostUSD, fmt.Errorf("%d documents generated for a batch of %d", len(generated), len(batch))
	}
	byKey := map[string]kbcorpus.GeneratedDocument{}
	for _, item := range generated {
		byKey[versionKey(item.DocID, item.Version)] = item
	}
	local := map[string]kbcorpus.Document{}
	var docs []kbcorpus.Document
	for _, plan := range batch {
		item, ok := byKey[versionKey(plan.DocID, plan.Version)]
		if !ok {
			return nil, envelope.TotalCostUSD, fmt.Errorf("batch reply lacks %s", versionKey(plan.DocID, plan.Version))
		}
		var previous *kbcorpus.Document
		if plan.Version > 1 {
			if prior, ok := local[versionKey(plan.DocID, plan.Version-1)]; ok {
				previous = &prior
			} else if prior, ok := applied[versionKey(plan.DocID, plan.Version-1)]; ok {
				previous = &prior
			}
		}
		doc, err := apply(plan, item, previous)
		if err != nil {
			return nil, envelope.TotalCostUSD, err
		}
		local[versionKey(doc.DocID, doc.Version)] = doc
		docs = append(docs, doc)
	}
	return docs, envelope.TotalCostUSD, nil
}

func truncate(text string) string {
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}

func countLines(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		count++
	}
	return count
}

func sumCost(path string) float64 {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	total := 0.0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var record batchRecord
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			total += record.CostUSD
		}
	}
	return total
}

func fatal(err error) {
	releaseLock()
	fmt.Fprintln(os.Stderr, "kb-corpus-v2:", err)
	os.Exit(1)
}
