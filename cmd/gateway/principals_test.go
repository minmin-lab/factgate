package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
)

func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writePrincipals(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "principals.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPrincipalsFile(t *testing.T) {
	reserved := []control.Principal{
		{ID: "principal_alice", Subject: "alice", Role: "query", TokenHash: digestOf("alice-token")},
		{ID: "principal_carol", Subject: "carol", Role: "auditor", TokenHash: digestOf("carol-token")},
	}
	entry := func(id, subject, role, hash string) string {
		return fmt.Sprintf(`{"id":%q,"subject":%q,"role":%q,"token_sha256":%q}`, id, subject, role, hash)
	}
	file := func(entries ...string) string {
		return `{"version":"taskgate-principals-v1","principals":[` + strings.Join(entries, ",") + `]}`
	}
	good := file(entry("principal_kb_s01", "kb_s01", "query", digestOf("t1")), entry("principal_kb_s02", "kb_s02", "query", digestOf("t2")))
	principals, err := loadPrincipalsFile(writePrincipals(t, good), reserved)
	if err != nil || len(principals) != 2 || principals[1].Subject != "kb_s02" || principals[0].TokenHash != digestOf("t1") || principals[0].Role != "query" {
		t.Fatalf("valid file: %v %+v", err, principals)
	}
	cases := map[string]struct{ body, want string }{
		"unknown key":      {`{"version":"taskgate-principals-v1","principals":[{"id":"principal_x","subject":"x","role":"query","token_sha256":"` + digestOf("x") + `","token":"x"}]}`, "unknown field"},
		"trailing data":    {good + `{}`, "trailing data"},
		"wrong version":    {strings.Replace(good, "taskgate-principals-v1", "taskgate-principals-v0", 1), "version"},
		"empty":            {file(), "no principals"},
		"auditor role":     {file(entry("principal_kb_a", "kb_a", "auditor", digestOf("a"))), "not query"},
		"bad id":           {file(entry("alice2", "kb_b", "query", digestOf("b"))), "principal_"},
		"bad subject":      {file(entry("principal_kb_c", "KB C", "query", digestOf("c"))), "lowercase identifier"},
		"bad digest":       {file(entry("principal_kb_d", "kb_d", "query", strings.ToUpper(digestOf("d")))), "lowercase SHA-256"},
		"reserved id":      {file(entry("principal_alice", "kb_e", "query", digestOf("e"))), "duplicate or reserved"},
		"reserved subject": {file(entry("principal_kb_f", "carol", "query", digestOf("f"))), "duplicate or reserved"},
		"reserved token":   {file(entry("principal_kb_g", "kb_g", "query", digestOf("alice-token"))), "duplicate or reserved"},
		"duplicate id":     {file(entry("principal_kb_h", "kb_h", "query", digestOf("h")), entry("principal_kb_h", "kb_h2", "query", digestOf("h2"))), "duplicate or reserved"},
		"duplicate token":  {file(entry("principal_kb_i", "kb_i", "query", digestOf("i")), entry("principal_kb_j", "kb_j", "query", digestOf("i"))), "duplicate or reserved"},
	}
	for name, c := range cases {
		if _, err := loadPrincipalsFile(writePrincipals(t, c.body), reserved); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
	if _, err := loadPrincipalsFile(filepath.Join(t.TempDir(), "missing.json"), reserved); err == nil {
		t.Error("missing file accepted")
	}
}
