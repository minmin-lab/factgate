// Package naiveledger is the P10-R2.E1 (B5 arm iii) reference implementation of
// the exposure ledger's semantics with the simplest correct representation:
// one PostgreSQL row per (root, dimension, fact hash), novelty by insert
// conflict, the per-dimension rule used + novel <= max checked and applied in
// the same transaction that persists the facts, and a refusal that rolls the
// whole transaction back so nothing is charged.
//
// It exists to be measured against the optimised ledger (ordinal
// dictionaries, Roaring bitmaps, Merkle-radix outcome set, CAS head) on
// byte-identical fact-hash inputs. It is evaluation code and never touches the
// frozen measured paths; it is not a component of the Gateway.
//
// What it deliberately shares with the production rule
// (internal/control/ordinal_exposure_v5.go settleV5ExposureMeasuredTx):
//   - fact identity is the 32-byte fact hash;
//   - three dimensions with independent limits per root;
//   - novelty is the set difference against the root's committed history;
//   - the budget check is "used + novel <= max" per dimension, decided inside
//     the settlement transaction under a lock on the root;
//   - an exhausted budget refuses the whole query: nothing persists;
//   - a settled query commits its facts, its observation row and the root
//     counters atomically.
//
// What it does not reproduce: the optimistic CAS head (this ledger locks the
// root row with SELECT ... FOR UPDATE), ordinal dictionaries, segment bitmaps,
// or the radix set; those are exactly the representation choices under test.
package naiveledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Dimension names, in the paper's order.
const (
	Release   = "release"
	Influence = "influence"
	Outcome   = "outcome"
)

var dimensions = []string{Release, Influence, Outcome}

// ErrBudgetExhausted is the naive ledger's refusal; the transaction that
// produced it has been rolled back.
var ErrBudgetExhausted = errors.New("naive ledger: exposure budget exhausted")

// Limits are the per-root maxima.
type Limits struct{ Release, Influence, Outcome int64 }

// Observation is one query's candidate fact sets, as lowercase hex SHA-256
// strings. Duplicates within a dimension are tolerated and count once.
type Observation struct {
	QueryID   string
	Release   []string
	Influence []string
	Outcome   []string
}

// Charge is what a settled query was charged: the novel facts per dimension.
type Charge struct {
	Release, Influence, Outcome int64
	Epoch                       int64
}

// Metrics are the wall-clock phases of one settlement, measured to commit.
type Metrics struct {
	Lock, Insert, Commit, Total time.Duration
	RowsInserted                int64
}

// Schema creates the ledger tables. It is idempotent.
const Schema = `
CREATE TABLE IF NOT EXISTS b5_naive_roots (
    root_id        text PRIMARY KEY,
    epoch          bigint NOT NULL DEFAULT 0,
    max_release    bigint NOT NULL,
    max_influence  bigint NOT NULL,
    max_outcome    bigint NOT NULL,
    used_release   bigint NOT NULL DEFAULT 0,
    used_influence bigint NOT NULL DEFAULT 0,
    used_outcome   bigint NOT NULL DEFAULT 0,
    CHECK (used_release <= max_release AND used_influence <= max_influence AND used_outcome <= max_outcome)
);
CREATE TABLE IF NOT EXISTS b5_naive_facts (
    root_id     text  NOT NULL REFERENCES b5_naive_roots(root_id),
    dimension   text  NOT NULL,
    fact_sha256 bytea NOT NULL,
    PRIMARY KEY (root_id, dimension, fact_sha256)
);
CREATE TABLE IF NOT EXISTS b5_naive_observations (
    root_id            text   NOT NULL REFERENCES b5_naive_roots(root_id),
    query_id           text   NOT NULL,
    epoch              bigint NOT NULL,
    observation_sha256 bytea  NOT NULL,
    novel_release      bigint NOT NULL,
    novel_influence    bigint NOT NULL,
    novel_outcome      bigint NOT NULL,
    settled_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (root_id, query_id)
);
`

// Ledger settles observations against roots in one PostgreSQL database.
type Ledger struct{ db *sql.DB }

// Open prepares the schema on db.
func Open(ctx context.Context, db *sql.DB) (*Ledger, error) {
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		return nil, fmt.Errorf("naive ledger schema: %w", err)
	}
	return &Ledger{db: db}, nil
}

