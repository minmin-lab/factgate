package principalattr

import (
	"strings"
	"testing"
	"time"
)

const validSnapshot = `version: taskgate-principal-attributes-v1
subjects:
  - subject: alice
    attributes:
      department: [finance]
      job_family: [analyst]
  - subject: hr.lead
    attributes:
      department: [hr]
      job_family: [hr]
    grants:
      - attribute: department
        value: sales
        valid_from: "2026-10-01T00:00:00Z"
        valid_until: "2026-10-31T00:00:00Z"
`

func mustParse(t *testing.T, data string) *Snapshot {
	t.Helper()
	snapshot, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	return snapshot
}

func TestParseValidSnapshot(t *testing.T) {
	snapshot := mustParse(t, validSnapshot)
	if snapshot.Version != Version || len(snapshot.Subjects) != 2 || len(snapshot.SHA256) != 64 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
	if !snapshot.Known("alice") || !snapshot.Known("hr.lead") || snapshot.Known("mallory") {
		t.Fatal("Known reports wrong membership")
	}
	var nilSnapshot *Snapshot
	if nilSnapshot.Known("alice") || nilSnapshot.Values("alice", "department", time.Now()) != nil {
		t.Fatal("nil snapshot must report nothing")
	}
}

func TestDigestIsStableUnderReordering(t *testing.T) {
	reordered := `version: taskgate-principal-attributes-v1
subjects:
  - subject: hr.lead
    grants:
      - attribute: department
        value: sales
        valid_from: "2026-10-01T00:00:00Z"
        valid_until: "2026-10-31T00:00:00Z"
    attributes:
      job_family: [hr]
      department: [hr]
  - subject: alice
    attributes:
      job_family: [analyst]
      department: [finance]
`
	a, b := mustParse(t, validSnapshot), mustParse(t, reordered)
	if a.SHA256 != b.SHA256 {
		t.Fatalf("digest changed under reordering: %s vs %s", a.SHA256, b.SHA256)
	}
	valueOrder := strings.Replace(validSnapshot, "job_family: [analyst]", "job_family: [analyst, lead]", 1)
	valueOrderSwapped := strings.Replace(validSnapshot, "job_family: [analyst]", "job_family: [lead, analyst]", 1)
	if mustParse(t, valueOrder).SHA256 != mustParse(t, valueOrderSwapped).SHA256 {
		t.Fatal("digest depends on attribute value order")
	}
	if mustParse(t, valueOrder).SHA256 == a.SHA256 {
		t.Fatal("digest did not change when a value was added")
	}
	grantChanged := strings.Replace(validSnapshot, "2026-10-31T00:00:00Z", "2026-11-30T00:00:00Z", 1)
	if mustParse(t, grantChanged).SHA256 == a.SHA256 {
		t.Fatal("digest did not change when a grant window changed")
	}
}

func TestValuesHonourGrantWindow(t *testing.T) {
	snapshot := mustParse(t, validSnapshot)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"before window", from.Add(-time.Second), "hr"},
		{"window start inclusive", from, "hr,sales"},
		{"inside window", from.Add(72 * time.Hour), "hr,sales"},
		{"window end exclusive", until, "hr"},
		{"after window", until.Add(time.Hour), "hr"},
		{"non-UTC instant inside window", from.Add(time.Hour).In(time.FixedZone("x", 8*3600)), "hr,sales"},
	}
	for _, tc := range cases {
		got := strings.Join(snapshot.Values("hr.lead", "department", tc.now), ",")
		if got != tc.want {
			t.Errorf("%s: Values = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := snapshot.Values("alice", "department", from); strings.Join(got, ",") != "finance" {
		t.Fatalf("alice department = %v", got)
	}
	if snapshot.Values("alice", "clearance", from) != nil || snapshot.Values("mallory", "department", from) != nil {
		t.Fatal("unknown attribute or subject must yield nil")
	}
}

func TestParseRejectsInvalidSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		message string
	}{
		{"wrong version", strings.Replace(validSnapshot, "taskgate-principal-attributes-v1", "v0", 1), "version must be"},
		{"no subjects", "version: taskgate-principal-attributes-v1\nsubjects: []\n", "at least one subject"},
		{"duplicate subject", strings.Replace(validSnapshot, "subject: hr.lead", "subject: alice", 1), "is duplicated"},
		{"bad subject identifier", strings.Replace(validSnapshot, "subject: alice", "subject: \"Alice Smith\"", 1), "canonical subject identifier"},
		{"uppercase attribute", strings.Replace(validSnapshot, "job_family: [analyst]", "JobFamily: [analyst]", 1), "not a lowercase identifier"},
		{"empty attribute values", strings.Replace(validSnapshot, "job_family: [analyst]", "job_family: []", 1), "at least one value"},
		{"duplicate attribute values", strings.Replace(validSnapshot, "job_family: [analyst]", "job_family: [analyst, analyst]", 1), "must be distinct"},
		{"padded value", strings.Replace(validSnapshot, "job_family: [analyst]", "job_family: [\" analyst\"]", 1), "canonical non-empty tokens"},
		{"grant bad attribute", strings.Replace(validSnapshot, "      - attribute: department\n        value: sales", "      - attribute: Dept\n        value: sales", 1), "grants[0].attribute"},
		{"grant bad timestamp", strings.Replace(validSnapshot, "2026-10-01T00:00:00Z", "2026-10-01", 1), "valid_from must be RFC 3339"},
		{"grant empty window", strings.Replace(validSnapshot, "2026-10-31T00:00:00Z", "2026-10-01T00:00:00Z", 1), "empty validity window"},
		{"grant inverted window", strings.Replace(validSnapshot, "2026-10-31T00:00:00Z", "2026-09-01T00:00:00Z", 1), "empty validity window"},
		{"unknown field", strings.Replace(validSnapshot, "subjects:", "issuer: hr\nsubjects:", 1), "issuer"},
		{"unknown grant field", strings.Replace(validSnapshot, "        valid_until:", "        reason: cover\n        valid_until:", 1), "reason"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data))
			if err == nil {
				t.Fatal("Parse accepted an invalid snapshot")
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.message)
			}
		})
	}
}

func TestLoadReadsFile(t *testing.T) {
	path := t.TempDir() + "/attrs.yaml"
	if err := writeFile(path, validSnapshot); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if snapshot.SHA256 != mustParse(t, validSnapshot).SHA256 {
		t.Fatal("Load digest differs from Parse digest")
	}
	if _, err := Load(path + ".missing"); err == nil || !strings.Contains(err.Error(), "read principal attributes") {
		t.Fatalf("missing file error = %v", err)
	}
}
