package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const protocolVersion = "2025-06-18"

var supportedProtocolVersions = map[string]struct{}{
	"2025-06-18": {},
}

type Principal struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Role    string `json:"role"`
}

type TokenIdentity struct {
	Token     string
	Principal Principal
}

type Authenticator interface {
	Authenticate(context.Context, string) (Principal, bool)
}

// DigestIdentity authenticates a principal by the SHA-256 of its bearer token
// (lowercase hex), so a deployment can configure it without holding the
// token: the gateway's principals file carries digests only.
type DigestIdentity struct {
	TokenSHA256 string
	Principal   Principal
}

type StaticAuthenticator struct {
	identities []TokenIdentity
	digests    []DigestIdentity
}

func NewStaticAuthenticator(identities []TokenIdentity) *StaticAuthenticator {
	return &StaticAuthenticator{identities: append([]TokenIdentity(nil), identities...)}
}

// NewStaticAuthenticatorWithDigests also admits digest identities. A presented
// token is hashed once and compared in constant time against every digest.
func NewStaticAuthenticatorWithDigests(identities []TokenIdentity, digests []DigestIdentity) *StaticAuthenticator {
	return &StaticAuthenticator{identities: append([]TokenIdentity(nil), identities...), digests: append([]DigestIdentity(nil), digests...)}
}

