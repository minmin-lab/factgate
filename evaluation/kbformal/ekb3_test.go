package kbformal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummarizeEKB3GroupsAndOrderStatistics(t *testing.T) {
	var samples []EKB3Sample
	for i := 1; i <= 20; i++ {
		samples = append(samples, EKB3Sample{Op: "search", Arm: "factgate", K: 5, ClientMS: float64(i), DeliveredMS: float64(10 * i),
			Components: map[string]float64{"settle": float64(i)}})
	}
	samples = append(samples, EKB3Sample{Op: "search", Arm: "factgate", K: 5, Error: "EXPOSURE_BUDGET_EXHAUSTED: x"},
		EKB3Sample{Op: "search", Arm: "factgate", K: 5, Replay: true, ClientMS: 7},
		EKB3Sample{Op: "aggregate", Arm: "direct", ClientMS: 3})
	summary := SummarizeEKB3(samples)
	novel := summary["search/factgate/k5/novel"]
	if novel.N != 21 || novel.Errors != 1 || novel.P50 != 11 || novel.P95 != 20 || novel.Mean != 10.5 ||
		novel.DeliveredP50 != 110 || novel.DeliveredP95 != 200 || novel.Components["settle"] != 11 {
		t.Fatalf("novel cell %+v", novel)
	}
	if summary["search/factgate/k5/replay"].N != 1 || summary["aggregate/direct/novel"].P50 != 3 || len(summary) != 3 {
		t.Fatalf("groups %+v", summary)
	}
}

func TestCheckChunkRow(t *testing.T) {
	sum := sha256.Sum256([]byte("差旅标准"))
	good := []any{"D-1", int32(1), int32(0), hex.EncodeToString(sum[:]), "差旅标准"}
	if err := checkChunkRow(good); err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string][]any{
		"tampered text": {"D-1", int32(1), int32(0), hex.EncodeToString(sum[:]), "差旅标准 "},
		"four columns":  {"D-1", int32(1), int32(0), hex.EncodeToString(sum[:])},
		"text not text": {"D-1", int32(1), int32(0), hex.EncodeToString(sum[:]), 7},
	} {
		if checkChunkRow(row) == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestRequestLogRecordsCallsWithoutCredentials(t *testing.T) {
	const token = "bearer-token-that-must-not-be-logged"
	const signedURL = "http://objects.invalid/result.parquet?X-Amz-Signature=must-not-be-logged"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		content := map[string]any{"task_id": "task_1", "query_id": "q_1", "status": "COMPLETED", "download_url": signedURL,
			"exposure": map[string]any{"charged_release_facts": 4}, "receipt": map[string]any{"version": "x"}}
		result := map[string]any{"structuredContent": content}
		if request.Params.Name == "fetch" {
			result = map[string]any{"isError": true, "structuredContent": map[string]any{"error": map[string]any{"code": "CHUNK_NOT_RETURNED", "message": "m", "reason": "r"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	requests, err := CreateRequestLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateRequestLog(path); err == nil {
		t.Fatal("a second log over the same path was created")
	}
	client := NewMCPClient(server.URL, token, 5*time.Second)
	client.Label, client.Log = "alice", requests
	ctx := context.Background()
	if _, err := client.Call(ctx, "search", map[string]any{"task_id": "task_1", "request_id": "r1", "k": 5, "query": map[string]any{"vector": []float64{0.25, 0.5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "fetch", map[string]any{"task_id": "task_1", "request_id": "r2"}); err == nil {
		t.Fatal("the tool error was not returned")
	}
	if _, err := client.Call(ctx, "get_task_status", map[string]string{"task_id": "task_1"}); err != nil {
		t.Fatal(err)
	}
	if err := requests.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control for the leak checks below: the probe finds what is there.
	if !strings.Contains(string(data), "CHUNK_NOT_RETURNED") {
		t.Fatal("the log lacks the tool error code")
	}
	for _, secret := range []string{token, "X-Amz-Signature", "must-not-be-logged", "0.25"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("the request log contains %q", secret)
		}
	}
	var entries []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != 3 || entries[0]["seq"] != 1.0 || entries[2]["seq"] != 3.0 {
		t.Fatalf("entries %v", entries)
	}
	first, second, third := entries[0], entries[1], entries[2]
	if first["principal"] != "alice" || first["tool"] != "search" || first["request_id"] != "r1" || first["outcome"] != "ok" ||
		first["response_query_id"] != "q_1" || first["receipt_json_sha256"] == nil || first["arguments_sha256"] == "" || first["response_exposure"] == nil {
		t.Fatalf("search entry %v", first)
	}
	if second["outcome"] != "tool_error" || second["code"] != "CHUNK_NOT_RETURNED" || second["reason"] != "r" {
		t.Fatalf("fetch entry %v", second)
	}
	if third["task_id"] != "task_1" || third["response_status"] != "COMPLETED" {
		t.Fatalf("status entry %v", third)
	}
}
