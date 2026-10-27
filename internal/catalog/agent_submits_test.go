package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// delegated_authorization.agent_submits (KB1-LOOP-149): the switch parses,
// defaults to off, and is bound by the Catalog digest like the rest of the
// block.
func TestAgentSubmitsParsesDefaultsOffAndChangesTheDigest(t *testing.T) {
	block := "\ndelegated_authorization:\n  issuer_role: {attribute: job_family, any_of: [dept_admin]}\n  child_self_confirm: true\n"
	off, err := Parse([]byte(validCatalogYAML(t) + block))
	if err != nil {
		t.Fatalf("parse without the switch: %v", err)
	}
	on, err := Parse([]byte(validCatalogYAML(t) + block + "  agent_submits: true\n"))
	if err != nil {
		t.Fatalf("parse with the switch: %v", err)
	}
	if off.DelegatedAuthorization == nil || off.DelegatedAuthorization.AgentSubmits {
		t.Fatalf("the switch is not off by default: %+v", off.DelegatedAuthorization)
	}
	if on.DelegatedAuthorization == nil || !on.DelegatedAuthorization.AgentSubmits {
		t.Fatalf("agent_submits: true did not parse: %+v", on.DelegatedAuthorization)
	}
	if len(on.SHA256) != 64 || on.SHA256 == off.SHA256 {
		t.Fatalf("the Catalog digest does not bind the switch: %s vs %s", on.SHA256, off.SHA256)
	}
	explicitOff, err := Parse([]byte(validCatalogYAML(t) + block + "  agent_submits: false\n"))
	if err != nil || explicitOff.DelegatedAuthorization.AgentSubmits {
		t.Fatalf("agent_submits: false: %v %+v", err, explicitOff)
	}
	// Off, the JSON form of the block is what it was before the switch existed.
	encoded, _ := json.Marshal(off.DelegatedAuthorization)
	if strings.Contains(string(encoded), "agent_submits") {
		t.Fatalf("an off switch is serialized: %s", encoded)
	}
	encoded, _ = json.Marshal(on.DelegatedAuthorization)
	if !strings.Contains(string(encoded), `"agent_submits":true`) {
		t.Fatalf("an on switch is not serialized: %s", encoded)
	}
	// A misspelled key is still refused by the strict decoder.
	if _, err := Parse([]byte(validCatalogYAML(t) + block + "  agent_submit: true\n")); err == nil {
		t.Fatal("a misspelled agent_submits key was accepted")
	}
}
