package gateway

import (
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
)

// TestSecondQueryWhileOneIsInFlightIsRetryable: the task's one in-flight slot
// refuses a second reservation with retry details, records nothing for it,
// and the same request id succeeds once the first query has returned.
func TestSecondQueryWhileOneIsInFlightIsRetryable(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-in-flight-slot")
	started := make(chan struct{})
	release := make(chan struct{})
	harness.connector.started = started
	harness.connector.release = release

	finished := make(chan error, 1)
	go func() {
		_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
			"task_id": "task-in-flight-slot", "request_id": "slot-first", "sql": testSummarySQL,
		})
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first query did not reach the connector")
	}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-in-flight-slot", "request_id": "slot-second", "sql": testSummarySQL,
	})
	details := requireToolDetails(t, err, apierr.CodeConflict)
	if details["reason"] != "QUERY_IN_PROGRESS" || details["retryable"] != true || details["same_request_id_ok"] != true {
		t.Fatalf("in-flight refusal details %v", details)
	}

	// A closed release lets every later query through as well.
	close(release)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("first query failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first query did not settle")
	}
	retried := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-in-flight-slot", "request_id": "slot-second", "sql": testSummarySQL,
	})
	if retried["status"] != control.QueryCompleted || retried["idempotent_replay"] == true {
		t.Fatalf("retry with the refused request id did not run as a new query: %#v", retried)
	}
}
