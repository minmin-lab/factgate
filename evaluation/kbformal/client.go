package kbformal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/parquet-go/parquet-go"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

// The clients below drive a deployed gateway and its approval simulator over
// their public HTTP interfaces, the way the V5 campaign adapter does
// (evaluation/cmd/final-v5-adapter client.go, oa.go, provision.go): JSON-RPC
// tools/call on /mcp with a bearer token, and the OA browser forms with CSRF.
// Nothing here writes to the Control store.

// ToolError is a structured gateway tool error.
type ToolError struct {
	Code    string
	Message string
	Reason  string
}

func (err *ToolError) Error() string { return err.Code + ": " + err.Message }

// MCPClient calls gateway tools as one principal.
type MCPClient struct {
	URL   string
	Token string
	HTTP  *http.Client
	next  atomic.Int64
}

// NewMCPClient returns a client for baseURL (the gateway root) and token.
func NewMCPClient(baseURL, token string, timeout time.Duration) *MCPClient {
	return &MCPClient{URL: strings.TrimRight(baseURL, "/") + "/mcp", Token: token, HTTP: &http.Client{Timeout: timeout}}
}

// Call invokes a tool and decodes its structured content into a map (numbers
// as json.Number). A tool error is returned as *ToolError.
func (client *MCPClient) Call(ctx context.Context, tool string, arguments any) (map[string]any, error) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": client.next.Add(1), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.URL, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MCP HTTP status %d", response.StatusCode)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return nil, err
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("MCP RPC error %d", rpc.Error.Code)
	}
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return nil, err
	}
	if result.IsError {
		var structured struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Reason  string `json:"reason"`
			} `json:"error"`
		}
		if json.Unmarshal(result.StructuredContent, &structured) == nil && structured.Error.Code != "" {
			return nil, &ToolError{Code: structured.Error.Code, Message: structured.Error.Message, Reason: structured.Error.Reason}
		}
		return nil, errors.New("tool returned an error without structured content")
	}
	if len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null" {
		return nil, errors.New("tool omitted structured content")
	}
	decoder := json.NewDecoder(strings.NewReader(string(result.StructuredContent)))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// OAAccount is one OA browser identity with a reused session.
type OAAccount struct {
	BaseURL  string
	Username string
	Password string
	Timeout  time.Duration
	client   *http.Client
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (account *OAAccount) login(ctx context.Context) error {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: account.Timeout}
	page, _, err := oaGet(ctx, client, strings.TrimRight(account.BaseURL, "/")+"/login")
	if err != nil {
		return err
	}
	match := csrfField.FindSubmatch(page)
	if len(match) != 2 {
		return errors.New("OA login page omitted CSRF token")
	}
	values := url.Values{"csrf": {string(match[1])}, "username": {account.Username}, "password": {account.Password}}
	if _, finalURL, err := oaPost(ctx, client, strings.TrimRight(account.BaseURL, "/")+"/login", values); err != nil {
		return err
	} else if landedOnLogin(finalURL) {
		return fmt.Errorf("OA login for %s failed", account.Username)
	}
	account.client = client
	return nil
}

// Act performs one draft action (submit, or decision with values), logging in
// on first use and once more if the session has expired. Landing on the login
// page is never read as success.
func (account *OAAccount) Act(ctx context.Context, draftID, action string, values url.Values) error {
	for attempt := 0; attempt < 2; attempt++ {
		if account.client == nil || attempt == 1 {
			if err := account.login(ctx); err != nil {
				return err
			}
		}
		taskURL := strings.TrimRight(account.BaseURL, "/") + "/tasks/" + url.PathEscape(draftID)
		page, finalURL, err := oaGet(ctx, account.client, taskURL)
		if err != nil {
			return err
		}
		if landedOnLogin(finalURL) {
			continue
		}
		match := csrfField.FindSubmatch(page)
		if len(match) != 2 {
			return errors.New("OA task page omitted CSRF token")
		}
		form := url.Values{}
		for key, value := range values {
			form[key] = append([]string(nil), value...)
		}
		form.Set("csrf", string(match[1]))
		if _, finalURL, err = oaPost(ctx, account.client, taskURL+"/"+action, form); err != nil {
			return err
		}
		if !landedOnLogin(finalURL) {
			return nil
		}
	}
	return fmt.Errorf("OA dropped the %s session twice during %s on draft %s", account.Username, action, draftID)
}

