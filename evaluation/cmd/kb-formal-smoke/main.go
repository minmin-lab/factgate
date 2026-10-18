// Command kb-formal-smoke exercises one fresh knowledge-product deployment
// end to end over its public interfaces (K4.10 acceptance): a real task
// request approved through the OA, a search whose receipt verifies against the
// gateway's published keyring and whose delivered text matches the released
// digests, the returned-chunk rule on fetch, eligibility for file principals,
// and the document download task (docs/kb-design.md §12): two documents of the
// search hits requested and approved, downloaded with verified part receipts,
// fetched again through the download capability, and the refusals across
// task kinds and outside the approved list. It prints one JSON summary and
// exits non-zero on any failure.
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
	"io"
	"net/http"
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
	knowledgeProduct := flag.String("knowledge-product", "", "the knowledge product to exercise when the Catalog declares several")
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
	var product catalog.KnowledgeProduct
	switch {
	case *knowledgeProduct != "":
		found := false
		if product, found = profile.LookupKnowledgeProduct(*knowledgeProduct); !found {
			return fmt.Errorf("the Catalog declares no knowledge product %q", *knowledgeProduct)
		}
	case len(profile.KnowledgeProducts) == 1:
		product = profile.KnowledgeProducts[0]
	default:
		return errors.New("profile catalog must declare one knowledge product, or name one with -knowledge-product")
	}
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
	if err := smokeDownload(ctx, summary, provisioner, alice, verifier, profile, product, taskID, rows, timeout); err != nil {
		return fmt.Errorf("download task: %w", err)
	}
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

