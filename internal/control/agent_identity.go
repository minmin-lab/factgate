package control

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// AgentIdentity is a demo binding (KB1-LOOP-151, migration 023): the
// principal one agent token currently acts as. The token is known by its
// SHA-256 only. A person switches the binding in the OA; the agent cannot.
// Nothing on the formal-round path reads or writes it.
type AgentIdentity struct {
	Label       string
	TokenSHA256 string
	PrincipalID string
	BoundBy     string
	BoundAt     time.Time
	CreatedAt   time.Time
}

// agentLabelPattern is the label CHECK of migration 023.
var agentLabelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidAgentLabel reports whether label satisfies the demo_agent_identities
// label constraint.
func ValidAgentLabel(label string) bool { return agentLabelPattern.MatchString(label) }

const agentIdentityColumns = `label, token_sha256, principal_id, bound_by, bound_at, created_at`

// RegisterAgentIdentity binds a label's token digest to a principal, creating
// the label or rebinding it (token digest and principal may both change; the
// label's created_at is kept), and records the binding in the switch history
// with the previous principal, or NULL for a new label.
func (s *Store) RegisterAgentIdentity(ctx context.Context, identity AgentIdentity) error {
	const op = "register agent identity"
	if err := s.checkOpen(op); err != nil {
		return err
	}
	identity.Label = strings.TrimSpace(identity.Label)
	identity.PrincipalID = strings.TrimSpace(identity.PrincipalID)
	identity.BoundBy = strings.TrimSpace(identity.BoundBy)
	if !ValidAgentLabel(identity.Label) || !validSHA256Hex(identity.TokenSHA256) || identity.PrincipalID == "" || identity.BoundBy == "" {
		return opErr(op, ErrInvalid, fmt.Errorf("a valid label, a lowercase SHA-256 token digest, a principal and who bound it are required"))
	}
	if identity.BoundAt.IsZero() {
		identity.BoundAt = s.now()
	}
	if identity.CreatedAt.IsZero() {
		identity.CreatedAt = identity.BoundAt
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	// Writers of the binding table are serialized so the switch history's
	// from_principal_id is the binding this write actually replaced, also for
	// two first registrations of one label. Readers (token lookups) are not
	// blocked by this lock mode.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE demo_agent_identities IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if exists, err := principalExistsTx(ctx, tx, identity.PrincipalID); err != nil {
		return opErr(op, ErrConflict, err)
	} else if !exists {
		return opErr(op, ErrNotFound, fmt.Errorf("principal %q does not exist", identity.PrincipalID))
	}
	var previous sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT principal_id FROM demo_agent_identities WHERE label = $1`, identity.Label).Scan(&previous)
	if err != nil && !isNoRows(err) {
		return opErr(op, ErrConflict, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO demo_agent_identities(label, token_sha256, principal_id, bound_by, bound_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (label) DO UPDATE SET
    token_sha256 = EXCLUDED.token_sha256,
    principal_id = EXCLUDED.principal_id,
    bound_by = EXCLUDED.bound_by,
    bound_at = EXCLUDED.bound_at`,
		identity.Label, identity.TokenSHA256, identity.PrincipalID, identity.BoundBy,
		dbTime(identity.BoundAt), dbTime(identity.CreatedAt)); err != nil {
		// The token digest is UNIQUE: one token acts for one label.
		return opErr(op, ErrConflict, err)
	}
	if err := appendAgentIdentitySwitchTx(ctx, tx, identity.Label, previous, identity.PrincipalID, identity.BoundBy, identity.BoundAt); err != nil {
		return opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return opErr(op, ErrCommitOutcomeUnknown, err)
	}
	return nil
}

