package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Client.WithdrawDraft posts the shared DraftWithdrawal to the draft's
// withdraw route with the service token; 2xx and 404 are success, anything
// else is an error.
func TestClientWithdrawDraftPostsTheSharedBodyWithTheServiceToken(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	var bodies []DraftWithdrawal
	status := http.StatusOK
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body DraftWithdrawal
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			t.Errorf("decode withdrawal: %v", err)
		}
		mu.Lock()
		seen, bodies = append(seen, r), append(bodies, body)
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer oa.Close()
	client, err := NewClient(oa.URL+"/", "service-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	withdrawal := DraftWithdrawal{TaskID: "task_1", DraftID: "oa_abc", Reason: WithdrawalRevoked, Actor: "staff", OccurredAt: at}
	if err := client.WithdrawDraft(context.Background(), withdrawal); err != nil {
		t.Fatalf("a 200 answer: %v", err)
	}
	mu.Lock()
	request, body := seen[0], bodies[0]
	mu.Unlock()
	if request.Method != http.MethodPost || request.URL.Path != "/api/drafts/oa_abc/withdraw" ||
		request.Header.Get("Authorization") != "Bearer service-token" || !strings.HasPrefix(request.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("withdrawal request %s %s auth-ok=%v content-type %q", request.Method, request.URL.Path,
			request.Header.Get("Authorization") == "Bearer service-token", request.Header.Get("Content-Type"))
	}
	if body != withdrawal {
		t.Fatalf("withdrawal body %+v, want %+v", body, withdrawal)
	}

	for code, wantErr := range map[int]bool{http.StatusNoContent: false, http.StatusNotFound: false,
		http.StatusInternalServerError: true, http.StatusUnauthorized: true, http.StatusConflict: true} {
		mu.Lock()
		status = code
		mu.Unlock()
		err := client.WithdrawDraft(context.Background(), DraftWithdrawal{TaskID: "task_1", DraftID: "oa_abc", Reason: WithdrawalExpired, Actor: "system", OccurredAt: at})
		if (err != nil) != wantErr {
			t.Fatalf("status %d: error %v, want error %v", code, err, wantErr)
		}
	}

	mu.Lock()
	before := len(seen)
	mu.Unlock()
	for _, bad := range []DraftWithdrawal{{TaskID: "task_1", Reason: WithdrawalRevoked}, {DraftID: "oa_abc", Reason: WithdrawalRevoked},
		{TaskID: "task_1", DraftID: "oa_abc", Reason: "completed"}} {
		if err := client.WithdrawDraft(context.Background(), bad); err == nil {
			t.Fatalf("an incomplete withdrawal %+v was sent", bad)
		}
	}
	mu.Lock()
	after := len(seen)
	mu.Unlock()
	if after != before {
		t.Fatalf("incomplete withdrawals reached the OA: %d requests", after-before)
	}
}

// An unreachable OA is an error, not a success.
func TestClientWithdrawDraftReportsAnUnreachableOA(t *testing.T) {
	oa := httptest.NewServer(http.NotFoundHandler())
	base := oa.URL
	oa.Close()
	client, err := NewClient(base, "service-token", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WithdrawDraft(context.Background(), DraftWithdrawal{TaskID: "task_1", DraftID: "oa_abc", Reason: WithdrawalRevoked,
		Actor: "staff", OccurredAt: time.Now()}); err == nil {
		t.Fatal("an unreachable OA reported success")
	}
}
