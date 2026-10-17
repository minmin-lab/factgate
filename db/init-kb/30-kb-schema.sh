#!/bin/sh
set -eu

# The knowledge-product source schema, installed by the same script and SQL a
# default deployment uses (mounted read-only at /opt/taskgate/db-init). A
# knowledge-product deployment sets KB_REQUIRE_PGVECTOR=1, so a business image
# without pgvector fails initialization here.
KB_SCHEMA_FILE=/opt/taskgate/db-init/kb-schema.psql exec sh /opt/taskgate/db-init/30-kb-schema.sh
