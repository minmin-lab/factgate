package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *Store) CreatePrincipal(ctx context.Context, principal Principal) error {
	const op = "create principal"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if strings.TrimSpace(principal.ID) == "" || strings.TrimSpace(principal.Subject) == "" || strings.TrimSpace(principal.Role) == "" {
		return opErr(op, ErrInvalid, fmt.Errorf("id, subject, and role are required"))
	}
	if principal.CreatedAt.IsZero() {
		principal.CreatedAt = s.now()
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	_, err = tx.ExecContext(ctx, `
INSERT INTO principals(id, subject, role, token_hash, created_at, disabled_at)
VALUES ($1, $2, $3, $4, $5, $6)`, principal.ID, principal.Subject, principal.Role, principal.TokenHash,
		dbTime(principal.CreatedAt), nullableTime(principal.DisabledAt))
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		Actor:      principal.Subject,
		EventType:  "PRINCIPAL_CREATED",
		Payload:    mustJSON(map[string]any{"principal_id": principal.ID, "role": principal.Role}),
		OccurredAt: principal.CreatedAt,
	})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func (s *Store) GetPrincipal(ctx context.Context, id string) (Principal, error) {
	const op = "get principal"
	if err := s.checkOpen(op); err != nil {
		return Principal{}, err
	}
	return scanPrincipal(op, s.db.QueryRowContext(ctx, `
SELECT id, subject, role, token_hash, created_at, disabled_at FROM principals WHERE id = $1`, id))
}

func (s *Store) GetPrincipalBySubject(ctx context.Context, subject string) (Principal, error) {
	const op = "get principal by subject"
	if err := s.checkOpen(op); err != nil {
		return Principal{}, err
	}
	return scanPrincipal(op, s.db.QueryRowContext(ctx, `
SELECT id, subject, role, token_hash, created_at, disabled_at FROM principals WHERE subject = $1`, subject))
}

func (s *Store) DisablePrincipal(ctx context.Context, principalID string, disabledAt time.Time) (Principal, error) {
	const op = "disable principal"
	if err := s.checkOpen(op); err != nil {
		return Principal{}, err
	}
	if strings.TrimSpace(principalID) == "" {
		return Principal{}, opErr(op, ErrInvalid, fmt.Errorf("principal id is required"))
	}
	if disabledAt.IsZero() {
		disabledAt = s.now()
	}
	return scanPrincipal(op, s.db.QueryRowContext(ctx, `
UPDATE principals
SET disabled_at = COALESCE(disabled_at, $2)
WHERE id = $1
RETURNING id, subject, role, token_hash, created_at, disabled_at`, principalID, dbTime(disabledAt)))
}

func scanPrincipal(op string, row rowScanner) (Principal, error) {
	var principal Principal
	var created time.Time
	var disabled sql.NullTime
	if err := row.Scan(&principal.ID, &principal.Subject, &principal.Role, &principal.TokenHash, &created, &disabled); err != nil {
		if isNoRows(err) {
			return Principal{}, opErr(op, ErrNotFound, err)
		}
		return Principal{}, opErr(op, ErrConflict, err)
	}
	principal.CreatedAt = dbTime(created)
	principal.DisabledAt = scanNullableTime(disabled)
	return principal, nil
}

