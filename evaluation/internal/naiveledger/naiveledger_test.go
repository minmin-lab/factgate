package naiveledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func hashes(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", prefix, i)))
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	dsn := os.Getenv("B5_TEST_DSN")
	if dsn == "" {
		t.Skip("B5_TEST_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	l, err := Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if l, err = Open(ctx, db); err != nil {
		t.Fatal(err)
	}
	return l
}

// The semantics under test: set-difference novelty against the committed
// history, per-dimension "used + novel <= max", refusal rolls back everything.
func TestSettleNoveltyBudgetAndRefusal(t *testing.T) {
	l := openTestLedger(t)
	ctx := context.Background()
	if err := l.CreateRoot(ctx, "root-a", Limits{Release: 5, Influence: 14, Outcome: 3}); err != nil {
		t.Fatal(err)
	}
	inf := hashes("inf", 20)
	// q1: 10 influence facts, 2 release, 1 outcome -> all novel.
	c, _, err := l.Settle(ctx, "root-a", Observation{QueryID: "q1", Release: hashes("rel", 2), Influence: inf[:10], Outcome: hashes("out", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Release != 2 || c.Influence != 10 || c.Outcome != 1 || c.Epoch != 1 {
		t.Fatalf("q1 charge %+v", c)
	}
	// q2: influence {0..13} overlaps 10 -> novel 4; same release facts -> novel 0.
	c, _, err = l.Settle(ctx, "root-a", Observation{QueryID: "q2", Release: hashes("rel", 2), Influence: inf[:14], Outcome: hashes("out2", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Release != 0 || c.Influence != 4 || c.Outcome != 1 || c.Epoch != 2 {
		t.Fatalf("q2 charge %+v", c)
	}
	// q3: one more influence fact -> 15 > 14: refused, and nothing persisted.
	before, err := l.ReadStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = l.Settle(ctx, "root-a", Observation{QueryID: "q3", Influence: inf[14:15]})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("q3 expected refusal, got %v", err)
	}
	after, err := l.ReadStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.FactRows != before.FactRows || before.FactRows != 2+10+1+4+1 {
		t.Fatalf("refusal must persist nothing: before %d after %d", before.FactRows, after.FactRows)
	}
	var usedI, epoch int64
	if err := l.db.QueryRowContext(ctx, `SELECT used_influence, epoch FROM b5_naive_roots WHERE root_id='root-a'`).Scan(&usedI, &epoch); err != nil {
		t.Fatal(err)
	}
	if usedI != 14 || epoch != 2 {
		t.Fatalf("root counters after refusal: used_influence=%d epoch=%d", usedI, epoch)
	}
	// q4: a zero-novelty replay of q2's facts settles with zero charge.
	c, _, err = l.Settle(ctx, "root-a", Observation{QueryID: "q4", Influence: inf[:14]})
	if err != nil || c.Influence != 0 || c.Epoch != 3 {
		t.Fatalf("q4 zero-novelty: %+v %v", c, err)
	}
}

// The reviewer's counter-example for the withdrawn sweep curve, run through the
// naive ledger: {1..10}, {1..14}, then five singletons under an influence
// budget of 5 admits five statements, not two.
func TestCounterExampleAdmitsFive(t *testing.T) {
	l := openTestLedger(t)
	ctx := context.Background()
	if err := l.CreateRoot(ctx, "root-b", Limits{Release: 1 << 30, Influence: 5, Outcome: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	all := hashes("f", 20)
	sets := [][]string{all[1:11], all[1:15], all[15:16], all[16:17], all[17:18], all[18:19], all[19:20]}
	admitted := 0
	for i, s := range sets {
		_, _, err := l.Settle(ctx, "root-b", Observation{QueryID: fmt.Sprint("s", i), Influence: s})
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrBudgetExhausted):
		default:
			t.Fatal(err)
		}
	}
	if admitted != 5 {
		t.Fatalf("admitted %d, want 5", admitted)
	}
}
