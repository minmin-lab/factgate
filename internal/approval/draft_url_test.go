package approval

import "testing"

func TestDraftPageURLJoinsBaseAndDraft(t *testing.T) {
	for base, want := range map[string]string{
		"https://oa.example:8443":  "https://oa.example:8443/tasks/draft_1",
		"https://oa.example:8443/": "https://oa.example:8443/tasks/draft_1",
	} {
		if got := DraftPageURL(base, "draft_1"); got != want {
			t.Fatalf("DraftPageURL(%q) = %q, want %q", base, got, want)
		}
	}
}