func (s *Store) CreateTask(ctx context.Context, task Task) error {
	const op = "create task"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.PrincipalID) == "" || strings.TrimSpace(task.Objective) == "" || strings.TrimSpace(task.CatalogVersion) == "" {
		return opErr(op, ErrInvalid, fmt.Errorf("id, principal, objective, and catalog version are required"))
	}
	if task.State == "" {
		task.State = TaskAwaitingSubmission
	}
	if !validTaskState(task.State) || (task.State != TaskArchived && task.TerminalReason != "") || (task.State == TaskArchived && task.TerminalReason == "") {
		return opErr(op, ErrInvalid, fmt.Errorf("invalid state/terminal reason combination"))
	}
	requestContext, err := normalizeJSON(task.RequestContext, `{}`)
	if err != nil {
		return opErr(op, ErrInvalid, fmt.Errorf("request context: %w", err))
	}
	now := s.now()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	rootTaskID := task.ID
	if task.ParentTaskID != "" {
		var parentCatalog string
		var parentState TaskState
		if err := tx.QueryRowContext(ctx, `SELECT root_task_id, catalog_version, state FROM tasks WHERE id=$1 FOR SHARE`, task.ParentTaskID).
			Scan(&rootTaskID, &parentCatalog, &parentState); err != nil {
			if isNoRows(err) {
				return opErr(op, ErrNotFound, fmt.Errorf("parent task: %w", err))
			}
			return opErr(op, ErrConflict, err)
		}
		var targetDisabled sql.NullTime
		if err := tx.QueryRowContext(ctx, `SELECT disabled_at FROM principals WHERE id=$1 FOR SHARE`, task.PrincipalID).
			Scan(&targetDisabled); err != nil {
			if isNoRows(err) {
				return opErr(op, ErrNotFound, fmt.Errorf("delegated principal: %w", err))
			}
			return opErr(op, ErrConflict, err)
		}
		if parentState != TaskActive || parentCatalog != task.CatalogVersion || targetDisabled.Valid {
			return opErr(op, ErrInvalid, fmt.Errorf("delegation requires an active parent, the same catalog, and an enabled principal"))
		}
	} else if task.RootTaskID != "" && task.RootTaskID != task.ID {
		return opErr(op, ErrInvalid, fmt.Errorf("a root task must identify itself as root"))
	}
	if task.RootTaskID != "" && task.RootTaskID != rootTaskID {
		return opErr(op, ErrInvalid, fmt.Errorf("root task does not match parent family"))
	}
	task.RootTaskID = rootTaskID
	_, err = tx.ExecContext(ctx, `
	INSERT INTO tasks(id, principal_id, objective, state, terminal_reason, catalog_version, sensitivity,
	                  request_context_json, approval_ref, created_at, updated_at, expires_at,
	                  root_task_id, parent_task_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NULLIF($14, ''))`, task.ID, task.PrincipalID, task.Objective, task.State,
		task.TerminalReason, task.CatalogVersion, task.Sensitivity, string(requestContext), task.ApprovalRef,
		dbTime(task.CreatedAt), dbTime(task.UpdatedAt), nullableTime(task.ExpiresAt), task.RootTaskID, task.ParentTaskID)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	actor := task.PrincipalID
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: task.ID, Actor: actor, EventType: "TASK_CREATED", OccurredAt: task.CreatedAt,
		Payload: mustJSON(map[string]any{"state": task.State, "catalog_version": task.CatalogVersion,
			"root_task_id": task.RootTaskID, "parent_task_id": task.ParentTaskID}),
	})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	const op = "get task"
	if err := s.checkOpen(op); err != nil {
		return Task{}, err
	}
	return scanTask(op, s.db.QueryRowContext(ctx, taskSelect+` WHERE id = $1`, id))
}

const taskSelect = `SELECT id, principal_id, objective, state, terminal_reason, catalog_version, sensitivity,
	request_context_json, approval_ref, created_at, updated_at, expires_at,
	root_task_id, COALESCE(parent_task_id, '') FROM tasks`

func scanTask(op string, row rowScanner) (Task, error) {
	var task Task
	var requestContext []byte
	var created, updated time.Time
	var expires sql.NullTime
	if err := row.Scan(&task.ID, &task.PrincipalID, &task.Objective, &task.State, &task.TerminalReason,
		&task.CatalogVersion, &task.Sensitivity, &requestContext, &task.ApprovalRef, &created, &updated, &expires,
		&task.RootTaskID, &task.ParentTaskID); err != nil {
		if isNoRows(err) {
			return Task{}, opErr(op, ErrNotFound, err)
		}
		return Task{}, opErr(op, ErrConflict, err)
	}
	task.RequestContext = append(json.RawMessage(nil), requestContext...)
	task.CreatedAt = dbTime(created)
	task.UpdatedAt = dbTime(updated)
	task.ExpiresAt = scanNullableTime(expires)
	return task, nil
}

func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	const op = "list tasks"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, taskSelect+`
WHERE ($1 = '' OR principal_id = $2) AND ($3 = '' OR state = $4) AND ($5 = '' OR id > $6)
ORDER BY id LIMIT $7`, filter.PrincipalID, filter.PrincipalID, filter.State, filter.State,
		filter.AfterID, filter.AfterID, limit)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(op, rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return tasks, nil
}

