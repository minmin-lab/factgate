package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixedAuthenticator struct{ principal Principal }

func (a fixedAuthenticator) Authenticate(context.Context, string) (Principal, bool) {
	return a.principal, true
}

// A principal without an agent label encodes exactly as before the field
// existed (KB1-LOOP-151); one with a label adds agent_label.
func TestPrincipalAgentLabelIsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	plain, err := json.Marshal(Principal{ID: "alice", Subject: "alice", Role: "query"})
	if err != nil || string(plain) != `{"id":"alice","subject":"alice","role":"query"}` {
		t.Fatalf("plain principal %s %v", plain, err)
	}
	labelled, err := json.Marshal(Principal{ID: "alice", Subject: "alice", Role: "query", AgentLabel: "lzj"})
	if err != nil || string(labelled) != `{"id":"alice","subject":"alice","role":"query","agent_label":"lzj"}` {
		t.Fatalf("labelled principal %s %v", labelled, err)
	}
}

// A failed tool call logs the agent label only when the principal has one, so
// the formal round's log lines are unchanged.
func TestToolFailureLogCarriesAgentLabelOnlyWhenSet(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		label string
		want  bool
	}{{"", false}, {"lzj", true}} {
		var logs bytes.Buffer
		server, err := NewServer(fixedAuthenticator{Principal{ID: "alice", Subject: "alice", Role: "query", AgentLabel: test.label}},
			testTools{}, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"missing","arguments":{}}}`))
		request.Header.Set("Authorization", "Bearer any-token")
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(logs.String(), "MCP tool failed") {
			t.Fatalf("label %q: status %d, logs %q", test.label, recorder.Code, logs.String())
		}
		if got := strings.Contains(logs.String(), "agent_label=lzj"); got != test.want {
			t.Fatalf("label %q: agent_label logged = %v, want %v: %q", test.label, got, test.want, logs.String())
		}
		if !test.want && strings.Contains(logs.String(), "agent_label") {
			t.Fatalf("an empty label was logged: %q", logs.String())
		}
	}
}
