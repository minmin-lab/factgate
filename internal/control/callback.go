package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

func callbackPayloadHash(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// LookupCallback reads an existing OA event without claiming or modifying it.
// A completed event with the same payload returns its durable response as a
// replay; in-flight and retryable events return their current status.
func (s *Store) LookupCallback(ctx context.Context, eventID string, rawPayload []byte) (CallbackClaim, error) {
	const op = "lookup callback"
	if err := s.checkOpen(op); err != nil {
		return CallbackClaim{}, err
	}
	if eventID == "" || len(rawPayload) == 0 {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("event ID and raw payload are required"))
	}
	var storedHash string
	var status CallbackStatus
	var response []byte
	err := s.db.QueryRowContext(ctx, `
SELECT payload_sha256, status, response_body
FROM callback_idempotency WHERE event_id=$1`, eventID).Scan(&storedHash, &status, &response)
	if err != nil {
		if isNoRows(err) {
			return CallbackClaim{}, opErr(op, ErrNotFound, err)
		}
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	if storedHash != callbackPayloadHash(rawPayload) {
		return CallbackClaim{}, opErr(op, ErrIdempotencyConflict, nil)
	}
	claim := CallbackClaim{EventID: eventID, Status: status}
	switch status {
	case CallbackCompleted:
		claim.Replay = true
		claim.Response = append([]byte(nil), response...)
	case CallbackProcessing, CallbackRetryable:
		// A lookup intentionally leaves the existing claim unchanged.
	default:
		return CallbackClaim{}, opErr(op, ErrConflict, fmt.Errorf("unknown callback status %q", status))
	}
	return claim, nil
}

// ClaimCallback claims an OA event ID. Completed events return their original
// response with Replay=true; the same ID with a different body fails closed.
func (s *Store) ClaimCallback(ctx context.Context, eventID string, rawPayload []byte) (CallbackClaim, error) {
	const op = "claim callback"
	if err := s.checkOpen(op); err != nil {
		return CallbackClaim{}, err
	}
	if eventID == "" || len(rawPayload) == 0 {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("event ID and raw payload are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	claim, err := claimCallbackTx(ctx, tx, eventID, callbackPayloadHash(rawPayload), s.now())
	if err != nil {
		return CallbackClaim{}, err
	}
	if claim.Claimed {
		_, err = appendAuditTx(ctx, tx, AuditEvent{
			Actor: "oa", EventType: "CALLBACK_CLAIMED", OccurredAt: s.now(),
			Payload: mustJSON(map[string]any{"event_id": eventID}),
		})
		if err != nil {
			return CallbackClaim{}, opErr(op, ErrConflict, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	return claim, nil
}

// RetryCallback reclaims an event previously marked retryable by
// FailCallback or startup recovery. It shares ClaimCallback's replay and
// payload-conflict semantics.
func (s *Store) RetryCallback(ctx context.Context, eventID string, rawPayload []byte) (CallbackClaim, error) {
	return s.ClaimCallback(ctx, eventID, rawPayload)
}

func claimCallbackTx(ctx context.Context, tx *sql.Tx, eventID, payloadHash string, now time.Time) (CallbackClaim, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO callback_idempotency(event_id, payload_sha256, status, claimed_at)
VALUES ($1, $2, 'PROCESSING', $3)
ON CONFLICT (event_id) DO NOTHING`, eventID, payloadHash, dbTime(now))
	if err != nil {
		return CallbackClaim{}, opErr("claim callback", ErrConflict, err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return CallbackClaim{EventID: eventID, Status: CallbackProcessing, Claimed: true}, nil
	}
	var storedHash string
	var status CallbackStatus
	var response []byte
	err = tx.QueryRowContext(ctx, `
SELECT payload_sha256, status, response_body
FROM callback_idempotency WHERE event_id=$1 FOR UPDATE`, eventID).
		Scan(&storedHash, &status, &response)
	if err != nil {
		return CallbackClaim{}, opErr("claim callback", ErrConflict, err)
	}
	if storedHash != payloadHash {
		return CallbackClaim{}, opErr("claim callback", ErrIdempotencyConflict, nil)
	}
	switch status {
	case CallbackCompleted:
		return CallbackClaim{EventID: eventID, Status: status, Replay: true, Response: append([]byte(nil), response...)}, nil
	case CallbackProcessing:
		return CallbackClaim{}, opErr("claim callback", ErrCallbackInProgress, nil)
	case CallbackRetryable:
		_, err = tx.ExecContext(ctx, `
UPDATE callback_idempotency
SET status='PROCESSING', last_error='', claimed_at=$1, completed_at=NULL
WHERE event_id=$2 AND status='RETRYABLE'`, dbTime(now), eventID)
		if err != nil {
			return CallbackClaim{}, opErr("claim callback", ErrConflict, err)
		}
		return CallbackClaim{EventID: eventID, Status: CallbackProcessing, Claimed: true}, nil
	default:
		return CallbackClaim{}, opErr("claim callback", ErrConflict, fmt.Errorf("unknown callback status %q", status))
	}
}

func (s *Store) CompleteCallback(ctx context.Context, eventID string, rawPayload, response []byte) error {
	const op = "complete callback"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if eventID == "" || len(rawPayload) == 0 {
		return opErr(op, ErrInvalid, fmt.Errorf("event ID and raw payload are required"))
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	var storedHash string
	var status CallbackStatus
	var storedResponse []byte
	err = tx.QueryRowContext(ctx, `SELECT payload_sha256, status, response_body FROM callback_idempotency WHERE event_id=$1 FOR UPDATE`, eventID).
		Scan(&storedHash, &status, &storedResponse)
	if err != nil {
		if isNoRows(err) {
			return opErr(op, ErrNotFound, err)
		}
		return opErr(op, ErrConflict, err)
	}
	if storedHash != callbackPayloadHash(rawPayload) {
		return opErr(op, ErrIdempotencyConflict, nil)
	}
	if status == CallbackCompleted {
		if !bytes.Equal(storedResponse, response) {
			return opErr(op, ErrConflict, fmt.Errorf("callback already completed with another response"))
		}
		return nil
	}
	if status != CallbackProcessing {
		return opErr(op, ErrConflict, fmt.Errorf("callback is %s", status))
	}
	if err := completeCallbackTx(ctx, tx, eventID, response, s.now()); err != nil {
		return opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		Actor: "oa", EventType: "CALLBACK_COMPLETED", OccurredAt: s.now(),
		Payload: mustJSON(map[string]any{"event_id": eventID}),
	})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func completeCallbackTx(ctx context.Context, tx *sql.Tx, eventID string, response []byte, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
UPDATE callback_idempotency
SET status='COMPLETED', response_body=$1, last_error='', completed_at=$2
WHERE event_id=$3 AND status='PROCESSING'`, response, dbTime(now), eventID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("callback is not processing")
	}
	return nil
}

func (s *Store) FailCallback(ctx context.Context, eventID string, rawPayload []byte, failure string) error {
	const op = "fail callback"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if eventID == "" || len(rawPayload) == 0 || failure == "" {
		return opErr(op, ErrInvalid, fmt.Errorf("event ID, raw payload, and failure are required"))
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE callback_idempotency
SET status='RETRYABLE', last_error=$1, completed_at=NULL
WHERE event_id=$2 AND payload_sha256=$3 AND status='PROCESSING'`, failure, eventID, callbackPayloadHash(rawPayload))
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return opErr(op, ErrConflict, fmt.Errorf("callback is not processing or payload differs"))
	}
	return nil
}

// ApprovalCallback applies an OA decision and its idempotency marker in one
// PostgreSQL transaction. Grant is required when NewState is ACTIVE.
type ApprovalCallback struct {
	EventID       string
	RawPayload    []byte
	Event         ApprovalEvent
	ExpectedState TaskState
	NewState      TaskState
	Reason        TerminalReason
	Grant         *TaskGrant
	Response      []byte
	// Step records one approval of an approval chain without changing the
	// task's state (KB1-LOOP-141): ExpectedState and NewState are both
	// AWAITING_APPROVAL, no grant or reason is carried, and the event is
	// claimed, recorded and audited like any other callback.
	Step bool
	// ApprovedSteps, when set, is the number of approved or narrowed
	// approval events the caller derived the chain position from. A different
	// count under the task lock means another approval landed first, and the
	// callback is refused rather than applied out of order.
	ApprovedSteps *int
}

func (s *Store) ApplyApprovalCallback(ctx context.Context, callback ApprovalCallback) (claimResult CallbackClaim, resultErr error) {
	const op = "apply approval callback"
	var trace *callbackPhaseTrace
	if callback.ExpectedState == TaskAwaitingSubmission && callback.NewState == TaskAwaitingApproval &&
		callback.Reason == "" && callback.Grant == nil {
		trace = s.newCallbackPhaseTrace(callback.Event.TaskID, callback.EventID)
	}
	defer func() { trace.finish(resultErr, claimResult.Replay) }()
	ctx = withCallbackPhaseTrace(ctx, trace)
	if err := s.checkOpen(op); err != nil {
		return CallbackClaim{}, err
	}
	if callback.EventID == "" || len(callback.RawPayload) == 0 || callback.Event.TaskID == "" || callback.Event.Actor == "" || callback.Event.Decision == "" {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("callback and approval fields are required"))
	}
	if callback.Event.EventID == "" {
		callback.Event.EventID = callback.EventID
	}
	if callback.NewState == TaskActive && callback.Grant == nil {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("active task requires a grant"))
	}
	if callback.Step && (callback.ExpectedState != TaskAwaitingApproval || callback.NewState != TaskAwaitingApproval ||
		callback.Grant != nil || callback.Reason != "") {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("an approval step keeps the task awaiting approval and carries no grant"))
	}
	if callback.Grant != nil && callback.Grant.TaskID != callback.Event.TaskID {
		return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("grant belongs to another task"))
	}
	payload, err := normalizeJSON(callback.Event.Payload, `{}`)
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrInvalid, err)
	}
	now := s.now()
	if callback.Event.CreatedAt.IsZero() {
		callback.Event.CreatedAt = now
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	finishClaim := trace.begin(callbackPhaseClaim)
	claim, err := claimCallbackTx(ctx, tx, callback.EventID, callbackPayloadHash(callback.RawPayload), now)
	finishClaim(err)
	if err != nil {
		return CallbackClaim{}, err
	}
	if claim.Replay {
		finishCommit := trace.begin(callbackPhaseCommit)
		err = tx.Commit()
		finishCommit(err)
		if err != nil {
			return CallbackClaim{}, opErr(op, ErrConflict, err)
		}
		return claim, nil
	}
	var current TaskState
	finishTaskLock := trace.begin(callbackPhaseTaskRowLock)
	err = tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=$1 FOR UPDATE`, callback.Event.TaskID).Scan(&current)
	finishTaskLock(err)
	if err != nil {
		if isNoRows(err) {
			return CallbackClaim{}, opErr(op, ErrNotFound, err)
		}
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	if callback.ExpectedState != "" && current != callback.ExpectedState {
		return CallbackClaim{}, opErr(op, ErrInvalidStateChange, fmt.Errorf("expected %s, found %s", callback.ExpectedState, current))
	}
	if !callback.Step && !allowedTransition(current, callback.NewState, callback.Reason) {
		return CallbackClaim{}, opErr(op, ErrInvalidStateChange, fmt.Errorf("%s -> %s", current, callback.NewState))
	}
	if callback.ApprovedSteps != nil {
		var approved int
		if err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM approval_events WHERE task_id=$1 AND decision IN ('approved', 'narrowed')`, callback.Event.TaskID).Scan(&approved); err != nil {
			return CallbackClaim{}, opErr(op, ErrConflict, err)
		}
		if approved != *callback.ApprovedSteps {
			return CallbackClaim{}, opErr(op, ErrConflict, fmt.Errorf("approval chain moved: %d approvals recorded, %d expected", approved, *callback.ApprovedSteps))
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO approval_events(event_id, task_id, actor, decision, payload_json, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, callback.Event.EventID, callback.Event.TaskID, callback.Event.Actor,
		callback.Event.Decision, string(payload), dbTime(callback.Event.CreatedAt))
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	var expires any
	if callback.Grant != nil {
		grant := *callback.Grant
		if grant.CreatedAt.IsZero() {
			grant.CreatedAt = now
		}
		if grant.Budget.Queries <= 0 || grant.Budget.Rows <= 0 || grant.Budget.DBMS <= 0 || grant.ExpiresAt.IsZero() {
			return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("invalid grant budget or expiry"))
		}
		if err := validateExposureGrant(grant.Exposure); err != nil {
			return CallbackClaim{}, opErr(op, ErrInvalid, err)
		}
		if grant.CatalogVersion == "" || !validSHA256Hex(grant.CatalogDigest) ||
			grant.DatasourceID == "" || !validSHA256Hex(grant.SchemaDigest) || grant.ApprovalReceipt == "" {
			return CallbackClaim{}, opErr(op, ErrInvalid, fmt.Errorf("invalid grant provenance"))
		}
		if err := validateGrantViewBinding(grant); err != nil {
			return CallbackClaim{}, opErr(op, ErrInvalid, err)
		}
		products, err := json.Marshal(grant.ApprovedProducts)
		if err != nil {
			return CallbackClaim{}, opErr(op, ErrInvalid, err)
		}
		columns, err := json.Marshal(grant.ApprovedColumns)
		if err != nil {
			return CallbackClaim{}, opErr(op, ErrInvalid, err)
		}
		scope, err := normalizeJSON(grant.MandatoryScope, `{}`)
		if err != nil {
			return CallbackClaim{}, opErr(op, ErrInvalid, err)
		}
		if err := insertGrantAndBudget(ctx, tx, grant, products, columns, scope); err != nil {
			return CallbackClaim{}, opErr(op, ErrConflict, err)
		}
		expires = dbTime(grant.ExpiresAt)
	}
	if callback.Step {
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET updated_at=$1 WHERE id=$2`, dbTime(now), callback.Event.TaskID)
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE tasks SET state=$1, terminal_reason=$2, updated_at=$3, expires_at=COALESCE($4, expires_at) WHERE id=$5`,
			callback.NewState, callback.Reason, dbTime(now), expires, callback.Event.TaskID)
	}
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	eventType := "APPROVAL_CALLBACK_APPLIED"
	if callback.Step {
		eventType = "APPROVAL_STEP_APPLIED"
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: callback.Event.TaskID, Actor: callback.Event.Actor, EventType: eventType, OccurredAt: now,
		Payload: approvalCallbackAuditPayload(callback, current),
	})
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	if err := completeCallbackTx(ctx, tx, callback.EventID, callback.Response, now); err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	finishCommit := trace.begin(callbackPhaseCommit)
	err = tx.Commit()
	finishCommit(err)
	if err != nil {
		return CallbackClaim{}, opErr(op, ErrConflict, err)
	}
	claim.Status = CallbackCompleted
	claim.Response = append([]byte(nil), callback.Response...)
	return claim, nil
}

func approvalCallbackAuditPayload(callback ApprovalCallback, from TaskState) json.RawMessage {
	payload := map[string]any{
		"event_id": callback.EventID, "decision": callback.Event.Decision,
		"from": from, "to": callback.NewState, "terminal_reason": callback.Reason,
	}
	if callback.Grant != nil {
		payload["catalog_version"] = callback.Grant.CatalogVersion
		payload["catalog_digest"] = callback.Grant.CatalogDigest
		payload["datasource_id"] = callback.Grant.DatasourceID
		payload["schema_digest"] = callback.Grant.SchemaDigest
		payload["view_binding_digest"] = callback.Grant.ViewBindingDigest
		payload["exposure"] = callback.Grant.Exposure
	}
	return mustJSON(payload)
}