type TaskTransition struct {
	TaskID       string
	ExpectedFrom TaskState
	To           TaskState
	Reason       TerminalReason
	Actor        string
	EventID      string
	Payload      json.RawMessage
	ExpiresAt    *time.Time
}

func (s *Store) TransitionTask(ctx context.Context, change TaskTransition) (Task, error) {
	const op = "transition task"
	if err := s.checkOpen(op); err != nil {
		return Task{}, err
	}
	if change.TaskID == "" || change.Actor == "" || !validTaskState(change.To) {
		return Task{}, opErr(op, ErrInvalid, fmt.Errorf("task, actor, and target state are required"))
	}
	payload, err := normalizeJSON(change.Payload, `{}`)
	if err != nil {
		return Task{}, opErr(op, ErrInvalid, err)
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return Task{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	current, err := scanTask(op, tx.QueryRowContext(ctx, taskSelect+` WHERE id = $1 FOR UPDATE`, change.TaskID))
	if err != nil {
		return Task{}, err
	}
	if change.ExpectedFrom != "" && current.State != change.ExpectedFrom {
		return Task{}, opErr(op, ErrConflict, fmt.Errorf("expected %s, found %s", change.ExpectedFrom, current.State))
	}
	if !allowedTransition(current.State, change.To, change.Reason) {
		return Task{}, opErr(op, ErrInvalidStateChange, fmt.Errorf("%s -> %s (%s)", current.State, change.To, change.Reason))
	}
	now := s.now()
	expires := current.ExpiresAt
	if change.ExpiresAt != nil {
		expires = change.ExpiresAt
	}
	_, err = tx.ExecContext(ctx, `UPDATE tasks SET state=$1, terminal_reason=$2, updated_at=$3, expires_at=$4 WHERE id=$5`,
		change.To, change.Reason, dbTime(now), nullableTime(expires), change.TaskID)
	if err != nil {
		return Task{}, opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		EventID: change.EventID, TaskID: change.TaskID, Actor: change.Actor, EventType: "TASK_STATE_CHANGED",
		Payload:    mustJSON(map[string]any{"from": current.State, "to": change.To, "terminal_reason": change.Reason, "detail": json.RawMessage(payload)}),
		OccurredAt: now,
	})
	if err != nil {
		return Task{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return Task{}, opErr(op, ErrConflict, err)
	}
	current.State, current.TerminalReason, current.UpdatedAt, current.ExpiresAt = change.To, change.Reason, now, expires
	return current, nil
}

func validTaskState(state TaskState) bool {
	switch state {
	case TaskAwaitingSubmission, TaskAwaitingApproval, TaskActive, TaskArchived:
		return true
	default:
		return false
	}
}

func allowedTransition(from, to TaskState, reason TerminalReason) bool {
	if to == TaskArchived {
		if reason == "" {
			return false
		}
		switch reason {
		case TerminalCompleted, TerminalBudgetExhausted, TerminalRejected, TerminalExpired, TerminalRevoked, TerminalFailed,
			TerminalApproverNotAuthorized:
		default:
			return false
		}
	} else if reason != "" {
		return false
	}
	switch from {
	case TaskAwaitingSubmission:
		return to == TaskAwaitingApproval || to == TaskArchived
	case TaskAwaitingApproval:
		return to == TaskActive || to == TaskArchived
	case TaskActive:
		return to == TaskArchived
	default:
		return false
	}
}

func (s *Store) PutGrant(ctx context.Context, grant TaskGrant) error {
	const op = "put task grant"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if grant.TaskID == "" || grant.Subject == "" || grant.Purpose == "" || grant.CatalogVersion == "" ||
		!validSHA256Hex(grant.CatalogDigest) || grant.DatasourceID == "" || !validSHA256Hex(grant.SchemaDigest) ||
		grant.ApprovalReceipt == "" || grant.ExpiresAt.IsZero() {
		return opErr(op, ErrInvalid, fmt.Errorf("required grant field is empty"))
	}
	if err := validateGrantViewBinding(grant); err != nil {
		return opErr(op, ErrInvalid, err)
	}
	if grant.Budget.Queries <= 0 || grant.Budget.Rows <= 0 || grant.Budget.DBMS <= 0 {
		return opErr(op, ErrInvalid, fmt.Errorf("all budget limits must be positive"))
	}
	if err := validateExposureGrant(grant.Exposure); err != nil {
		return opErr(op, ErrInvalid, err)
	}
	products, err := json.Marshal(grant.ApprovedProducts)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	columns, err := json.Marshal(grant.ApprovedColumns)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	scope, err := normalizeJSON(grant.MandatoryScope, `{}`)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	if grant.CreatedAt.IsZero() {
		grant.CreatedAt = s.now()
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	var state TaskState
	if err := tx.QueryRowContext(ctx, `SELECT state FROM tasks WHERE id=$1 FOR UPDATE`, grant.TaskID).Scan(&state); err != nil {
		if isNoRows(err) {
			return opErr(op, ErrNotFound, err)
		}
		return opErr(op, ErrConflict, err)
	}
	if state != TaskAwaitingApproval && state != TaskActive {
		return opErr(op, ErrInvalidStateChange, fmt.Errorf("cannot grant task in state %s", state))
	}
	if err := insertGrantAndBudget(ctx, tx, grant, products, columns, scope); err != nil {
		return opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: grant.TaskID, Actor: grant.Subject, EventType: "TASK_GRANT_CREATED", OccurredAt: grant.CreatedAt,
		Payload: mustJSON(map[string]any{
			"catalog_version": grant.CatalogVersion, "catalog_digest": grant.CatalogDigest,
			"datasource_id": grant.DatasourceID, "schema_digest": grant.SchemaDigest,
			"view_binding_digest": grant.ViewBindingDigest,
			"budget":              grant.Budget, "exposure": grant.Exposure, "expires_at": formatTime(grant.ExpiresAt),
		}),
	})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func insertGrantAndBudget(ctx context.Context, tx *sql.Tx, grant TaskGrant, products, columns, scope []byte) error {
	predicate := PredicateFootprintLimitsV1{}
	if grant.Exposure.PredicateFootprint != nil {
		predicate = *grant.Exposure.PredicateFootprint
	}
	_, err := tx.ExecContext(ctx, `
	INSERT INTO task_grants(task_id, subject, purpose, approved_products_json, approved_columns_json,
	 mandatory_scope_json, sensitivity_ceiling, max_queries, max_rows, max_db_ms, expires_at,
	 catalog_version, catalog_digest, datasource_id, schema_digest, approval_receipt, created_at,
	 max_release_facts, max_influence_facts, max_outcome_facts, exposure_profile_version, view_binding_digest,
	 predicate_footprint_version,predicate_max_raw_literals,predicate_max_unique_atoms,
	 predicate_max_atom_payload_bytes,predicate_max_total_payload_bytes)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
	 $23,$24,$25,$26,$27)`, grant.TaskID, grant.Subject, grant.Purpose,
		string(products), string(columns), string(scope), grant.SensitivityCeiling, grant.Budget.Queries, grant.Budget.Rows,
		grant.Budget.DBMS, dbTime(grant.ExpiresAt), grant.CatalogVersion, grant.CatalogDigest,
		grant.DatasourceID, grant.SchemaDigest, grant.ApprovalReceipt,
		dbTime(grant.CreatedAt), grant.Exposure.Limits.ReleaseFacts, grant.Exposure.Limits.InfluenceFacts, grant.Exposure.Limits.OutcomeFacts,
		grant.Exposure.ProfileVersion, grant.ViewBindingDigest, predicate.Version, predicate.MaxRawLiteralsPerQuery,
		predicate.MaxUniqueAtomsPerQuery, predicate.MaxAtomPayloadBytes, predicate.MaxTotalAtomPayloadBytes)
	if err != nil {
		return err
	}
	if grant.ViewBindingSet != nil {
		if err := insertTaskViewBindingTx(ctx, tx, grant.TaskID, *grant.ViewBindingSet, dbTime(grant.CreatedAt)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO budget_ledger(task_id, max_queries, max_rows, max_db_ms, updated_at)
	VALUES ($1, $2, $3, $4, $5)`, grant.TaskID, grant.Budget.Queries, grant.Budget.Rows, grant.Budget.DBMS,
		dbTime(grant.CreatedAt))
	if err != nil {
		return err
	}
	return ensureExposureLedgerTx(ctx, tx, grant.TaskID, grant.Exposure, grant.CreatedAt)
}

func (s *Store) GetGrant(ctx context.Context, taskID string) (TaskGrant, error) {
	const op = "get task grant"
	if err := s.checkOpen(op); err != nil {
		return TaskGrant{}, err
	}
	var grant TaskGrant
	var products, columns, scope []byte
	var expires, created time.Time
	var predicate PredicateFootprintLimitsV1
	err := s.db.QueryRowContext(ctx, `
	 SELECT task_id, subject, purpose, approved_products_json, approved_columns_json, mandatory_scope_json,
	 sensitivity_ceiling, max_queries, max_rows, max_db_ms, expires_at, catalog_version, catalog_digest,
	 datasource_id, schema_digest, approval_receipt, created_at, max_release_facts, max_influence_facts, max_outcome_facts,
	 exposure_profile_version, view_binding_digest,predicate_footprint_version,predicate_max_raw_literals,
	 predicate_max_unique_atoms,predicate_max_atom_payload_bytes,predicate_max_total_payload_bytes
	 FROM task_grants WHERE task_id=$1`, taskID).Scan(&grant.TaskID, &grant.Subject, &grant.Purpose, &products,
		&columns, &scope, &grant.SensitivityCeiling, &grant.Budget.Queries, &grant.Budget.Rows, &grant.Budget.DBMS,
		&expires, &grant.CatalogVersion, &grant.CatalogDigest, &grant.DatasourceID, &grant.SchemaDigest,
		&grant.ApprovalReceipt, &created, &grant.Exposure.Limits.ReleaseFacts, &grant.Exposure.Limits.InfluenceFacts, &grant.Exposure.Limits.OutcomeFacts,
		&grant.Exposure.ProfileVersion, &grant.ViewBindingDigest, &predicate.Version, &predicate.MaxRawLiteralsPerQuery,
		&predicate.MaxUniqueAtomsPerQuery, &predicate.MaxAtomPayloadBytes, &predicate.MaxTotalAtomPayloadBytes)
	if err != nil {
		if isNoRows(err) {
			return TaskGrant{}, opErr(op, ErrNotFound, err)
		}
		return TaskGrant{}, opErr(op, ErrConflict, err)
	}
	if err := json.Unmarshal(products, &grant.ApprovedProducts); err != nil {
		return TaskGrant{}, opErr(op, ErrConflict, err)
	}
	if err := json.Unmarshal(columns, &grant.ApprovedColumns); err != nil {
		return TaskGrant{}, opErr(op, ErrConflict, err)
	}
	grant.MandatoryScope = append(json.RawMessage(nil), scope...)
	grant.ExpiresAt = dbTime(expires)
	grant.CreatedAt = dbTime(created)
	if predicate.Version != "" {
		grant.Exposure.PredicateFootprint = &predicate
	}
	return grant, nil
}

func (s *Store) RecordApprovalEvent(ctx context.Context, event ApprovalEvent) error {
	const op = "record approval event"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	if event.EventID == "" || event.TaskID == "" || event.Actor == "" || event.Decision == "" {
		return opErr(op, ErrInvalid, fmt.Errorf("event, task, actor, and decision are required"))
	}
	payload, err := normalizeJSON(event.Payload, `{}`)
	if err != nil {
		return opErr(op, ErrInvalid, err)
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = s.now()
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	_, err = tx.ExecContext(ctx, `
INSERT INTO approval_events(event_id, task_id, actor, decision, payload_json, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, event.EventID, event.TaskID, event.Actor, event.Decision, string(payload), dbTime(event.CreatedAt))
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{
		TaskID: event.TaskID, Actor: event.Actor, EventType: "APPROVAL_EVENT_RECORDED",
		Payload: mustJSON(map[string]any{"event_id": event.EventID, "decision": event.Decision}), OccurredAt: event.CreatedAt,
	})
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrConflict, err)
	}
	return nil
}

func (s *Store) ListApprovalEvents(ctx context.Context, taskID string) ([]ApprovalEvent, error) {
	const op = "list approval events"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT event_id, task_id, actor, decision, payload_json, created_at
FROM approval_events WHERE task_id=$1 ORDER BY created_at, event_id`, taskID)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	var events []ApprovalEvent
	for rows.Next() {
		var event ApprovalEvent
		var payload []byte
		var created time.Time
		if err := rows.Scan(&event.EventID, &event.TaskID, &event.Actor, &event.Decision, &payload, &created); err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		event.Payload = append(json.RawMessage(nil), payload...)
		event.CreatedAt = dbTime(created)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return events, nil
}
