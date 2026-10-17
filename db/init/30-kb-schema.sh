#!/bin/sh
set -eu

# Installs the Knowledge Product source schema (kb-schema.psql, same directory)
# when the pgvector extension is available on this server. The default
# compose.yaml business-postgres image ships pgvector, so the schema is
# installed there. KB_REQUIRE_PGVECTOR defaults to 0 because the formal and
# db-test topologies pin a plain PostgreSQL image for this service and never
# publish a knowledge product; a knowledge-product deployment sets it to 1 so
# a missing extension fails initialization instead of being skipped.

schema_file=${KB_SCHEMA_FILE:-/docker-entrypoint-initdb.d/kb-schema.psql}
available=$(psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --no-psqlrc --tuples-only --no-align \
  --command "SELECT count(*) FROM pg_available_extensions WHERE name = 'vector'")
if [ "$available" != "1" ]; then
  if [ "${KB_REQUIRE_PGVECTOR:-0}" = "1" ]; then
    echo "kb-schema: pgvector is required (KB_REQUIRE_PGVECTOR=1) but is not available on this server" >&2
    exit 1
  fi
  echo "kb-schema: pgvector is not available; skipping the Knowledge Product schema (set KB_REQUIRE_PGVECTOR=1 to require it)"
  exit 0
fi
psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --no-psqlrc --set ON_ERROR_STOP=1 --file "$schema_file"
echo "kb-schema: Knowledge Product schema installed"