// CreateRoot registers a root with its limits.
func (l *Ledger) CreateRoot(ctx context.Context, rootID string, limits Limits) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO b5_naive_roots (root_id, max_release, max_influence, max_outcome) VALUES ($1,$2,$3,$4)`,
		rootID, limits.Release, limits.Influence, limits.Outcome)
	return err
}

// Settle charges obs against rootID in one transaction: lock the root, insert
// the candidate facts per dimension (novelty = rows the insert did not find),
// refuse and roll back if any dimension would exceed its maximum, otherwise
// update the counters, record the observation and commit.
func (l *Ledger) Settle(ctx context.Context, rootID string, obs Observation) (Charge, Metrics, error) {
	started := time.Now()
	var m Metrics
	tx, err := l.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Charge{}, m, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	var epoch, maxR, maxI, maxO, usedR, usedI, usedO int64
	if err := tx.QueryRowContext(ctx, `SELECT epoch, max_release, max_influence, max_outcome, used_release, used_influence, used_outcome
		FROM b5_naive_roots WHERE root_id = $1 FOR UPDATE`, rootID).Scan(&epoch, &maxR, &maxI, &maxO, &usedR, &usedI, &usedO); err != nil {
		return Charge{}, m, fmt.Errorf("lock root: %w", err)
	}
	m.Lock = time.Since(started)
	insertStarted := time.Now()
	novel := map[string]int64{}
	for _, dim := range dimensions {
		hashes, err := decodeHashes(obs.hashes(dim))
		if err != nil {
			return Charge{}, m, err
		}
		n, err := insertFacts(ctx, tx, rootID, dim, hashes)
		if err != nil {
			return Charge{}, m, err
		}
		novel[dim] = n
		m.RowsInserted += n
	}
	m.Insert = time.Since(insertStarted)
	if usedR+novel[Release] > maxR || usedI+novel[Influence] > maxI || usedO+novel[Outcome] > maxO {
		// The deferred rollback discards the inserted rows: nothing is charged.
		m.Total = time.Since(started)
		return Charge{}, m, ErrBudgetExhausted
	}
	commitStarted := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE b5_naive_roots SET epoch = epoch + 1, used_release = used_release + $2,
		used_influence = used_influence + $3, used_outcome = used_outcome + $4 WHERE root_id = $1`,
		rootID, novel[Release], novel[Influence], novel[Outcome]); err != nil {
		return Charge{}, m, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO b5_naive_observations (root_id, query_id, epoch, observation_sha256, novel_release, novel_influence, novel_outcome)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, rootID, obs.QueryID, epoch+1, obs.digest(), novel[Release], novel[Influence], novel[Outcome]); err != nil {
		return Charge{}, m, err
	}
	if err := tx.Commit(); err != nil {
		return Charge{}, m, err
	}
	m.Commit = time.Since(commitStarted)
	m.Total = time.Since(started)
	return Charge{Release: novel[Release], Influence: novel[Influence], Outcome: novel[Outcome], Epoch: epoch + 1}, m, nil
}

// insertFacts inserts hashes for one dimension in batches and returns how many
// rows were new (the set difference against the committed history plus what
// this transaction already inserted).
func insertFacts(ctx context.Context, tx *sql.Tx, rootID, dim string, hashes [][]byte) (int64, error) {
	const batch = 5000
	var novel int64
	for start := 0; start < len(hashes); start += batch {
		end := start + batch
		if end > len(hashes) {
			end = len(hashes)
		}
		var sb strings.Builder
		sb.WriteString(`INSERT INTO b5_naive_facts (root_id, dimension, fact_sha256) VALUES `)
		args := make([]any, 0, 2+(end-start))
		args = append(args, rootID, dim)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteString(",")
			}
			args = append(args, hashes[i])
			fmt.Fprintf(&sb, "($1,$2,$%d)", len(args))
		}
		sb.WriteString(` ON CONFLICT DO NOTHING`)
		result, err := tx.ExecContext(ctx, sb.String(), args...)
		if err != nil {
			return 0, fmt.Errorf("insert %s facts: %w", dim, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		novel += n
	}
	return novel, nil
}

// Storage reports the ledger relations' sizes and row counts.
type Storage struct {
	FactRows                                   int64
	FactTableBytes, FactIndexBytes, TotalBytes int64
}

// ReadStorage sizes the ledger relations (pg_table_size / pg_indexes_size /
// pg_total_relation_size over the three tables).
func (l *Ledger) ReadStorage(ctx context.Context) (Storage, error) {
	var s Storage
	if err := l.db.QueryRowContext(ctx, `SELECT count(*) FROM b5_naive_facts`).Scan(&s.FactRows); err != nil {
		return s, err
	}
	if err := l.db.QueryRowContext(ctx, `SELECT pg_table_size('b5_naive_facts'), pg_indexes_size('b5_naive_facts'),
		pg_total_relation_size('b5_naive_facts') + pg_total_relation_size('b5_naive_roots') + pg_total_relation_size('b5_naive_observations')`).
		Scan(&s.FactTableBytes, &s.FactIndexBytes, &s.TotalBytes); err != nil {
		return s, err
	}
	return s, nil
}

// Vacuum reclaims the dead tuples that rolled-back (refused) settlements leave
// behind, so storage can be read both as the trace left it and after
// maintenance; VACUUM cannot run inside a transaction.
func (l *Ledger) Vacuum(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `VACUUM (ANALYZE) b5_naive_facts`)
	return err
}

// Reset drops the ledger tables.
func (l *Ledger) Reset(ctx context.Context) error {
	_, err := l.db.ExecContext(ctx, `DROP TABLE IF EXISTS b5_naive_observations, b5_naive_facts, b5_naive_roots`)
	return err
}

func (o Observation) hashes(dim string) []string {
	switch dim {
	case Release:
		return o.Release
	case Influence:
		return o.Influence
	default:
		return o.Outcome
	}
}

// digest is a canonical digest of the observation (sorted, deduplicated hashes
// per dimension), the analogue of observation_sha256 in the V5 ledger.
func (o Observation) digest() []byte {
	h := sha256.New()
	for _, dim := range dimensions {
		hs := append([]string(nil), o.hashes(dim)...)
		sort.Strings(hs)
		h.Write([]byte(dim))
		last := ""
		for _, x := range hs {
			if x == last {
				continue
			}
			last = x
			h.Write([]byte(x))
		}
	}
	return h.Sum(nil)
}

func decodeHashes(hexes []string) ([][]byte, error) {
	out := make([][]byte, 0, len(hexes))
	seen := make(map[string]struct{}, len(hexes))
	for _, x := range hexes {
		if _, dup := seen[x]; dup {
			continue
		}
		seen[x] = struct{}{}
		b, err := hex.DecodeString(x)
		if err != nil || len(b) != sha256.Size {
			return nil, fmt.Errorf("fact hash %q is not a 32-byte hex digest", x)
		}
		out = append(out, b)
	}
	return out, nil
}
