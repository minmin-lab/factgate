package kbformal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"testing"
)

// The gateway loader's patterns (cmd/gateway/principals.go).
var (
	gatewayPrincipalID      = regexp.MustCompile(`^principal_[a-z0-9_]{1,60}$`)
	gatewayPrincipalSubject = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

func TestFilePrincipalsMatchTheGatewayLayout(t *testing.T) {
	// Positive and negative controls for the copied patterns.
	if !gatewayPrincipalID.MatchString("principal_finance_director") || gatewayPrincipalID.MatchString("principal_Finance") ||
		!gatewayPrincipalSubject.MatchString("finance-director") || gatewayPrincipalSubject.MatchString("Finance-director") {
		t.Fatal("pattern controls failed")
	}
	principals, tokens, err := FilePrincipals(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(principals))
	decoder.DisallowUnknownFields()
	var file struct {
		Version    string `json:"version"`
		Principals []struct {
			ID          string `json:"id"`
			Subject     string `json:"subject"`
			Role        string `json:"role"`
			TokenSHA256 string `json:"token_sha256"`
		} `json:"principals"`
	}
	if err := decoder.Decode(&file); err != nil {
		t.Fatal(err)
	}
	var tokenMap map[string]string
	if err := json.Unmarshal(tokens, &tokenMap); err != nil {
		t.Fatal(err)
	}
	subjects := EligibilitySubjects()
	if file.Version != "taskgate-principals-v1" || len(file.Principals) != len(subjects) || len(subjects) != 60 || len(tokenMap) != 60 {
		t.Fatalf("version %q, %d principals, %d tokens", file.Version, len(file.Principals), len(tokenMap))
	}
	seen := map[string]bool{}
	for index, principal := range file.Principals {
		token := tokenMap[principal.Subject]
		digest := sha256.Sum256([]byte(token))
		switch {
		case principal.Subject != subjects[index].Name:
			t.Fatalf("principal %d is %q, want %q", index, principal.Subject, subjects[index].Name)
		case !gatewayPrincipalID.MatchString(principal.ID) || !gatewayPrincipalSubject.MatchString(principal.Subject) || principal.Role != "query":
			t.Fatalf("principal %d does not match the gateway layout: %+v", index, principal)
		case len(token) != 48 || principal.TokenSHA256 != hex.EncodeToString(digest[:]):
			t.Fatalf("principal %d token digest does not match its token", index)
		case principal.Subject == "alice" || principal.Subject == "carol":
			t.Fatalf("principal %d reuses a reserved subject", index)
		case seen[principal.ID] || seen[principal.Subject] || seen[principal.TokenSHA256]:
			t.Fatalf("principal %d repeats an id, subject or digest", index)
		}
		seen[principal.ID], seen[principal.Subject], seen[principal.TokenSHA256] = true, true, true
	}
	again, _, err := FilePrincipals(rand.Reader)
	if err != nil || bytes.Equal(again, principals) {
		t.Fatal("two generations produced the same principals")
	}
}

func TestFilePrincipalsRefuseAShortRandomSource(t *testing.T) {
	if _, _, err := FilePrincipals(bytes.NewReader(make([]byte, 100))); err == nil {
		t.Fatal("a 100-byte random source must not yield 60 tokens")
	}
}
