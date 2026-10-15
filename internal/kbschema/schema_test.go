package kbschema

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// scratchDatabase creates a database on the pgvector test server and returns a
// simple-protocol DSN into it. KB_TEST_POSTGRES_DSN must point at a server
// whose image ships pgvector (pgvector/pgvector:pg16); the control-store test
// server does not, so this test keeps its own variable.
func scratchDatabase(t *testing.T) *sql.DB {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("KB_TEST_POSTGRES_DSN"))
	if base == "" {
		t.Skip("KB_TEST_POSTGRES_DSN (a pgvector-enabled PostgreSQL) is required for the Knowledge Product schema test")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse KB_TEST_POSTGRES_DSN: %v", err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("kb_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	parsed.Path = "/" + name
	query := parsed.Query()
	query.Set("default_query_exec_mode", "simple_protocol")
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = admin.ExecContext(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close()
	})
	return db
}

func applySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	// 10-reader.sh creates gateway_reader before 30-kb-schema.sh runs; the
	// scratch database only needs the role to exist for the GRANTs.
	if _, err := db.ExecContext(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_reader') THEN CREATE ROLE gateway_reader NOLOGIN; END IF; END $$`); err != nil {
		t.Fatalf("ensure gateway_reader: %v", err)
	}
	schema, err := os.ReadFile(filepath.Join("..", "..", "db", "init", "kb-schema.psql"))
	if err != nil {
		t.Fatalf("read schema file: %v", err)
	}
	if _, err := db.ExecContext(ctx, string(schema)); err != nil {
		t.Fatalf("apply kb-schema.psql: %v", err)
	}
}