// SwitchAgentIdentity rebinds an existing label to another principal and
// records the switch. An unknown label is ErrNotFound.
func (s *Store) SwitchAgentIdentity(ctx context.Context, label, principalID, by string, at time.Time) (AgentIdentity, error) {
	const op = "switch agent identity"
	if err := s.checkOpen(op); err != nil {
		return AgentIdentity{}, err
	}
	label, principalID, by = strings.TrimSpace(label), strings.TrimSpace(principalID), strings.TrimSpace(by)
	if !ValidAgentLabel(label) || principalID == "" || by == "" {
		return AgentIdentity{}, opErr(op, ErrInvalid, fmt.Errorf("a valid label, a principal and who switched it are required"))
	}
	if at.IsZero() {
		at = s.now()
	}
	tx, err := beginTx(ctx, s.db)
	if err != nil {
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `LOCK TABLE demo_agent_identities IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	}
	var previous sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT principal_id FROM demo_agent_identities WHERE label = $1`, label).Scan(&previous); err != nil {
		if isNoRows(err) {
			return AgentIdentity{}, opErr(op, ErrNotFound, err)
		}
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	}
	if exists, err := principalExistsTx(ctx, tx, principalID); err != nil {
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	} else if !exists {
		return AgentIdentity{}, opErr(op, ErrNotFound, fmt.Errorf("principal %q does not exist", principalID))
	}
	identity, err := scanAgentIdentity(tx.QueryRowContext(ctx, `
UPDATE demo_agent_identities SET principal_id = $2, bound_by = $3, bound_at = $4
WHERE label = $1
RETURNING `+agentIdentityColumns, label, principalID, by, dbTime(at)))
	if err != nil {
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	}
	if err := appendAgentIdentitySwitchTx(ctx, tx, label, previous, principalID, by, at); err != nil {
		return AgentIdentity{}, opErr(op, ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return AgentIdentity{}, opErr(op, ErrCommitOutcomeUnknown, err)
	}
	return identity, nil
}

// LookupAgentIdentityByToken returns the binding of a token digest
// (lowercase hex SHA-256), ErrNotFound when no label holds it.
func (s *Store) LookupAgentIdentityByToken(ctx context.Context, tokenSHA256 string) (AgentIdentity, error) {
	const op = "lookup agent identity by token"
	if err := s.checkOpen(op); err != nil {
		return AgentIdentity{}, err
	}
	if !validSHA256Hex(tokenSHA256) {
		return AgentIdentity{}, opErr(op, ErrInvalid, fmt.Errorf("token digest must be lowercase SHA-256 hex"))
	}
	identity, err := scanAgentIdentity(s.db.QueryRowContext(ctx, `
SELECT `+agentIdentityColumns+` FROM demo_agent_identities WHERE token_sha256 = $1`, tokenSHA256))
	if err != nil {
		return AgentIdentity{}, agentIdentityReadErr(op, err)
	}
	return identity, nil
}

// GetAgentIdentity returns a label's binding, ErrNotFound for an unknown label.
func (s *Store) GetAgentIdentity(ctx context.Context, label string) (AgentIdentity, error) {
	const op = "get agent identity"
	if err := s.checkOpen(op); err != nil {
		return AgentIdentity{}, err
	}
	identity, err := scanAgentIdentity(s.db.QueryRowContext(ctx, `
SELECT `+agentIdentityColumns+` FROM demo_agent_identities WHERE label = $1`, label))
	if err != nil {
		return AgentIdentity{}, agentIdentityReadErr(op, err)
	}
	return identity, nil
}

// ListAgentIdentities returns every binding, ordered by label.
func (s *Store) ListAgentIdentities(ctx context.Context) ([]AgentIdentity, error) {
	const op = "list agent identities"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentIdentityColumns+` FROM demo_agent_identities ORDER BY label`)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	identities := []AgentIdentity{}
	for rows.Next() {
		identity, err := scanAgentIdentity(rows)
		if err != nil {
			return nil, opErr(op, ErrConflict, err)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return identities, nil
}

// ListPrincipals returns every principal, disabled ones included, ordered by
// subject.
func (s *Store) ListPrincipals(ctx context.Context) ([]Principal, error) {
	const op = "list principals"
	if err := s.checkOpen(op); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, subject, role, token_hash, created_at, disabled_at FROM principals ORDER BY subject`)
	if err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	defer rows.Close()
	principals := []Principal{}
	for rows.Next() {
		principal, err := scanPrincipal(op, rows)
		if err != nil {
			return nil, err
		}
		principals = append(principals, principal)
	}
	if err := rows.Err(); err != nil {
		return nil, opErr(op, ErrConflict, err)
	}
	return principals, nil
}

func principalExistsTx(ctx context.Context, tx *sql.Tx, principalID string) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM principals WHERE id = $1`, principalID).Scan(&exists); err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func appendAgentIdentitySwitchTx(ctx context.Context, tx *sql.Tx, label string, from sql.NullString, to, by string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO demo_agent_identity_switches(label, from_principal_id, to_principal_id, switched_by, switched_at)
VALUES ($1, $2, $3, $4, $5)`, label, from, to, by, dbTime(at))
	return err
}

func scanAgentIdentity(row rowScanner) (AgentIdentity, error) {
	var identity AgentIdentity
	var bound, created time.Time
	if err := row.Scan(&identity.Label, &identity.TokenSHA256, &identity.PrincipalID, &identity.BoundBy, &bound, &created); err != nil {
		return AgentIdentity{}, err
	}
	identity.BoundAt, identity.CreatedAt = dbTime(bound), dbTime(created)
	return identity, nil
}

func agentIdentityReadErr(op string, err error) error {
	if isNoRows(err) {
		return opErr(op, ErrNotFound, err)
	}
	return opErr(op, ErrConflict, err)
}
