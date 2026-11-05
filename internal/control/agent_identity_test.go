package control

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// The demo's switchable agent identities (KB1-LOOP-151, migration 023).

func agentIdentityHistory(t *testing.T, store *Store, label string) [][3]string {
	t.Helper()
	rows, err := store.db.Query(`SELECT COALESCE(from_principal_id, ''), to_principal_id, switched_by FROM demo_agent_identity_switches WHERE label = $1 ORDER BY id`, label)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var history [][3]string
	for rows.Next() {
		var entry [3]string
		if err := rows.Scan(&entry[0], &entry[1], &entry[2]); err != nil {
			t.Fatal(err)
		}
		history = append(history, entry)
	}
	return history
}

func TestAgentIdentityRegisterSwitchAndLookup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 51), WithClock(fixedClock{value: now}))
	for _, principal := range []Principal{
		{ID: "principal_staff", Subject: "staff", Role: "query"},
		{ID: "principal_finance_admin", Subject: "finance-admin", Role: "query"},
		{ID: "principal_auditor", Subject: "auditor", Role: "auditor"},
	} {
		if err := store.CreatePrincipal(ctx, principal); err != nil {
			t.Fatal(err)
		}
	}
	digestA, digestB := strings.Repeat("a", 64), strings.Repeat("b", 64)

	if err := store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "lzj", TokenSHA256: digestA, PrincipalID: "principal_staff", BoundBy: "wmm"}); err != nil {
		t.Fatal(err)
	}
	registered, err := store.LookupAgentIdentityByToken(ctx, digestA)
	want := AgentIdentity{Label: "lzj", TokenSHA256: digestA, PrincipalID: "principal_staff", BoundBy: "wmm", BoundAt: now, CreatedAt: now}
	if err != nil || registered != want {
		t.Fatalf("lookup %+v %v, want %+v", registered, err, want)
	}

	// Re-registering rebinds the digest and principal and keeps created_at.
	later := now.Add(time.Hour)
	if err := store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "lzj", TokenSHA256: digestB, PrincipalID: "principal_finance_admin", BoundBy: "ops", BoundAt: later}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LookupAgentIdentityByToken(ctx, digestA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the replaced digest still resolves: %v", err)
	}
	rebound, err := store.GetAgentIdentity(ctx, "lzj")
	if err != nil || rebound.TokenSHA256 != digestB || rebound.PrincipalID != "principal_finance_admin" || !rebound.BoundAt.Equal(later) || !rebound.CreatedAt.Equal(now) {
		t.Fatalf("rebound %+v %v", rebound, err)
	}

	switchedAt := later.Add(time.Minute)
	switched, err := store.SwitchAgentIdentity(ctx, "lzj", "principal_staff", "procurement-admin", switchedAt)
	if err != nil || switched.PrincipalID != "principal_staff" || switched.BoundBy != "procurement-admin" || !switched.BoundAt.Equal(switchedAt) ||
		switched.TokenSHA256 != digestB || !switched.CreatedAt.Equal(now) {
		t.Fatalf("switch %+v %v", switched, err)
	}
	wantHistory := [][3]string{{"", "principal_staff", "wmm"}, {"principal_staff", "principal_finance_admin", "ops"}, {"principal_finance_admin", "principal_staff", "procurement-admin"}}
	if got := agentIdentityHistory(t, store, "lzj"); !reflect.DeepEqual(got, wantHistory) {
		t.Fatalf("history %v, want %v", got, wantHistory)
	}

	if err := store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "abc", TokenSHA256: digestA, PrincipalID: "principal_staff", BoundBy: "wmm"}); err != nil {
		t.Fatal(err)
	}
	identities, err := store.ListAgentIdentities(ctx)
	if err != nil || len(identities) != 2 || identities[0].Label != "abc" || identities[1].Label != "lzj" {
		t.Fatalf("list %+v %v", identities, err)
	}

	// Refusals change nothing.
	for name, test := range map[string]struct {
		err  error
		kind error
	}{
		"digest held by another label": {store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "other", TokenSHA256: digestB, PrincipalID: "principal_staff", BoundBy: "wmm"}), ErrConflict},
		"unknown principal":            {store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "other", TokenSHA256: strings.Repeat("c", 64), PrincipalID: "principal_nobody", BoundBy: "wmm"}), ErrNotFound},
		"invalid label":                {store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "Upper", TokenSHA256: strings.Repeat("c", 64), PrincipalID: "principal_staff", BoundBy: "wmm"}), ErrInvalid},
		"uppercase digest":             {store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "other", TokenSHA256: strings.Repeat("C", 64), PrincipalID: "principal_staff", BoundBy: "wmm"}), ErrInvalid},
		"no binder":                    {store.RegisterAgentIdentity(ctx, AgentIdentity{Label: "other", TokenSHA256: strings.Repeat("c", 64), PrincipalID: "principal_staff"}), ErrInvalid},
	} {
		if !errors.Is(test.err, test.kind) {
			t.Fatalf("%s: %v, want %v", name, test.err, test.kind)
		}
	}
	if _, err := store.SwitchAgentIdentity(ctx, "nobody", "principal_staff", "procurement-admin", switchedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("switching an unknown label: %v", err)
	}
	if _, err := store.SwitchAgentIdentity(ctx, "lzj", "principal_nobody", "procurement-admin", switchedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("switching to an unknown principal: %v", err)
	}
	if _, err := store.SwitchAgentIdentity(ctx, "lzj", "principal_staff", " ", switchedAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("switching without an actor: %v", err)
	}
	if _, err := store.LookupAgentIdentityByToken(ctx, strings.Repeat("d", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup of an unregistered digest: %v", err)
	}
	if got := agentIdentityHistory(t, store, "lzj"); !reflect.DeepEqual(got, wantHistory) {
		t.Fatalf("a refusal changed the history: %v", got)
	}
	if identities, err := store.ListAgentIdentities(ctx); err != nil || len(identities) != 2 {
		t.Fatalf("a refusal changed the bindings: %+v %v", identities, err)
	}

	// The table's own CHECKs hold the same line for writers that bypass the store.
	if _, err := store.db.Exec(`INSERT INTO demo_agent_identities(label, token_sha256, principal_id, bound_by, bound_at, created_at) VALUES ('Bad Label', $1, 'principal_staff', 'x', now(), now())`, strings.Repeat("e", 64)); err == nil {
		t.Fatal("the label CHECK admitted an invalid label")
	}
	if _, err := store.db.Exec(`INSERT INTO demo_agent_identities(label, token_sha256, principal_id, bound_by, bound_at, created_at) VALUES ('ok', 'not-a-digest', 'principal_staff', 'x', now(), now())`); err == nil {
		t.Fatal("the digest CHECK admitted an invalid digest")
	}
}

func TestListPrincipalsOrdersBySubject(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 52), WithClock(fixedClock{value: now}))
	for _, principal := range []Principal{
		{ID: "p3", Subject: "zed", Role: "query"},
		{ID: "p1", Subject: "alice", Role: "query"},
		{ID: "p2", Subject: "carol", Role: "auditor"},
	} {
		if err := store.CreatePrincipal(ctx, principal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DisablePrincipal(ctx, "p3", now); err != nil {
		t.Fatal(err)
	}
	principals, err := store.ListPrincipals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, principal := range principals {
		subjects = append(subjects, principal.Subject)
	}
	if !reflect.DeepEqual(subjects, []string{"alice", "carol", "zed"}) || principals[2].DisabledAt == nil || principals[1].Role != "auditor" {
		t.Fatalf("principals %+v", principals)
	}
}
