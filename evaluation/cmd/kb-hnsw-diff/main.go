// Command kb-hnsw-diff measures the hit-set deviation of an HNSW index from
// exact cosine retrieval on a published knowledge collection (plan K3.4).
//
// It samples query chunks deterministically from kb.chunks, runs the exact
// arm (full scan, index scans disabled) and the approximate arm (HNSW index,
// sequential scan disabled) in the same session, and reports identity rate,
// recall@k and Jaccard per (k, ef_search, iterative_scan, filter mode). The
// index it builds is dropped at the end unless -keep-index is given. It needs
// an administrative connection because it creates the index; it never writes
// rows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/kbhnsw"
)

const indexName = "kb_chunks_hnsw_diff"

type query struct {
	kbhnsw.Key
	Department     string
	Classification string
	Embedding      string
}

type configResult struct {
	K             int            `json:"k"`
	EFSearch      int            `json:"ef_search"`
	IterativeScan string         `json:"iterative_scan"`
	Filter        string         `json:"filter"`
	Summary       kbhnsw.Summary `json:"summary"`
	ExactMS       latency        `json:"exact_latency_ms"`
	ApproxMS      latency        `json:"approx_latency_ms"`
}

type latency struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
}

type report struct {
	Version        string         `json:"version"`
	GeneratedAt    string         `json:"generated_at"`
	Collection     string         `json:"collection"`
	Chunks         int            `json:"chunks"`
	Dimension      int            `json:"dimension"`
	PGVector       string         `json:"pgvector_version"`
	Queries        int            `json:"queries"`
	Seed           string         `json:"seed"`
	IndexM         int            `json:"index_m"`
	IndexEFBuild   int            `json:"index_ef_construction"`
	IndexBuildSecs float64        `json:"index_build_seconds"`
	IndexBytes     int64          `json:"index_bytes"`
	Results        []configResult `json:"results"`
}

func main() {
	dsnEnv := flag.String("dsn-env", "KB_ADMIN_POSTGRES_DSN", "environment variable holding the administrative DSN (CREATE INDEX on kb.chunks)")
	collection := flag.String("collection", "", "kb.chunks collection to measure (required)")
	queries := flag.Int("queries", 200, "number of query chunks sampled from the collection")
	seed := flag.String("seed", "kb-hnsw-diff-1", "sampling seed (hashed with text_sha256)")
	ks := flag.String("k", "5,10,20", "comma-separated k values")
	efs := flag.String("ef", "40,100,200", "comma-separated hnsw.ef_search values")
	iteratives := flag.String("iterative", "off,relaxed_order", "comma-separated hnsw.iterative_scan values")
	filters := flag.String("filters", "none,department,department+classification", "comma-separated filter modes")
	m := flag.Int("m", 16, "HNSW m")
	efBuild := flag.Int("ef-construction", 64, "HNSW ef_construction")
	maintenanceMem := flag.String("maintenance-work-mem", "1GB", "maintenance_work_mem for the index build")
	keep := flag.Bool("keep-index", false, "keep the HNSW index after the run")
	out := flag.String("out", "", "write the JSON report here (stdout summary is always printed)")
	flag.Parse()
	if err := run(*dsnEnv, *collection, *queries, *seed, *ks, *efs, *iteratives, *filters, *m, *efBuild, *maintenanceMem, *keep, *out); err != nil {
		fmt.Fprintln(os.Stderr, "kb-hnsw-diff:", err)
		os.Exit(1)
	}
}

