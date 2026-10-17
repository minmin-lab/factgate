// Command kb-formal-smoke exercises one fresh knowledge-product deployment
// end to end over its public interfaces (K4.10 acceptance): a real task
// request approved through the OA, a search whose receipt verifies against the
// gateway's published keyring and whose delivered text matches the released
// digests, the returned-chunk rule on fetch, and eligibility for file
// principals. It prints one JSON summary and exits non-zero on any failure.
//
// Credentials come from a deployment env file (-env-file) and a principal
// token file (-principal-tokens), read in-process and never printed.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/kbformal"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kb-formal-smoke:", err)
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
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values, scanner.Err()
}

func run() error {
	gateway := flag.String("gateway", "http://127.0.0.1:8082", "gateway base URL")
	oa := flag.String("oa", "http://127.0.0.1:8092", "OA base URL")
	envFile := flag.String("env-file", "", "deployment env file")
	tokensFile := flag.String("principal-tokens", "", "JSON map subject -> token for file principals")
	catalogPath := flag.String("catalog", "", "the activated profile Catalog")
	flag.Parse()
	env, err := readEnvFile(*envFile)
	if err != nil {
		return err
	}
	var tokens map[string]string
	if data, err := os.ReadFile(*tokensFile); err != nil {
		return err
	} else if err := json.Unmarshal(data, &tokens); err != nil {
		return err
	}
	profile, err := catalog.Load(*catalogPath)
	if err != nil {
		return err
	}
	if len(profile.KnowledgeProducts) != 1 {
		return errors.New("profile catalog must declare one knowledge product")
	}
	product := profile.KnowledgeProducts[0]
	ctx := context.Background()
	timeout := 60 * time.Second
	summary := map[string]any{"catalog_sha256": profile.SHA256, "knowledge_product": product.Name, "epoch": product.IndexPublication}
	alice := kbformal.NewMCPClient(*gateway, env["TASKBOUND_ALICE_TOKEN"], timeout)
	provisioner := &kbformal.Provisioner{Requester: alice,
		Applicant: &kbformal.OAAccount{BaseURL: *oa, Username: "alice", Password: env["OA_ALICE_PASSWORD"], Timeout: timeout},
		Approver:  &kbformal.OAAccount{BaseURL: *oa, Username: "bob", Password: env["OA_BOB_PASSWORD"], Timeout: timeout},
		Poll:      200 * time.Millisecond, Deadline: 60 * time.Second}
	chunks, documents := product.Products.Chunks, product.Products.Documents
	request := kbformal.TaskRequest{Objective: "K4.10 smoke: department policy retrieval",
		Products: []string{chunks, documents},
		Columns: map[string][]string{
			chunks:    {"doc_id", "version", "chunk_index", "text_sha256", "kb_department", "classification", "job_role", "valid_until"},
			documents: {"doc_id", "version", "title", "document_type", "kb_department", "classification"},
		},
		Scope: map[string]any{"kb_department": []string{"finance"}, "classification": []string{"internal", "public"}}}
	started := time.Now()
	taskID, created, err := provisioner.Provision(ctx, request)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	summary["provision_ms"] = time.Since(started).Milliseconds()
	summary["budget_profile"] = created["budget_profile"]
	verifier, err := kbformal.FetchReceiptVerifier(ctx, *gateway, timeout)
	if err != nil {
		return fmt.Errorf("keyring: %w", err)
	}
	search, err := alice.Call(ctx, "search", map[string]any{"task_id": taskID, "request_id": "smoke-search-1",
		"knowledge_product": product.Name, "query": map[string]any{"text": "财务部 差旅 报销 标准"}, "k": 5})
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}
	receipt, err := kbformal.VerifyReceipt(verifier, search["receipt"])
	if err != nil {
		return fmt.Errorf("search receipt: %w", err)
	}
	delivered, err := kbformal.DeliverRows(ctx, alice, receipt, timeout)
	if err != nil {
		return fmt.Errorf("search delivery: %w", err)
	}
	rows, err := checkRows(delivered)
	if err != nil {
		return fmt.Errorf("search rows: %w", err)
	}
	if receipt.CatalogDigest != profile.SHA256 {
		return fmt.Errorf("receipt catalog digest %s is not the profile's %s", receipt.CatalogDigest, profile.SHA256)
	}
	summary["search_rows"] = len(rows)
	summary["search_exposure"] = search["exposure"]
	summary["receipt_verified"] = true
	returned := []map[string]any{{"doc_id": rows[0][0], "version": rows[0][1], "chunk_index": rows[0][2]}}
	fetch, err := alice.Call(ctx, "fetch", map[string]any{"task_id": taskID, "request_id": "smoke-fetch-returned",
		"knowledge_product": product.Name, "chunks": returned})
	if err != nil {
		return fmt.Errorf("fetch of a returned chunk: %w", err)
	}
	fetchReceipt, err := kbformal.VerifyReceipt(verifier, fetch["receipt"])
	if err != nil {
		return fmt.Errorf("fetch receipt: %w", err)
	}
	fetchedRows, err := kbformal.DeliverRows(ctx, alice, fetchReceipt, timeout)
	if err != nil {
		return fmt.Errorf("fetch delivery: %w", err)
	}
	if fetched, err := checkRows(fetchedRows); err != nil || len(fetched) != 1 {
		return fmt.Errorf("fetch rows: %v (%d)", err, len(fetched))
	}
	notReturned := []map[string]any{{"doc_id": rows[0][0], "version": rows[0][1], "chunk_index": json.Number("9999")}}
	_, err = alice.Call(ctx, "fetch", map[string]any{"task_id": taskID, "request_id": "smoke-fetch-unreturned",
		"knowledge_product": product.Name, "chunks": notReturned})
	var toolErr *kbformal.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != "CHUNK_NOT_RETURNED" {
		return fmt.Errorf("fetch of an unreturned chunk: want CHUNK_NOT_RETURNED, got %v", err)
	}
	summary["fetch_unreturned_code"] = toolErr.Code
	// Eligibility through file principals: another department is refused before
	// any draft, the subject's own department is admitted.
	if token := tokens["sales-staff"]; token != "" {
		sales := kbformal.NewMCPClient(*gateway, token, timeout)
		request := map[string]any{"objective": "K4.10 smoke eligibility", "data_products": []string{chunks, documents},
			"columns": request.Columns, "scopes": map[string]any{"kb_department": []string{"finance"}, "classification": []string{"internal"}}}
		_, err := sales.Call(ctx, "request_data_task", request)
		if !errors.As(err, &toolErr) || toolErr.Code != "NOT_ELIGIBLE" {
			return fmt.Errorf("sales-staff requesting finance: want NOT_ELIGIBLE, got %v", err)
		}
		request["scopes"] = map[string]any{"kb_department": []string{"sales"}, "classification": []string{"internal"}}
		admitted, err := sales.Call(ctx, "request_data_task", request)
		if err != nil {
			return fmt.Errorf("sales-staff requesting sales: %w", err)
		}
		summary["eligibility_other_department"] = "NOT_ELIGIBLE"
		summary["eligibility_own_department_state"] = admitted["state"]
	} else {
		return errors.New("principal token for sales-staff is missing")
	}
	encoded, _ := json.MarshalIndent(summary, "", " ")
	fmt.Println(string(encoded))
	return nil
}

// checkRows requires the retrieval shape (doc_id, version, chunk_index,
// text_sha256, text) and that every delivered text hashes to its digest.
func checkRows(rows [][]any) ([][]any, error) {
	if len(rows) == 0 {
		return nil, errors.New("no rows")
	}
	for _, row := range rows {
		if len(row) != 5 {
			return nil, fmt.Errorf("row has %d columns, want 5", len(row))
		}
		text, _ := row[4].(string)
		sum := sha256.Sum256([]byte(text))
		if text == "" || hex.EncodeToString(sum[:]) != fmt.Sprint(row[3]) {
			return nil, errors.New("delivered text does not hash to the released digest")
		}
	}
	return rows, nil
}
