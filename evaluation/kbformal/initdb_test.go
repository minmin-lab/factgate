package kbformal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// block returns the text from the first occurrence of start through the end
// of the first occurrence of end after it.
func block(t *testing.T, source, name, start, end string) string {
	t.Helper()
	i := strings.Index(source, start)
	if i < 0 {
		t.Fatalf("%s: start marker %q not found", name, start)
	}
	j := strings.Index(source[i:], end)
	if j < 0 {
		t.Fatalf("%s: end marker %q not found after start", name, end)
	}
	return source[i : i+j+len(end)]
}

// TestKnowledgeInitCopiesV5DefinitionsVerbatim keeps db/init-kb tied to the V5
// init scripts it copies: every block below is extracted from the V5 file and
// must appear unchanged in db/init-kb/00-kb-base.sh, so a change on either side
// fails here instead of drifting silently.
func TestKnowledgeInitCopiesV5DefinitionsVerbatim(t *testing.T) {
	kb := readRepoFile(t, "db", "init-kb", "00-kb-base.sh")
	schema := readRepoFile(t, "db", "init", "00-schema.sql")
	sidecars := readRepoFile(t, "db", "init", "05-ordinal-sidecars.sql")
	freeze := readRepoFile(t, "db", "init", "07-freeze-publications.sql")
	reader := readRepoFile(t, "db", "init", "10-reader.sh")
	blocks := map[string]string{
		"00-schema reporting":     block(t, schema, "00-schema", "CREATE SCHEMA reporting;", "CREATE SCHEMA reporting;"),
		"00-schema attestation":   block(t, schema, "00-schema", "CREATE TABLE reporting.datasource_attestation (", "VALUES (TRUE, 'taskgate-demo-travel');"),
		"05 sidecar schema":       block(t, sidecars, "05", "CREATE SCHEMA taskgate_ordinal;", "REVOKE ALL ON SCHEMA taskgate_ordinal FROM PUBLIC;"),
		"07 snapshot owner":       block(t, freeze, "07", "CREATE ROLE taskgate_snapshot_owner", "NOBYPASSRLS;"),
		"07 rejection function":   block(t, freeze, "07", "CREATE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()", "$taskgate_reject_mutation$;"),
		"07 rejection revoke":     block(t, freeze, "07", "REVOKE ALL ON FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()", "FROM PUBLIC;"),
		"10 reader role":          block(t, reader, "10-reader", "CREATE ROLE gateway_reader", "ALTER ROLE gateway_reader SET search_path = pg_catalog;"),
		"10 reader connect grant": block(t, reader, "10-reader", "GRANT CONNECT ON DATABASE :DBNAME TO gateway_reader;", "GRANT USAGE ON SCHEMA reporting TO gateway_reader;"),
	}
	for name, text := range blocks {
		if len(text) < 20 {
			t.Fatalf("%s: extracted block %q is implausibly short", name, text)
		}
		if !strings.Contains(kb, text) {
			t.Errorf("db/init-kb/00-kb-base.sh no longer contains the V5 block %s:\n%s", name, text)
		}
	}
	// Positive control: a block that is not copied must be reported missing,
	// so a passing run is not an artifact of an always-true check.
	v5Only := block(t, reader, "10-reader", "GRANT USAGE ON SCHEMA final_v5_compiler TO gateway_reader;", ";")
	if strings.Contains(kb, v5Only) {
		t.Fatal("V5-only grant unexpectedly present; the knowledge init must not grant V5 relations")
	}
}