func (a *StaticAuthenticator) Authenticate(_ context.Context, token string) (Principal, bool) {
	for _, identity := range a.identities {
		if len(token) == len(identity.Token) && subtle.ConstantTimeCompare([]byte(token), []byte(identity.Token)) == 1 {
			return identity.Principal, true
		}
	}
	if len(a.digests) != 0 {
		sum := sha256.Sum256([]byte(token))
		presented := []byte(hex.EncodeToString(sum[:]))
		for _, identity := range a.digests {
			if len(presented) == len(identity.TokenSHA256) && subtle.ConstantTimeCompare(presented, []byte(identity.TokenSHA256)) == 1 {
				return identity.Principal, true
			}
		}
	}
	return Principal{}, false
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type ToolResult struct {
	Structured any
	Text       string
	// Resources are embedded resources appended to the tool result's content
	// after its text item, for a tool that delivers a file (MCP
	// EmbeddedResource with a text body).
	Resources []EmbeddedResource
}

// EmbeddedResource is one resource of a tool result: Text for a text file,
// Blob (sent base64-encoded) for a binary one such as a PDF.
type EmbeddedResource struct {
	URI      string
	MimeType string
	Text     string
	Blob     []byte
}

type ToolError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"-"`
	// Cause is the unclassified underlying error a generic mapping wrapped.
	// It is logged next to the trace_id and never serialized to the caller.
	Cause error `json:"-"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// Unwrap exposes the wrapped cause to errors.Is/As and the server log.
func (e *ToolError) Unwrap() error { return e.Cause }

type ToolHandler interface {
	ListTools(Principal) []Tool
	CallTool(context.Context, Principal, string, json.RawMessage) (ToolResult, error)
}

type Server struct {
	authenticator Authenticator
	handler       ToolHandler
	logger        *slog.Logger
}

func NewServer(authenticator Authenticator, handler ToolHandler, logger *slog.Logger) (*Server, error) {
	if authenticator == nil || handler == nil {
		return nil, errors.New("MCP authenticator and tool handler are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{authenticator: authenticator, handler: handler, logger: logger}, nil
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !validOrigin(r.Header.Get("Origin")) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="taskbound-gateway"`)
		writeHTTPJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	if requestedVersion := r.Header.Get("MCP-Protocol-Version"); requestedVersion != "" && !supportsProtocol(requestedVersion) {
		http.Error(w, "unsupported MCP protocol version", http.StatusBadRequest)
		return
	}
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var rpcRequest request
	if err := decoder.Decode(&rpcRequest); err != nil {
		s.writeRPC(w, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "Parse error"}})
		return
	}
	if err := ensureEOF(decoder); err != nil || rpcRequest.JSONRPC != "2.0" || rpcRequest.Method == "" || !validRequestID(rpcRequest.ID) {
		s.writeRPC(w, response{JSONRPC: "2.0", ID: rpcRequest.ID, Error: &rpcError{Code: -32600, Message: "Invalid Request"}})
		return
	}
	if len(rpcRequest.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.dispatch(r.Context(), principal, rpcRequest)
	s.writeRPC(w, response{JSONRPC: "2.0", ID: rpcRequest.ID, Result: result, Error: rpcErr})
}

func (s *Server) authenticate(r *http.Request) (Principal, bool) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return Principal{}, false
	}
	return s.authenticator.Authenticate(r.Context(), token)
}

func (s *Server) dispatch(ctx context.Context, principal Principal, rpcRequest request) (any, *rpcError) {
	switch rpcRequest.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      struct {
				Name    string `json:"name"`
				Title   string `json:"title,omitempty"`
				Version string `json:"version"`
			} `json:"clientInfo"`
			Meta json.RawMessage `json:"_meta,omitempty"`
		}
		if err := decodeParams(rpcRequest.Params, &params); err != nil || params.ProtocolVersion == "" || params.Capabilities == nil || params.ClientInfo.Name == "" || params.ClientInfo.Version == "" {
			return nil, &rpcError{Code: -32602, Message: "Invalid params"}
		}
		negotiated := protocolVersion
		if supportsProtocol(params.ProtocolVersion) {
			negotiated = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "taskbound-agent-data-gateway", "version": "2.1.0"},
			"instructions":    "Create an approved task before querying. Task grants can only narrow and all queries are audited.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.handler.ListTools(principal)}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta,omitempty"`
		}
		if err := decodeParams(rpcRequest.Params, &params); err != nil || params.Name == "" {
			return nil, &rpcError{Code: -32602, Message: "Invalid params"}
		}
		if len(params.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		}
		traceID := traceIDFromContext(ctx)
		toolResult, err := s.handler.CallTool(context.WithValue(ctx, traceContextKey{}, traceID), principal, params.Name, params.Arguments)
		if err != nil {
			code, message := "INTERNAL_ERROR", "请求处理失败；请使用 trace_id 联系管理员"
			var toolErr *ToolError
			if errors.As(err, &toolErr) {
				code, message = toolErr.Code, toolErr.Message
			}
			logArguments := []any{"trace_id", traceID, "tool", params.Name, "subject", principal.Subject, "code", code, "error", err}
			if toolErr != nil && toolErr.Cause != nil {
				logArguments = append(logArguments, "cause", toolErr.Cause)
			}
			s.logger.Warn("MCP tool failed", logArguments...)
			errorBody := map[string]any{"code": code, "message": message}
			if toolErr != nil {
				for key, value := range toolErr.Details {
					if key == "code" || key == "message" || key == "trace_id" {
						continue
					}
					errorBody[key] = value
				}
			}
			structured := map[string]any{"trace_id": traceID, "error": errorBody}
			encoded, _ := json.Marshal(structured)
			return map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": string(encoded)}},
				"structuredContent": structured,
				"isError":           true,
			}, nil
		}
		structured := ensureTrace(toolResult.Structured, traceID)
		text := toolResult.Text
		if text == "" {
			encoded, _ := json.Marshal(structured)
			text = string(encoded)
		}
		content := []any{map[string]any{"type": "text", "text": text}}
		for _, resource := range toolResult.Resources {
			body := map[string]any{"uri": resource.URI, "mimeType": resource.MimeType}
			if resource.Blob != nil {
				body["blob"] = base64.StdEncoding.EncodeToString(resource.Blob)
			} else {
				body["text"] = resource.Text
			}
			content = append(content, map[string]any{"type": "resource", "resource": body})
		}
		return map[string]any{
			"content":           content,
			"structuredContent": structured,
			"isError":           false,
		}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "Method not found"}
	}
}

func validOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func supportsProtocol(version string) bool {
	_, ok := supportedProtocolVersions[version]
	return ok
}

func validRequestID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	decoder := json.NewDecoder(strings.NewReader(string(id)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := typed.Int64()
		return err == nil
	default:
		return false
	}
}

func (s *Server) writeRPC(w http.ResponseWriter, value response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureEOF(decoder)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON data")
	}
	return err
}

type traceContextKey struct{}

func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceContextKey{}).(string)
	return value
}

func traceIDFromContext(context.Context) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "trace_unavailable"
	}
	return "trace_" + hex.EncodeToString(buffer)
}

func ensureTrace(value any, traceID string) any {
	if object, ok := value.(map[string]any); ok {
		copy := make(map[string]any, len(object)+1)
		for key, item := range object {
			copy[key] = item
		}
		copy["trace_id"] = traceID
		return copy
	}
	return map[string]any{"trace_id": traceID, "data": value}
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