func TestKnowledgeProductSchemaInstallsAndIsReadOnlyForGateway(t *testing.T) {
	db := scratchDatabase(t)
	applySchema(t, db)
	ctx := context.Background()

	var extension string
	if err := db.QueryRowContext(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&extension); err != nil {
		t.Fatalf("vector extension not installed: %v", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'kb' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	if got := strings.Join(tables, ","); got != "chunks,collections,documents,extracted_values,labels" {
		t.Fatalf("kb tables = %s", got)
	}

	var embeddingType string
	if err := db.QueryRowContext(ctx, `SELECT udt_name FROM information_schema.columns WHERE table_schema = 'kb' AND table_name = 'chunks' AND column_name = 'embedding'`).Scan(&embeddingType); err != nil || embeddingType != "vector" {
		t.Fatalf("chunks.embedding type = %q (%v), want vector", embeddingType, err)
	}
	var chunkKey string
	if err := db.QueryRowContext(ctx, `SELECT string_agg(a.attname, ',' ORDER BY k.ordinality)
		FROM pg_constraint c JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ordinality) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'kb.chunks'::regclass AND c.contype = 'p'`).Scan(&chunkKey); err != nil || chunkKey != "collection,doc_id,version,chunk_index" {
		t.Fatalf("chunks primary key = %q (%v)", chunkKey, err)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE contype = 'f' AND connamespace = 'kb'::regnamespace`).Scan(&foreignKeys); err != nil || foreignKeys != 4 {
		t.Fatalf("kb foreign keys = %d (%v), want 4 (labels, documents, chunks, extracted_values)", foreignKeys, err)
	}
	var indexes int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'kb' AND indexname LIKE 'kb_%_idx'`).Scan(&indexes); err != nil || indexes != 3 {
		t.Fatalf("kb filter indexes = %d (%v), want 3", indexes, err)
	}

	for _, table := range tables {
		var canSelect, canInsert, canUpdate, canDelete bool
		if err := db.QueryRowContext(ctx, `SELECT has_table_privilege('gateway_reader', $1, 'SELECT'), has_table_privilege('gateway_reader', $1, 'INSERT'), has_table_privilege('gateway_reader', $1, 'UPDATE'), has_table_privilege('gateway_reader', $1, 'DELETE')`, "kb."+table).Scan(&canSelect, &canInsert, &canUpdate, &canDelete); err != nil {
			t.Fatal(err)
		}
		if !canSelect || canInsert || canUpdate || canDelete {
			t.Fatalf("gateway_reader privileges on kb.%s: select=%v insert=%v update=%v delete=%v", table, canSelect, canInsert, canUpdate, canDelete)
		}
	}
	var schemaUsage bool
	if err := db.QueryRowContext(ctx, `SELECT has_schema_privilege('gateway_reader', 'kb', 'USAGE')`).Scan(&schemaUsage); err != nil || !schemaUsage {
		t.Fatalf("gateway_reader lacks USAGE on schema kb (%v)", err)
	}
	var publicUsage bool
	if err := db.QueryRowContext(ctx, `SELECT has_schema_privilege('public', 'kb', 'USAGE')`).Scan(&publicUsage); err != nil || publicUsage {
		t.Fatalf("PUBLIC must not have USAGE on schema kb (%v)", err)
	}
}

func TestKnowledgeProductSchemaRejectsMalformedRows(t *testing.T) {
	db := scratchDatabase(t)
	applySchema(t, db)
	ctx := context.Background()
	mustExec := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	mustReject := func(query, want string) {
		t.Helper()
		_, err := db.ExecContext(ctx, query)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want it to mention %q", query, err, want)
		}
	}
	sha := strings.Repeat("ab", 32)
	mustExec(`INSERT INTO kb.collections (collection, description) VALUES ('policy_kb', 'test')`)
	mustExec(`INSERT INTO kb.labels (collection, label) VALUES ('policy_kb', 'leadership-only')`)
	mustExec(`INSERT INTO kb.documents (collection, doc_id, version, title, document_type, department, classification, job_role, labels, valid_from, valid_until, content_sha256, chunk_count, body)
		VALUES ('policy_kb', 'D-0001', 1, 'Pricing policy', 'policy', 'sales', 'internal', 'staff', '{leadership-only}', DATE '2026-01-01', NULL, '` + sha + `', 1, 'body')`)
	mustExec(`INSERT INTO kb.chunks (collection, doc_id, version, chunk_index, text, text_sha256, embedding, department, classification, job_role)
		VALUES ('policy_kb', 'D-0001', 1, 0, 'body', '` + sha + `', '[0.1, 0.2, 0.3]', 'sales', 'internal', 'staff')`)
	mustExec(`INSERT INTO kb.extracted_values (collection, doc_id, version, document_type, field, value_type, canonical_value, value_numeric, department, classification)
		VALUES ('policy_kb', 'D-0001', 1, 'contract', 'amount', 'numeric', 'n:1250', 1250.00, 'sales', 'internal')`)
	mustReject(`INSERT INTO kb.extracted_values (collection, doc_id, version, document_type, field, value_type, canonical_value, value_text, department, classification)
		VALUES ('policy_kb', 'D-0001', 1, 'contract', 'amount3', 'numeric', 'n:1', 'text in numeric slot', 'sales', 'internal')`, "check constraint")

	mustReject(`INSERT INTO kb.chunks (collection, doc_id, version, chunk_index, text, text_sha256, department, classification, job_role)
		VALUES ('policy_kb', 'D-9999', 1, 0, 'orphan', '`+sha+`', 'sales', 'internal', 'staff')`, "foreign key")
	mustReject(`INSERT INTO kb.documents (collection, doc_id, version, title, document_type, department, classification, job_role, valid_from, valid_until, content_sha256, chunk_count, body)
		VALUES ('policy_kb', 'D-0002', 1, 'x', 'policy', 'sales', 'internal', 'staff', DATE '2026-01-01', DATE '2025-12-31', '`+sha+`', 1, 'b')`, "check constraint")
	mustReject(`INSERT INTO kb.documents (collection, doc_id, version, title, document_type, department, classification, job_role, valid_from, content_sha256, chunk_count, body)
		VALUES ('policy_kb', 'D-0003', 1, 'x', 'policy', 'sales', 'internal', 'staff', DATE '2026-01-01', 'not-a-digest', 1, 'b')`, "check constraint")
	mustReject(`INSERT INTO kb.extracted_values (collection, doc_id, version, document_type, field, value_type, canonical_value, value_numeric, department, classification)
		VALUES ('policy_kb', 'D-0001', 1, 'contract', 'amount2', 'float', '1', 1, 'sales', 'internal')`, "check constraint")
	mustReject(`INSERT INTO kb.chunks (collection, doc_id, version, chunk_index, text, text_sha256, department, classification, job_role)
		VALUES ('policy_kb', 'D-0001', 1, 0, 'dup', '`+sha+`', 'sales', 'internal', 'staff')`, "duplicate key")

	var distance float64
	if err := db.QueryRowContext(ctx, `SELECT embedding <=> '[0.1, 0.2, 0.3]' FROM kb.chunks WHERE doc_id = 'D-0001'`).Scan(&distance); err != nil || distance > 1e-9 {
		t.Fatalf("cosine distance to itself = %v (%v), want 0", distance, err)
	}
}
