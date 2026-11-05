package catalog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// delegated_authorization.pending_ttl (KB1-LOOP-137): a positive Go duration
// bounding how long a task may wait for submission or approval; absent, pending
// tasks never expire and the block serializes as before. The Catalog digest
// binds it like the rest of the block.
func TestPendingTTLParsesAndRefusesNonPositiveOrMalformedValues(t *testing.T) {
	block := "\ndelegated_authorization:\n  issuer_role: {attribute: job_family, any_of: [dept_admin]}\n  child_self_confirm: true\n"
	absent, err := Parse([]byte(validCatalogYAML(t) + block))
	if err != nil {
		t.Fatalf("parse without pending_ttl: %v", err)
	}
	if absent.DelegatedAuthorization.PendingTTL != "" || absent.DelegatedAuthorization.PendingTTLDuration() != 0 {
		t.Fatalf("an absent pending_ttl is not off: %+v", absent.DelegatedAuthorization)
	}
	if encoded, _ := json.Marshal(absent.DelegatedAuthorization); strings.Contains(string(encoded), "pending_ttl") {
		t.Fatalf("an absent pending_ttl is serialized: %s", encoded)
	}
	set, err := Parse([]byte(validCatalogYAML(t) + block + "  pending_ttl: 72h\n"))
	if err != nil {
		t.Fatalf("parse pending_ttl 72h: %v", err)
	}
	if got := set.DelegatedAuthorization.PendingTTLDuration(); got != 72*time.Hour || set.DelegatedAuthorization.PendingTTL != "72h" {
		t.Fatalf("pending_ttl 72h parsed as %v (%q)", got, set.DelegatedAuthorization.PendingTTL)
	}
	if len(set.SHA256) != 64 || set.SHA256 == absent.SHA256 {
		t.Fatalf("the Catalog digest does not bind pending_ttl: %s vs %s", set.SHA256, absent.SHA256)
	}
	for _, bad := range []string{"0s", "abc", "-1h", "0"} {
		_, err := Parse([]byte(validCatalogYAML(t) + block + "  pending_ttl: \"" + bad + "\"\n"))
		var problems ValidationErrors
		if err == nil || !errors.As(err, &problems) || !strings.Contains(err.Error(), "delegated_authorization.pending_ttl") {
			t.Fatalf("pending_ttl %q: error %v, want a ValidationErrors naming delegated_authorization.pending_ttl", bad, err)
		}
	}
}
