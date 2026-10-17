package kbformal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The combined summaries must equal the registered pilot reports' summaries
// when recomputed from those reports' own per-deployment details: the port
// of each pilot runner's summary is then exact on the data the paper uses.

func pilotReport(t *testing.T, relative string, into any) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "kb-pilot", relative))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if into != nil {
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatal(err)
		}
	}
	return fields
}

func normalized(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func requireSame(t *testing.T, name string, computed any, registered json.RawMessage) {
	t.Helper()
	var want any
	if err := json.Unmarshal(registered, &want); err != nil {
		t.Fatal(err)
	}
	if got := normalized(t, computed); !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("%s differs from the pilot report:\n got  %.600s\n want %.600s", name, gotJSON, registered)
	}
}

func TestCombinedSummariesEqualThePilotReports(t *testing.T) {
	var ekb1 struct {
		Details []EKB1Deployment `json:"deployments_detail"`
	}
	fields := pilotReport(t, "ekb1/report-v2r3.json", &ekb1)
	summary, legitimate := SummarizeEKB1(ekb1.Details)
	requireSame(t, "E-KB1 summary", summary, fields["summary"])
	requireSame(t, "E-KB1 legitimate", legitimate, fields["legitimate"])

	var ekb2 struct {
		Details []EKB2Deployment `json:"deployments_detail"`
	}
	fields = pilotReport(t, "ekb2/report-v2r2.json", &ekb2)
	requireSame(t, "E-KB2(a) summary", SummarizeEKB2a(ekb2.Details), fields["summary"])

	var ekb4 struct {
		Details []EKB4Deployment `json:"deployments_detail"`
		Summary map[string]any   `json:"summary"`
	}
	fields = pilotReport(t, "ekb4/report-v2r2.json", &ekb4)
	want := ekb4.Summary
	delete(want, "seconds") // wall time of the pilot run, not a function of the details
	requireSame(t, "E-KB4 summary", SummarizeEKB4(ekb4.Details, 600), mustJSON(t, want))

	for _, tier := range []string{"48k", "96k"} {
		var ekb3 struct {
			Samples []EKB3Sample `json:"samples"`
		}
		fields = pilotReport(t, "ekb3/report-"+tier+"-r2.json", &ekb3)
		requireSame(t, "E-KB3 "+tier+" summary", SummarizeEKB3(ekb3.Samples), fields["summary"])
	}
}

func TestCombinedSummaryDetectsAChangedDetail(t *testing.T) {
	var ekb1 struct {
		Details []EKB1Deployment `json:"deployments_detail"`
	}
	fields := pilotReport(t, "ekb1/report-v2r3.json", &ekb1)
	ekb1.Details[0].Trajectories[0].LeakFraction += 0.5
	summary, _ := SummarizeEKB1(ekb1.Details)
	var want any
	if err := json.Unmarshal(fields["summary"], &want); err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(normalized(t, summary), want) {
		t.Fatal("a changed trajectory left the summary equal to the pilot's")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestEligibilityBlock(t *testing.T) {
	text := "knowledge_products:\n  - name: policy_kb\n    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n    extraction_rules: []\n"
	want := "    eligibility:\n      scope_envelope:\n        kb_department:\n          from_attribute: department\n"
	if got := EligibilityBlock(text); got != want {
		t.Fatalf("got %q", got)
	}
}