func run(dsnEnv, collection string, n int, seed, ks, efs, iteratives, filters string, m, efBuild int, maintenanceMem string, keep bool, out string) error {
	if collection == "" {
		return errors.New("-collection is required")
	}
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		return fmt.Errorf("%s is not set", dsnEnv)
	}
	kList, err := ints(ks)
	if err != nil {
		return err
	}
	efList, err := ints(efs)
	if err != nil {
		return err
	}
	iterList := split(iteratives)
	for _, it := range iterList {
		switch it {
		case "off", "relaxed_order", "strict_order":
		default:
			return fmt.Errorf("unknown iterative_scan %q", it)
		}
	}
	filterList := split(filters)
	for _, f := range filterList {
		switch f {
		case "none", "department", "department+classification":
		default:
			return fmt.Errorf("unknown filter mode %q", f)
		}
	}
	maxK := 0
	for _, k := range kList {
		if k > maxK {
			maxK = k
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	rep := report{Version: "taskgate-kb-hnsw-diff-v1", GeneratedAt: time.Now().UTC().Format(time.RFC3339), Collection: collection, Queries: n, Seed: seed, IndexM: m, IndexEFBuild: efBuild}
	if err := conn.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname='vector'").Scan(&rep.PGVector); err != nil {
		return fmt.Errorf("pgvector version: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*), coalesce(max(vector_dims(embedding)),0) FROM kb.chunks WHERE collection=$1", collection).Scan(&rep.Chunks, &rep.Dimension); err != nil {
		return err
	}
	if rep.Chunks == 0 {
		return fmt.Errorf("collection %q has no chunks", collection)
	}
	// Deterministic sample: order by a keyed hash of the chunk's content hash.
	rows, err := conn.Query(ctx, `SELECT doc_id, version, chunk_index, department, classification, embedding::text
		FROM kb.chunks WHERE collection=$1 AND embedding IS NOT NULL
		ORDER BY md5($2 || ':' || text_sha256) LIMIT $3`, collection, seed, n)
	if err != nil {
		return err
	}
	var qs []query
	for rows.Next() {
		var q query
		if err := rows.Scan(&q.DocID, &q.Version, &q.ChunkIndex, &q.Department, &q.Classification, &q.Embedding); err != nil {
			return err
		}
		qs = append(qs, q)
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	if len(qs) < n {
		return fmt.Errorf("collection holds only %d embedded chunks, fewer than %d queries", len(qs), n)
	}

	// Exact arm first, once per filter mode, at maxK+1 so boundary ties show.
	exact := make(map[string][][]kbhnsw.Hit, len(filterList))
	exactLat := make(map[string][]float64, len(filterList))
	for _, f := range filterList {
		for _, q := range qs {
			t0 := time.Now()
			hits, err := search(ctx, conn, collection, q, f, maxK+1, "SET LOCAL enable_indexscan = off; SET LOCAL enable_bitmapscan = off; SET LOCAL enable_indexonlyscan = off")
			if err != nil {
				return fmt.Errorf("exact %s: %w", f, err)
			}
			exactLat[f] = append(exactLat[f], float64(time.Since(t0).Microseconds())/1000)
			exact[f] = append(exact[f], hits)
		}
	}

	// Build the index.
	if _, err := conn.Exec(ctx, "DROP INDEX IF EXISTS kb."+indexName); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "SET maintenance_work_mem = '"+strings.ReplaceAll(maintenanceMem, "'", "")+"'"); err != nil {
		return err
	}
	t0 := time.Now()
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE INDEX %s ON kb.chunks USING hnsw (embedding vector_cosine_ops) WITH (m = %d, ef_construction = %d)", indexName, m, efBuild)); err != nil {
		return fmt.Errorf("create hnsw index: %w", err)
	}
	rep.IndexBuildSecs = time.Since(t0).Seconds()
	if !keep {
		defer conn.Exec(context.Background(), "DROP INDEX IF EXISTS kb."+indexName)
	}
	if err := conn.QueryRow(ctx, "SELECT pg_relation_size('kb."+indexName+"')").Scan(&rep.IndexBytes); err != nil {
		return err
	}

	for _, f := range filterList {
		for _, it := range iterList {
			for _, ef := range efList {
				for _, k := range kList {
					if ef < k {
						// pgvector caps results at ef_search; the arm would be short by construction.
						continue
					}
					var outcomes []kbhnsw.Outcome
					var lat []float64
					settings := fmt.Sprintf("SET LOCAL enable_seqscan = off; SET LOCAL hnsw.ef_search = %d; SET LOCAL hnsw.iterative_scan = %s", ef, it)
					for i, q := range qs {
						t0 := time.Now()
						hits, err := search(ctx, conn, collection, q, f, k, settings)
						if err != nil {
							return fmt.Errorf("hnsw %s ef=%d %s k=%d: %w", f, ef, it, k, err)
						}
						lat = append(lat, float64(time.Since(t0).Microseconds())/1000)
						o, err := kbhnsw.Compare(exact[f][i], hits, k)
						if err != nil {
							return fmt.Errorf("compare %s ef=%d k=%d query %d: %w", f, ef, k, i, err)
						}
						outcomes = append(outcomes, o)
					}
					rep.Results = append(rep.Results, configResult{K: k, EFSearch: ef, IterativeScan: it, Filter: f, Summary: kbhnsw.Summarize(outcomes), ExactMS: stats(exactLat[f]), ApproxMS: stats(lat)})
				}
			}
		}
	}

	fmt.Printf("collection=%s chunks=%d dim=%d pgvector=%s queries=%d index_build_s=%.1f index_bytes=%d\n", rep.Collection, rep.Chunks, rep.Dimension, rep.PGVector, rep.Queries, rep.IndexBuildSecs, rep.IndexBytes)
	fmt.Printf("%-28s %-14s %4s %4s  %9s %9s %9s %6s %5s  %8s %8s\n", "filter", "iterative", "ef", "k", "identity", "recall", "jaccard", "short", "ties", "exact_ms", "hnsw_ms")
	for _, r := range rep.Results {
		fmt.Printf("%-28s %-14s %4d %4d  %9.4f %9.4f %9.4f %6d %5d  %8.2f %8.2f\n", r.Filter, r.IterativeScan, r.EFSearch, r.K, r.Summary.IdentityRate, r.Summary.MeanRecall, r.Summary.MeanJaccard, r.Summary.Short, r.Summary.TiesExcluded, r.ExactMS.P50, r.ApproxMS.P50)
	}
	if out != "" {
		data, err := json.MarshalIndent(rep, "", " ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// search runs one query inside a transaction so SET LOCAL planner settings
// apply to it alone. The query chunk itself is excluded from the result.
func search(ctx context.Context, conn *pgx.Conn, collection string, q query, filter string, limit int, settings string) ([]kbhnsw.Hit, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, settings); err != nil {
		return nil, err
	}
	where := "collection = $1 AND NOT (doc_id = $3 AND version = $4 AND chunk_index = $5)"
	args := []any{collection, q.Embedding, q.DocID, q.Version, q.ChunkIndex, limit}
	switch filter {
	case "department":
		where += " AND department = $7"
		args = append(args, q.Department)
	case "department+classification":
		where += " AND department = $7 AND classification = $8"
		args = append(args, q.Department, q.Classification)
	}
	sql := "SELECT doc_id, version, chunk_index, embedding <=> $2::vector AS distance FROM kb.chunks WHERE " + where + " ORDER BY embedding <=> $2::vector LIMIT $6"
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var hits []kbhnsw.Hit
	for rows.Next() {
		var h kbhnsw.Hit
		if err := rows.Scan(&h.DocID, &h.Version, &h.ChunkIndex, &h.Distance); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	return hits, tx.Commit(ctx)
}

func stats(v []float64) latency {
	if len(v) == 0 {
		return latency{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return latency{Mean: sum / float64(len(s)), P50: s[len(s)/2], P95: s[(len(s)*95)/100]}
}

func ints(csv string) ([]int, error) {
	var out []int
	for _, part := range split(csv) {
		v, err := strconv.Atoi(part)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("bad integer list %q", csv)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty integer list %q", csv)
	}
	return out, nil
}

func split(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
