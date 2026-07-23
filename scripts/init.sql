-- =============================================================================
-- CodeRuntime — MySQL Initialization Script
-- =============================================================================
-- Executed once when the MySQL container first starts (via the
-- /docker-entrypoint-initdb.d hook), after MYSQL_DATABASE / MYSQL_USER have
-- been created. Table DDL is entirely owned by GORM AutoMigrate at API
-- startup; this file only ensures the database exists with the right charset
-- and that the application user has full privileges on it before any tables
-- are created.
--
-- AWS RDS note: RDS does not run this init hook. There, create the database
-- and grant privileges once via the master user:
--   CREATE DATABASE coderuntime CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
--   GRANT ALL PRIVILEGES ON coderuntime.* TO 'coderuntime'@'%';
-- =============================================================================

CREATE DATABASE IF NOT EXISTS coderuntime
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_0900_ai_ci;

-- Give the application user full control of the database so GORM AutoMigrate
-- can CREATE / ALTER / DROP tables on startup without a separate migration user.
GRANT ALL PRIVILEGES ON coderuntime.* TO 'coderuntime'@'%';
FLUSH PRIVILEGES;
