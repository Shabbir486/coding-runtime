-- =============================================================================
-- CodeRuntime — PostgreSQL Initialization Script
-- =============================================================================
-- Executed once when the Postgres container first starts (via initdb hook).
-- Table DDL is entirely owned by GORM AutoMigrate at API startup; this file
-- only installs extensions and grants the minimum privileges the app role needs
-- before any tables exist.
-- =============================================================================

-- uuid_generate_v4() is used by the Go layer to generate submission tokens.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- pg_stat_statements enables per-query execution statistics, used by Grafana
-- dashboards and performance investigations.
CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";

-- btree_gin allows multi-column GIN indexes that mix equality and range
-- predicates — useful for (language_id, created_at) query patterns.
CREATE EXTENSION IF NOT EXISTS "btree_gin";

-- Give the application role full control over the database so GORM
-- AutoMigrate can CREATE, ALTER, and DROP tables on startup without a
-- separate migration user.
GRANT ALL PRIVILEGES ON DATABASE coderuntime TO coderuntime;