// NarrowDecision is the complete OA narrow form; only TTL is reduced by the
// formal round (E-KB2(b)).
type NarrowDecision struct {
	Products       []string
	Columns        map[string][]string
	MandatoryScope map[string]any
	MaxQueries     int64
	MaxResultRows  int64
	MaxDBMS        int64
	QueryTimeoutMS int64
	TaskTTLMS      int64
}

func (decision NarrowDecision) values() (url.Values, error) {
	products, err := json.Marshal(decision.Products)
	if err != nil {
		return nil, err
	}
	columns, err := json.Marshal(decision.Columns)
	if err != nil {
		return nil, err
	}
	scope, err := json.Marshal(decision.MandatoryScope)
	if err != nil {
		return nil, err
	}
	return url.Values{"decision": {"narrow"}, "approved_products": {string(products)}, "approved_columns": {string(columns)},
		"mandatory_scope": {string(scope)}, "max_queries": {strconv.FormatInt(decision.MaxQueries, 10)},
		"max_result_rows": {strconv.FormatInt(decision.MaxResultRows, 10)}, "max_db_ms": {strconv.FormatInt(decision.MaxDBMS, 10)},
		"per_query_timeout_ms": {strconv.FormatInt(decision.QueryTimeoutMS, 10)}, "task_ttl_ms": {strconv.FormatInt(decision.TaskTTLMS, 10)}}, nil
}

func landedOnLogin(finalURL string) bool {
	parsed, err := url.Parse(finalURL)
	return err == nil && (parsed.Path == "/login" || strings.HasSuffix(parsed.Path, "/login"))
}

func oaGet(ctx context.Context, client *http.Client, target string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.Request.URL.String(), fmt.Errorf("OA GET %s returned %d", target, response.StatusCode)
	}
	return body, response.Request.URL.String(), nil
}

func oaPost(ctx context.Context, client *http.Client, target string, values url.Values) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.Request.URL.String(), fmt.Errorf("OA POST %s returned %d", target, response.StatusCode)
	}
	return body, response.Request.URL.String(), nil
}

// TaskRequest is what a requester asks for; the route budget comes from the
// profile Catalog.
type TaskRequest struct {
	Objective string
	Products  []string
	Columns   map[string][]string
	Scope     map[string]any
	// TTL, when positive, is the narrowed task TTL the approver grants
	// (E-KB2(b)); zero approves the request as made.
	TTL time.Duration
}

// Provisioner runs the real request -> OA submit -> approve -> ACTIVE path.
type Provisioner struct {
	Requester *MCPClient
	Applicant *OAAccount
	Approver  *OAAccount
	Poll      time.Duration
	Deadline  time.Duration
}

// Provision returns the task ID once the task is ACTIVE, and the
// request_data_task response.
func (p *Provisioner) Provision(ctx context.Context, request TaskRequest) (string, map[string]any, error) {
	created, err := p.Requester.Call(ctx, "request_data_task", map[string]any{
		"objective": request.Objective, "data_products": request.Products, "columns": request.Columns, "scopes": request.Scope,
	})
	if err != nil {
		return "", nil, err
	}
	taskID, _ := created["task_id"].(string)
	oaURL, _ := created["oa_url"].(string)
	if taskID == "" || oaURL == "" {
		return "", created, errors.New("request_data_task omitted task_id or oa_url")
	}
	draftID := oaURL[strings.LastIndex(oaURL, "/")+1:]
	if err := p.Applicant.Act(ctx, draftID, "submit", url.Values{}); err != nil {
		return "", created, err
	}
	if err := p.wait(ctx, taskID, "AWAITING_APPROVAL"); err != nil {
		return "", created, err
	}
	if request.TTL > 0 {
		budget, _ := created["budget"].(map[string]any)
		number := func(key string) int64 {
			value, _ := budget[key].(json.Number)
			parsed, _ := value.Int64()
			return parsed
		}
		values, err := NarrowDecision{Products: request.Products, Columns: request.Columns, MandatoryScope: request.Scope,
			MaxQueries: number("max_queries"), MaxResultRows: number("max_rows"), MaxDBMS: number("max_db_ms"),
			QueryTimeoutMS: number("query_timeout_ms"), TaskTTLMS: request.TTL.Milliseconds()}.values()
		if err != nil {
			return "", created, err
		}
		if err := p.Approver.Act(ctx, draftID, "decision", values); err != nil {
			return "", created, err
		}
	} else if err := p.Approver.Act(ctx, draftID, "decision", url.Values{"decision": {"approved"}}); err != nil {
		return "", created, err
	}
	if err := p.wait(ctx, taskID, "ACTIVE"); err != nil {
		return "", created, err
	}
	return taskID, created, nil
}

