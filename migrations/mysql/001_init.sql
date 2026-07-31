-- M2: unredo_meta schema. M0 keeps the same file in sync so a single
-- init script can bootstrap both stages. The schema is identical to
-- DESIGN.md §8.
CREATE DATABASE IF NOT EXISTS unredo_meta CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS unredo_meta.action_markers (
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
) ENGINE=InnoDB;
