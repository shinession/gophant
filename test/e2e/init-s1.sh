#!/bin/sh
# Runs once when the s1 container is initialized: creates the replication account described in
# section 8 of docs/gophant.md and lets it use the replication protocol from other containers.
set -eu

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
CREATE ROLE gophant_repl WITH LOGIN REPLICATION PASSWORD '${POSTGRES_PASSWORD}';
GRANT pg_read_all_data TO gophant_repl;
-- Lets the cutover see the transactions of the application sessions (section 8 of the design does not
-- list it, gophant warns when it is missing).
GRANT pg_read_all_stats TO gophant_repl;
SQL

# The image only trusts "all" databases for normal connections; replication needs its own line.
echo "host replication gophant_repl all scram-sha-256" >> "$PGDATA/pg_hba.conf"
