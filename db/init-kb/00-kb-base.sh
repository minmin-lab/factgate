#!/bin/sh
set -eu

# Business database base objects for a knowledge-product deployment (the
# formal round, docs/kb_experiment_design.md). It replaces db/init, whose V5
# scripts create and grant the V5 benchmark relations a knowledge-product
# Catalog never declares. Each block below is the V5 definition verbatim;
# evaluation/kbformal's drift test fails if one of them changes there without
# changing here.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv gateway_password GATEWAY_DB_PASSWORD

-- db/init/00-schema.sql: the reporting surface and its datasource identity.
CREATE SCHEMA reporting;

CREATE TABLE reporting.datasource_attestation (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    datasource_id TEXT NOT NULL
);
INSERT INTO reporting.datasource_attestation(singleton, datasource_id)
VALUES (TRUE, 'taskgate-demo-travel');

-- db/init/05-ordinal-sidecars.sql: the sealed sidecar namespace.
CREATE SCHEMA taskgate_ordinal;

REVOKE ALL ON SCHEMA taskgate_ordinal FROM PUBLIC;

-- db/init/07-freeze-publications.sql: the snapshot owner and the
-- mutation-rejection primitive the sidecar installer attaches.
CREATE ROLE taskgate_snapshot_owner
    NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;

CREATE FUNCTION taskgate_ordinal.reject_frozen_publication_mutation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $taskgate_reject_mutation$
BEGIN
    RAISE EXCEPTION USING
        ERRCODE = '55000',
        MESSAGE = format(
            'TaskGate frozen publication relation %I.%I rejects %s',
            TG_TABLE_SCHEMA,
            TG_TABLE_NAME,
            TG_OP
        );
END
$taskgate_reject_mutation$;

REVOKE ALL ON FUNCTION taskgate_ordinal.reject_frozen_publication_mutation() FROM PUBLIC;

-- db/init/10-reader.sh: the read-only Gateway role, granted only what a
-- knowledge-product Catalog reads before kb-publish adds its relations.
CREATE ROLE gateway_reader
  LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS
  PASSWORD :'gateway_password';
ALTER ROLE gateway_reader SET default_transaction_read_only = on;
ALTER ROLE gateway_reader SET statement_timeout = '5s';
ALTER ROLE gateway_reader SET search_path = pg_catalog;
GRANT CONNECT ON DATABASE :DBNAME TO gateway_reader;
GRANT USAGE ON SCHEMA reporting TO gateway_reader;
GRANT SELECT ON reporting.datasource_attestation TO gateway_reader;
GRANT USAGE ON SCHEMA taskgate_ordinal TO gateway_reader;
SQL
