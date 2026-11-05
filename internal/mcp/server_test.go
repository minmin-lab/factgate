package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testTools struct{}

func (testTools) ListTools(Principal) []Tool {
	return []Tool{{Name: "hello", Description: "test", InputSchema: map[string]any{"type": "object"}}}
}

func (testTools) CallTool(_ context.Context, principal Principal, name string, _ json.RawMessage) (ToolResult, error) {
	if name == "detailed_error" {
		return ToolResult{}, &ToolError{Code: "SQL_NOT_LOWERABLE", Message: "rewrite the query", Details: map[string]any{
			"reason": "LEFT_JOIN_UNSUPPORTED", "retryable_after_rewrite": true,
		}}
	}
	if name != "hello" {
		return ToolResult{}, &ToolError{Code: "NOT_FOUND", Message: "unknown tool"}
	}
	return ToolResult{Structured: map[string]any{"subject": principal.Subject}}, nil
}

func TestToolErrorDetailsAreReturnedAsStructuredFields(t *testing.T) {
	t.Parallel()
	auth := NewStaticAuthenticator([]TokenIdentity{{Token: "alice-token", Principal: Principal{ID: "alice", Subject: "alice", Role: "query"}}})
	server, err := NewServer(auth, testTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{
		"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"detailed_error","arguments":{}}
	}`))
	request.Header.Set("Authorization", "Bearer alice-token")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	var response struct {
		Result struct {
			StructuredContent struct {
				Error map[string]any `json:"error"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response.Result.StructuredContent.Error
	if got["reason"] != "LEFT_JOIN_UNSUPPORTED" || got["retryable_after_rewrite"] != true {
		t.Fatalf("structured error details = %#v", got)
	}
}

func TestAuthenticatedMCPInitializeAndCall(t *testing.T) {
	t.Parallel()
	auth := NewStaticAuthenticator([]TokenIdentity{{Token: "alice-token", Principal: Principal{ID: "alice", Subject: "alice", Role: "query"}}})
	server, err := NewServer(auth, testTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	unauthorizedResult := httptest.NewRecorder()
	server.ServeHTTP(unauthorizedResult, unauthorized)
	if unauthorizedResult.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorizedResult.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hello","arguments":{}}}`))
	request.Header.Set("Authorization", "Bearer alice-token")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("status = %d", result.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	encoded := result.Body.String()
	if !strings.Contains(encoded, `"subject":"alice"`) || !strings.Contains(encoded, `"trace_id"`) {
		t.Fatalf("response = %s", encoded)
	}
}

func TestTransportValidationAndMetadata(t *testing.T) {
	t.Parallel()
	auth := NewStaticAuthenticator([]TokenIdentity{{Token: "alice-token", Principal: Principal{ID: "alice", Subject: "alice", Role: "query"}}})
	server, err := NewServer(auth, testTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	originRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	originRequest.Header.Set("Origin", "https://attacker.example")
	originResult := httptest.NewRecorder()
	server.ServeHTTP(originResult, originRequest)
	if originResult.Code != http.StatusForbidden {
		t.Fatalf("untrusted origin status = %d", originResult.Code)
	}

	versionRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	versionRequest.Header.Set("Authorization", "Bearer alice-token")
	versionRequest.Header.Set("MCP-Protocol-Version", "2099-01-01")
	versionResult := httptest.NewRecorder()
	server.ServeHTTP(versionResult, versionRequest)
	if versionResult.Code != http.StatusBadRequest {
		t.Fatalf("unknown protocol status = %d", versionResult.Code)
	}

	metaRequest := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"hello","arguments":{},"_meta":{"progressToken":"p1"}}}`))
	metaRequest.Header.Set("Authorization", "Bearer alice-token")
	metaResult := httptest.NewRecorder()
	server.ServeHTTP(metaResult, metaRequest)
	if metaResult.Code != http.StatusOK || !strings.Contains(metaResult.Body.String(), `"isError":false`) {
		t.Fatalf("metadata call = %d %s", metaResult.Code, metaResult.Body.String())
	}

	notification := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list","params":{}}`))
	notification.Header.Set("Authorization", "Bearer alice-token")
	notificationResult := httptest.NewRecorder()
	server.ServeHTTP(notificationResult, notification)
	if notificationResult.Code != http.StatusAccepted || notificationResult.Body.Len() != 0 {
		t.Fatalf("notification = %d %q", notificationResult.Code, notificationResult.Body.String())
	}

	nullID := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":null,"method":"ping"}`))
	nullID.Header.Set("Authorization", "Bearer alice-token")
	nullResult := httptest.NewRecorder()
	server.ServeHTTP(nullResult, nullID)
	if nullResult.Code != http.StatusOK || !strings.Contains(nullResult.Body.String(), `"code":-32600`) {
		t.Fatalf("null ID = %d %s", nullResult.Code, nullResult.Body.String())
	}
}

func TestInitializeValidatesAndNegotiatesProtocol(t *testing.T) {
	t.Parallel()
	auth := NewStaticAuthenticator([]TokenIdentity{{Token: "alice-token", Principal: Principal{ID: "alice", Subject: "alice", Role: "query"}}})
	server, err := NewServer(auth, testTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	request.Header.Set("Authorization", "Bearer alice-token")
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if !strings.Contains(result.Body.String(), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize response = %s", result.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`))
	invalid.Header.Set("Authorization", "Bearer alice-token")
	invalidResult := httptest.NewRecorder()
	server.ServeHTTP(invalidResult, invalid)
	if !strings.Contains(invalidResult.Body.String(), `"code":-32602`) {
		t.Fatalf("invalid initialize response = %s", invalidResult.Body.String())
	}
}

func TestStaticAuthenticatorAdmitsDigestIdentities(t *testing.T) {
	sum := sha256.Sum256([]byte("file-principal-token"))
	digest := hex.EncodeToString(sum[:])
	authenticator := NewStaticAuthenticatorWithDigests(
		[]TokenIdentity{{Token: "alice-token", Principal: Principal{ID: "principal_alice", Subject: "alice", Role: "query"}}},
		[]DigestIdentity{{TokenSHA256: digest, Principal: Principal{ID: "principal_kb_s01", Subject: "kb_s01", Role: "query"}}})
	if principal, ok := authenticator.Authenticate(context.Background(), "alice-token"); !ok || principal.Subject != "alice" {
		t.Fatalf("static identity: %+v %v", principal, ok)
	}
	if principal, ok := authenticator.Authenticate(context.Background(), "file-principal-token"); !ok || principal.Subject != "kb_s01" {
		t.Fatalf("digest identity: %+v %v", principal, ok)
	}
	for _, wrong := range []string{"", digest, "file-principal-token ", "alice-token2"} {
		if _, ok := authenticator.Authenticate(context.Background(), wrong); ok {
			t.Errorf("token %q authenticated", wrong)
		}
	}
	if _, ok := NewStaticAuthenticator(nil).Authenticate(context.Background(), "file-principal-token"); ok {
		t.Error("an authenticator without digests admitted a digest identity")
	}
}
