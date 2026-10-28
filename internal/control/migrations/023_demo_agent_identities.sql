-- Demo only (KB1-LOOP-151): which principal one agent token currently acts as.
--
-- The demo gives a tester one agent program and one MCP token. The principal
-- that token represents is switched by a person on the OA's "my agent" page;
-- the agent itself has no way to switch it. The gateway stores the token's
-- SHA-256 only, never the token. A binding is consulted before the static
-- principal table, so an existing token's digest registered under a label
-- switches without any client change.
--
-- Nothing on the formal-round path reads or writes these tables: the gateway
-- consults them only when its Catalog declares delegated_authorization, which
-- the formal Catalog does not. Creating them empty changes no existing row,
-- constraint or query.
--
-- demo_agent_identity_switches keeps every binding a label has had, the first
-- registration included (from_principal_id NULL), so who acted as whom when
-- stays answerable after a switch.
CREATE TABLE IF NOT EXISTS demo_agent_identities (
    label        TEXT PRIMARY KEY CHECK (label ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    token_sha256 TEXT NOT NULL UNIQUE CHECK (token_sha256 ~ '^[0-9a-f]{64}$'),
    principal_id TEXT NOT NULL REFERENCES principals(id),
    bound_by     TEXT NOT NULL,
    bound_at     TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS demo_agent_identity_switches (
    id                BIGSERIAL PRIMARY KEY,
    label             TEXT NOT NULL REFERENCES demo_agent_identities(label),
    from_principal_id TEXT,
    to_principal_id   TEXT NOT NULL,
    switched_by       TEXT NOT NULL,
    switched_at       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS demo_agent_identity_switches_label_idx
    ON demo_agent_identity_switches(label, id);