// smokeDownload is the deployed-path check of the document download task.
func smokeDownload(ctx context.Context, summary map[string]any, provisioner *kbformal.Provisioner, alice *kbformal.MCPClient,
	verifier kbformal.ReceiptVerifier, profile *catalog.Catalog, product catalog.KnowledgeProduct, searchTask string, rows [][]any,
	timeout time.Duration) error {
	type ref struct {
		DocID   string `json:"doc_id"`
		Version any    `json:"version"`
	}
	var documents []ref
	seen := map[string]bool{}
	for _, row := range rows {
		key := fmt.Sprint(row[0]) + "@" + fmt.Sprint(row[1])
		if !seen[key] && len(documents) < 2 {
			seen[key] = true
			documents = append(documents, ref{DocID: fmt.Sprint(row[0]), Version: row[1]})
		}
	}
	taskID, created, err := provisioner.ProvisionDownload(ctx, map[string]any{"objective": "K7.3 smoke: download two search hits",
		"knowledge_product": product.Name, "documents": documents})
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	summary["download_task_documents"] = created["documents"]
	summary["download_budget_profile"] = created["budget_profile"]
	var files []map[string]any
	for index, document := range documents {
		result, err := alice.Call(ctx, "download_document", map[string]any{"task_id": taskID, "request_id": fmt.Sprintf("smoke-download-%d", index),
			"knowledge_product": product.Name, "doc_id": document.DocID, "version": document.Version})
		if err != nil {
			return fmt.Errorf("download %s: %w", document.DocID, err)
		}
		content, _ := result["content"].(string)
		sum := sha256.Sum256([]byte(content))
		if content == "" || hex.EncodeToString(sum[:]) != result["file_sha256"] {
			return fmt.Errorf("download %s: the file does not hash to file_sha256", document.DocID)
		}
		parts, _ := result["queries"].([]any)
		if len(parts) < 2 {
			return fmt.Errorf("download %s: %d settled parts, want document and chunks at least", document.DocID, len(parts))
		}
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			receipt, err := kbformal.VerifyReceipt(verifier, part["receipt"])
			if err != nil {
				return fmt.Errorf("download %s part %v receipt: %w", document.DocID, part["part"], err)
			}
			if receipt.CatalogDigest != profile.SHA256 {
				return fmt.Errorf("download %s part %v receipt names catalog %s", document.DocID, part["part"], receipt.CatalogDigest)
			}
		}
		downloadURL, _ := result["download_url"].(string)
		if downloadURL == "" {
			return fmt.Errorf("download %s: no download_url", document.DocID)
		}
		fetched, digestHeader, err := fetchFile(ctx, downloadURL, timeout)
		if err != nil {
			return fmt.Errorf("download %s through its capability: %w", document.DocID, err)
		}
		if string(fetched) != content {
			return fmt.Errorf("download %s: the capability served a different file", document.DocID)
		}
		chunks, _ := result["chunks"].([]any)
		files = append(files, map[string]any{"doc": fmt.Sprintf("%s@%v", document.DocID, document.Version), "file_sha256": result["file_sha256"],
			"file_bytes": result["file_bytes"], "chunks": len(chunks), "parts": len(parts), "content_digest_header": digestHeader != ""})
	}
	summary["download_files"] = files
	var toolErr *kbformal.ToolError
	_, err = alice.Call(ctx, "download_document", map[string]any{"task_id": taskID, "request_id": "smoke-download-unlisted",
		"knowledge_product": product.Name, "doc_id": "NOT-LISTED", "version": 1})
	if !errors.As(err, &toolErr) || toolErr.Code != "DOWNLOAD_NOT_APPROVED" {
		return fmt.Errorf("download of an unlisted document: want DOWNLOAD_NOT_APPROVED, got %v", err)
	}
	summary["download_unlisted_code"] = toolErr.Code
	_, err = alice.Call(ctx, "search", map[string]any{"task_id": taskID, "request_id": "smoke-download-search",
		"knowledge_product": product.Name, "query": map[string]any{"text": "差旅"}, "k": 1})
	if !errors.As(err, &toolErr) || toolErr.Code != "TASK_KIND_MISMATCH" {
		return fmt.Errorf("search in a download task: want TASK_KIND_MISMATCH, got %v", err)
	}
	_, err = alice.Call(ctx, "download_document", map[string]any{"task_id": searchTask, "request_id": "smoke-search-download",
		"knowledge_product": product.Name, "doc_id": documents[0].DocID, "version": documents[0].Version})
	if !errors.As(err, &toolErr) || toolErr.Code != "TASK_KIND_MISMATCH" {
		return fmt.Errorf("download in a search task: want TASK_KIND_MISMATCH, got %v", err)
	}
	summary["task_kind_mismatch_code"] = toolErr.Code
	// The original-file channel (docs/kb-design.md §12.8): refused in a task
	// without the right, delivered byte for byte in one with it.
	_, err = alice.Call(ctx, "download_document", map[string]any{"task_id": taskID, "request_id": "smoke-original-unapproved",
		"knowledge_product": product.Name, "doc_id": documents[0].DocID, "version": documents[0].Version, "format": "original"})
	if !errors.As(err, &toolErr) || toolErr.Code != "DOWNLOAD_NOT_APPROVED" {
		return fmt.Errorf("original file without the right: want DOWNLOAD_NOT_APPROVED, got %v", err)
	}
	originalTask, originalCreated, err := provisioner.ProvisionDownload(ctx, map[string]any{"objective": "K7.2b smoke: original file",
		"knowledge_product": product.Name, "documents": documents[:1], "original_file": true})
	if err != nil {
		return fmt.Errorf("provision original-file task: %w", err)
	}
	if originalCreated["original_file"] != true {
		return errors.New("request_download_task did not record the original-file right")
	}
	original, err := alice.Call(ctx, "download_document", map[string]any{"task_id": originalTask, "request_id": "smoke-original-1",
		"knowledge_product": product.Name, "doc_id": documents[0].DocID, "version": documents[0].Version, "format": "original"})
	if err != nil {
		return fmt.Errorf("original file: %w", err)
	}
	content, _ := original["content"].(string)
	sum := sha256.Sum256([]byte(content))
	if content == "" || original["format"] != "original" || hex.EncodeToString(sum[:]) != original["file_sha256"] {
		return errors.New("original file: content, format or digest is wrong")
	}
	for _, raw := range original["queries"].([]any) {
		part, _ := raw.(map[string]any)
		if _, err := kbformal.VerifyReceipt(verifier, part["receipt"]); err != nil {
			return fmt.Errorf("original file part %v receipt: %w", part["part"], err)
		}
	}
	downloadURL, _ := original["download_url"].(string)
	fetched, _, err := fetchFile(ctx, downloadURL, timeout)
	if err != nil || string(fetched) != content {
		return fmt.Errorf("original file through its capability: %v", err)
	}
	summary["original_file"] = map[string]any{"doc": fmt.Sprintf("%s@%v", documents[0].DocID, documents[0].Version),
		"file_sha256": original["file_sha256"], "file_bytes": original["file_bytes"], "parts": len(original["queries"].([]any)),
		"unapproved_code": "DOWNLOAD_NOT_APPROVED"}
	return nil
}

// fetchFile GETs a download capability and returns the body and its
// Content-Digest header.
func fetchFile(ctx context.Context, target string, timeout time.Duration) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	return body, response.Header.Get("Content-Digest"), err
}
