package mysql

import (
	"context"
	"database/sql"
	"fmt"

	driver "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/config"
)

const createMetaDatabaseSQL = `CREATE DATABASE IF NOT EXISTS unredo_meta CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci`

const createActionMarkersSQL = `CREATE TABLE IF NOT EXISTS unredo_meta.action_markers (
    action_id        BINARY(16)   NOT NULL,
    plan_id          BINARY(16)   NOT NULL,
    parent_action_id BINARY(16)   NULL,
    root_plan_digest BINARY(32)   NOT NULL,
    action_type      ENUM('REVERT', 'REAPPLY') NOT NULL,
    target_state     ENUM('ORIGINAL_APPLIED', 'ORIGINAL_REVERTED') NOT NULL,
    chain_depth      INT UNSIGNED NOT NULL,
    source_native_transaction_id VARCHAR(255) NOT NULL,
    plan_digest      BINARY(32)   NOT NULL,
    execution_class  ENUM('SAFE', 'UNSAFE_RESOLVED') NOT NULL,
    reason           VARCHAR(1024) NULL,
    tool_version     VARCHAR(32)  NOT NULL,
    operator_name    VARCHAR(255) NOT NULL,
    created_at       TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (action_id),
    UNIQUE KEY uq_plan_id (plan_id),
    KEY idx_parent_action (parent_action_id),
    UNIQUE KEY uq_root_depth (root_plan_digest, chain_depth)
) ENGINE=InnoDB`

// ApplyMetaMigration creates only Unredo's metadata schema. It never creates
// users, changes global MySQL variables, or modifies business schemas.
func ApplyMetaMigration(ctx context.Context, address, user, passwordEnv string) error {
	password, err := config.ResolvePassword(passwordEnv)
	if err != nil {
		return err
	}
	cfg := driver.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = address
	cfg.MultiStatements = false
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return fmt.Errorf("mysql bootstrap: open: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("mysql bootstrap: connect: %w", err)
	}
	var databaseExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = 'unredo_meta'`).Scan(&databaseExists); err != nil {
		return fmt.Errorf("mysql bootstrap: inspect metadata database: %w", err)
	}
	if databaseExists == 0 {
		if _, err := db.ExecContext(ctx, createMetaDatabaseSQL); err != nil {
			return fmt.Errorf("mysql bootstrap: create metadata database: %w", err)
		}
	}
	var tableExists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'unredo_meta' AND table_name = 'action_markers'`).Scan(&tableExists); err != nil {
		return fmt.Errorf("mysql bootstrap: inspect action marker table: %w", err)
	}
	if tableExists == 0 {
		if _, err := db.ExecContext(ctx, createActionMarkersSQL); err != nil {
			return fmt.Errorf("mysql bootstrap: create action marker table: %w", err)
		}
	}
	return nil
}