func (p *Provisioner) wait(ctx context.Context, taskID, want string) error {
	deadline := time.Now().Add(p.Deadline)
	last := ""
	for time.Now().Before(deadline) {
		status, err := p.Requester.Call(ctx, "get_task_status", map[string]string{"task_id": taskID})
		if err == nil {
			last, _ = status["state"].(string)
			if last == want {
				return nil
			}
		}
		time.Sleep(p.Poll)
	}
	return fmt.Errorf("task %s did not reach %s (last state %q)", taskID, want, last)
}

// ReceiptVerifier checks query receipts against the gateway's published keyring.
type ReceiptVerifier interface {
	Verify(queryreceipt.QueryReceiptV1) error
}

// FetchReceiptVerifier reads /.well-known/taskgate/query-receipt-keyring.json.
func FetchReceiptVerifier(ctx context.Context, gatewayBase string, timeout time.Duration) (ReceiptVerifier, error) {
	client := &http.Client{Timeout: timeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gatewayBase, "/")+"/.well-known/taskgate/query-receipt-keyring.json", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("receipt keyring returned %d", response.StatusCode)
	}
	var bundle queryreceipt.PublicKeyBundleV1
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&bundle); err != nil {
		return nil, err
	}
	return bundle.Verifier()
}

// VerifyReceipt decodes a response's receipt and applies the V5 adapter's
// acceptance rule: current version, structural validity, signature, and the
// execution binding, ledger pre-state, artifact intent and exposure evidence.
func VerifyReceipt(verifier ReceiptVerifier, raw any) (queryreceipt.QueryReceiptV1, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return queryreceipt.QueryReceiptV1{}, err
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(encoded, &receipt); err != nil {
		return receipt, fmt.Errorf("decode receipt: %w", err)
	}
	if err := queryreceipt.RequireCurrentVersion(receipt.Version); err != nil {
		return receipt, err
	}
	if err := receipt.Validate(); err != nil {
		return receipt, fmt.Errorf("receipt does not validate: %w", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		return receipt, fmt.Errorf("verify receipt: %w", err)
	}
	if receipt.ExecutionBindingV2 == nil || receipt.ExposureLedgerBefore == nil || receipt.ArtifactIntent == nil || receipt.Exposure == nil {
		return receipt, errors.New("receipt lacks execution binding, pre-state, artifact intent or exposure evidence")
	}
	return receipt, nil
}

// DeliverRows fetches a completed result the way the V5 adapter does: the
// deliver_result tool names a short-lived Parquet download whose sha256 must be
// the receipt's artifact intent, and the downloaded bytes must hash to it
// before they are parsed with the TaskGate schema they carry.
func DeliverRows(ctx context.Context, client *MCPClient, receipt queryreceipt.QueryReceiptV1, timeout time.Duration) ([][]any, error) {
	intent := receipt.ArtifactIntent
	if intent == nil {
		return nil, errors.New("receipt carries no artifact intent")
	}
	delivery, err := client.Call(ctx, "deliver_result", map[string]any{"result_id": intent.ResultID, "format": "parquet"})
	if err != nil {
		return nil, fmt.Errorf("deliver_result: %w", err)
	}
	downloadURL, _ := delivery["download_url"].(string)
	artifactSHA, _ := delivery["artifact_sha256"].(string)
	if downloadURL == "" || artifactSHA != intent.ParquetSHA256 {
		return nil, errors.New("delivery metadata disagrees with the receipt's artifact intent")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<30))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != intent.ParquetSHA256 {
		return nil, errors.New("downloaded Parquet does not hash to the receipt's artifact intent")
	}
	reader := parquet.NewReader(bytes.NewReader(body))
	defer reader.Close()
	stored, ok := reader.File().Lookup("taskgate.schema")
	if !ok {
		return nil, errors.New("Parquet omitted the TaskGate schema")
	}
	var schema []resultartifact.ColumnSchema
	if err := json.Unmarshal([]byte(stored), &schema); err != nil {
		return nil, err
	}
	if reader.NumRows() != intent.RowCount {
		return nil, errors.New("Parquet row count disagrees with the receipt")
	}
	limit := intent.RowCount
	if limit < 1 {
		limit = 1
	}
	return resultartifact.ReadParquet(body, intent.ResultID, schema, 0, limit)
}
