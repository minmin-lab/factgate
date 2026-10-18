// Command kb-formal-driver runs the knowledge-product experiments of the
// formal round against one fresh deployment over its public interfaces
// (docs/kb_experiment_design.md, 正式轮规程 §5) and writes one per-deployment
// report and, with -request-log, the per-request record of the run (正式轮规程
// §7). Credentials are read in-process from a deployment env file and a
// principal token file and never written out; the driver keeps stderr empty
// on success.
//
//	kb-formal-driver -experiment ekb5 -label document -deployment 1 -report out.json \
//	  -request-log out.requests.jsonl -catalog P.catalog.yaml -env-file E -principal-tokens T \
//	  -business-dsn-env KB_FORMAL_READER_DSN
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/kbpublish"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-driver:", err)
		os.Exit(1)
	}
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	return values, scanner.Err()
}

func run() (err error) {
	experiment := flag.String("experiment", "", "ekb1 | ekb2a | ekb2b | ekb3 | ekb4 | ekb5 | ekb6 | ekb7 | ekb8")
	label := flag.String("label", "", "ekb5: document | collection")
	tier := flag.String("tier", "", "ekb3, ekb8: 48k | 96k")
	rounds := flag.Int("rounds", 200, "ekb3, ekb8: rounds per deployment (pre-registered: 200)")
	requestLogPath := flag.String("request-log", "", "create the per-request JSONL record here")
	deployment := flag.Int("deployment", 0, "deployment index (1..3)")
	reportPath := flag.String("report", "", "create the per-deployment report here")
	catalogPath := flag.String("catalog", "", "the activated profile Catalog")
	gateway := flag.String("gateway", "http://127.0.0.1:8082", "gateway base URL")
	oa := flag.String("oa", "http://127.0.0.1:8092", "OA base URL")
	envFile := flag.String("env-file", "", "deployment env file")
	tokensFile := flag.String("principal-tokens", "", "JSON map subject -> token for file principals")
	businessEnv := flag.String("business-dsn-env", "KB_FORMAL_READER_DSN", "environment variable holding the business database's read-only DSN")
	embedding := flag.String("embedding", "http://127.0.0.1:28090", "the deployment's embedding service (direct-arm query vectors)")
	queryset := flag.String("queryset", "evaluation/kb-pilot/queryset-v2", "frozen query set directory")
	keywords := flag.String("keywords", "evaluation/kb-formal/keywordset-v1", "frozen keyword set directory (ekb6, ekb7, ekb8)")
	seeds := flag.Int("seeds", 3, "ekb1, ekb7: seeds per deployment")
	steps := flag.Int("steps", 400, "ekb1, ekb7: maximum steps per trajectory")
	flag.Parse()
	if *experiment == "" || *deployment < 1 || *reportPath == "" || *catalogPath == "" || *envFile == "" {
		return errors.New("-experiment, -deployment, -report, -catalog and -env-file are required")
	}
	if _, err := os.Lstat(*reportPath); err == nil {
		return fmt.Errorf("report already exists: %s", *reportPath)
	}
	var requests *kbformal.RequestLog
	if *requestLogPath != "" {
		if requests, err = kbformal.CreateRequestLog(*requestLogPath); err != nil {
			return err
		}
		// Closed on every path, so the record of a failed run is complete up
		// to its last request.
		defer func() {
			if closeErr := requests.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("request log: %w", closeErr)
			}
		}()
	}
	env, err := readEnvFile(*envFile)
	if err != nil {
		return err
	}
	profile, err := catalog.Load(*catalogPath)
	if err != nil {
		return err
	}
	if len(profile.KnowledgeProducts) != 1 {
		return errors.New("profile catalog must declare exactly one knowledge product")
	}
	ctx := context.Background()
	timeout := 2 * time.Minute
	verifier, err := kbformal.FetchReceiptVerifier(ctx, *gateway, timeout)
	if err != nil {
		return fmt.Errorf("receipt keyring: %w", err)
	}
	business, err := pgx.Connect(ctx, os.Getenv(*businessEnv))
	if err != nil {
		return fmt.Errorf("business database: %w", err)
	}
	defer business.Close(ctx)
	alice := kbformal.NewMCPClient(*gateway, env["TASKBOUND_ALICE_TOKEN"], timeout)
	alice.Label, alice.Log = "alice", requests
	d := &kbformal.Deployment{Index: *deployment, Catalog: profile, Product: profile.KnowledgeProducts[0], Alice: alice,
		Provisioner: &kbformal.Provisioner{Requester: alice,
			Applicant: &kbformal.OAAccount{BaseURL: *oa, Username: "alice", Password: env["OA_ALICE_PASSWORD"], Timeout: timeout, Log: requests},
			Approver:  &kbformal.OAAccount{BaseURL: *oa, Username: "bob", Password: env["OA_BOB_PASSWORD"], Timeout: timeout, Log: requests},
			Poll:      200 * time.Millisecond, Deadline: 2 * time.Minute},
		Principals: map[string]*kbformal.MCPClient{}, Verifier: verifier, Business: business, Timeout: timeout}
	if *tokensFile != "" {
		var tokens map[string]string
		data, err := os.ReadFile(*tokensFile)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &tokens); err != nil {
			return err
		}
		for subject, token := range tokens {
			client := kbformal.NewMCPClient(*gateway, token, timeout)
			client.Label, client.Log = subject, requests
			d.Principals[subject] = client
		}
	}
	started := time.Now()
	var result any
	switch *experiment {
	case "ekb5":
		if *label != "document" && *label != "collection" {
			return errors.New("ekb5 needs -label document or collection")
		}
		result, err = kbformal.RunEKB5(ctx, d, *label)
	case "ekb2a":
		result, err = kbformal.RunEKB2a(ctx, d)
	case "ekb2b":
		result, err = kbformal.RunEKB2b(ctx, d)
	case "ekb1":
		embedder, embedErr := kbpublish.NewHTTPEmbedder(*embedding)
		if embedErr != nil {
			return embedErr
		}
		result, err = kbformal.RunEKB1(ctx, d, embedder, *queryset, *seeds, *steps)
	case "ekb3":
		if *tier != "48k" && *tier != "96k" {
			return errors.New("ekb3 needs -tier 48k or 96k")
		}
		embedder, embedErr := kbpublish.NewHTTPEmbedder(*embedding)
		if embedErr != nil {
			return embedErr
		}
		result, err = kbformal.RunEKB3(ctx, d, embedder, *tier, *rounds)
	case "ekb4":
		embedder, embedErr := kbpublish.NewHTTPEmbedder(*embedding)
		if embedErr != nil {
			return embedErr
		}
		result, err = kbformal.RunEKB4(ctx, d, embedder, *queryset)
	case "ekb6":
		result, err = kbformal.RunEKB6(ctx, d, *keywords)
	case "ekb7":
		result, err = kbformal.RunEKB7(ctx, d, *keywords, *seeds, *steps)
	case "ekb8":
		if *tier != "48k" && *tier != "96k" {
			return errors.New("ekb8 needs -tier 48k or 96k")
		}
		result, err = kbformal.RunEKB8(ctx, d, *keywords, *tier, *rounds)
	default:
		return fmt.Errorf("unknown experiment %q", *experiment)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", *experiment, err)
	}
	report := map[string]any{"version": "taskgate-kb-formal-deployment-report-v1", "experiment": *experiment, "label": *label, "tier": *tier,
		"deployment": *deployment, "catalog_sha256": profile.SHA256, "knowledge_product": d.Product.Name, "epoch": d.Product.IndexPublication,
		"started_at": started.UTC().Format(time.RFC3339Nano), "seconds": time.Since(started).Seconds(), "result": result}
	encoded, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
