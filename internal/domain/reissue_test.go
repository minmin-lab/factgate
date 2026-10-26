package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestReissuedRebasesOnlyTheExpiry(t *testing.T) {
	first := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	later := first.Add(7 * time.Minute)
	core := testGrantCoreV1(t, first)
	again := core.Reissued(later)
	if !again.ExpiresAt.Equal(later.Add(900 * time.Second)) {
		t.Fatalf("re-issued expiry %v, want issue time plus the core's TTL", again.ExpiresAt)
	}
	// Re-issuing at the original time reproduces the core exactly.
	if !reflect.DeepEqual(core.Reissued(first), core) {
		t.Fatal("re-issuing at the original issue time changed the core")
	}
	// A re-issued core narrows its source once both count from one time.
	if err := core.Reissued(later).CheckNarrowing(again); err != nil {
		t.Fatalf("a re-issued core does not narrow its re-issued source: %v", err)
	}
	again.ApprovedColumns["expense_detail"][0] = "mutated"
	if core.ApprovedColumns["expense_detail"][0] == "mutated" {
		t.Fatal("the re-issued core shares column slices with its source")
	}
}
