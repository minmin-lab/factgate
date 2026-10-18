package apierr

import (
	"errors"
	"fmt"
)

const (
	CodeInvalidRequest           = "INVALID_REQUEST"
	CodeUnauthenticated          = "UNAUTHENTICATED"
	CodeForbidden                = "FORBIDDEN"
	CodeNotFound                 = "NOT_FOUND"
	CodeConflict                 = "CONFLICT"
	CodeTaskNotActive            = "TASK_NOT_ACTIVE"
	CodePolicyDenied             = "POLICY_DENIED"
	CodeBudgetExhausted          = "BUDGET_EXHAUSTED"
	CodeExposureBudgetExhausted  = "EXPOSURE_BUDGET_EXHAUSTED"
	CodeExposureEvidenceRequired = "EXPOSURE_EVIDENCE_REQUIRED"
	CodeApprovalUnavailable      = "APPROVAL_UNAVAILABLE"
	CodeDataSourceUnavailable    = "DATA_SOURCE_UNAVAILABLE"
	CodeInternal                 = "INTERNAL_ERROR"
	CodeSQLSyntaxError           = "SQL_SYNTAX_ERROR"
	CodeProductNotApproved       = "PRODUCT_NOT_APPROVED"
	CodeColumnNotApproved        = "COLUMN_NOT_APPROVED"
	CodeSQLNotLowerable          = "SQL_NOT_LOWERABLE"
	CodeJoinTypeUnsupported      = "JOIN_TYPE_UNSUPPORTED"
	CodeJoinGraphDisconnected    = "JOIN_GRAPH_DISCONNECTED"
	CodeJoinKeyTypeMismatch      = "JOIN_KEY_TYPE_MISMATCH"
	CodeCollationMismatch        = "COLLATION_MISMATCH"
	CodeSubqueryUnsupported      = "SUBQUERY_UNSUPPORTED"
	CodeViewSemanticChanged      = "VIEW_SEMANTIC_CHANGED"
	CodeViewQueryUnsupported     = "VIEW_QUERY_UNSUPPORTED"
	// CodeNotEligible rejects a task request whose subject or scope falls
	// outside the Catalog eligibility envelope; it never reaches an approver.
	CodeNotEligible = "NOT_ELIGIBLE"
	// Retrieval (knowledge product) rejections. Every one is closed-form: the
	// request is outside the admitted retrieval language and nothing executed.
	CodeRetrievalFilterUnsupported = "RETRIEVAL_FILTER_UNSUPPORTED"
	CodeRetrievalQueryUnsupported  = "RETRIEVAL_QUERY_UNSUPPORTED"
	CodeRetrievalKExceeded         = "RETRIEVAL_K_EXCEEDED"
	CodeRetrievalScopeExceeded     = "RETRIEVAL_SCOPE_EXCEEDED"
	CodeChunkNotReturned           = "CHUNK_NOT_RETURNED"
	CodeAggregateUnsupported       = "AGGREGATE_UNSUPPORTED"
	// Document download (docs/kb-design.md §12). Searching and downloading are
	// separate task kinds; a download names a document of the approved list;
	// a document is delivered whole or not at all.
	CodeTaskKindMismatch        = "TASK_KIND_MISMATCH"
	CodeDownloadNotApproved     = "DOWNLOAD_NOT_APPROVED"
	CodeDocumentNotDownloadable = "DOCUMENT_NOT_DOWNLOADABLE"
)

// Error contains a stable machine-readable code and a client-safe explanation.
// Cause is intentionally omitted from JSON responses.
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) error { return &Error{Code: code, Message: message} }

func Wrap(code, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func Public(err error) (code, message string) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Code, appErr.Message
	}
	return CodeInternal, "请求处理失败；请使用 trace_id 联系管理员"
}
