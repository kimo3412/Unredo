-- Unredo M0 schema: action_markers, test shop schema, and grant the executor.
-- Re-runnable: drops and recreates the test DBs.
-- Run as: mysql -uroot -p123456 < scripts/init_m0_schema.sql

DROP DATABASE IF EXISTS unredo_meta;
DROP DATABASE IF EXISTS unredo_shop;

CREATE DATABASE unredo_meta CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE DATABASE unredo_shop CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- M2 will own this; created now so the executor already has access.
CREATE TABLE unredo_meta.action_markers (
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

-- Test shop tables for M0 fixtures.
CREATE TABLE unredo_shop.orders (
    id          BIGINT       NOT NULL AUTO_INCREMENT,
    user_id     BIGINT       NOT NULL,
    status      VARCHAR(16)  NOT NULL,
    amount      DECIMAL(12,2) NOT NULL,
    created_at  TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY idx_user (user_id)
) ENGINE=InnoDB;

CREATE TABLE unredo_shop.payments (
    id         BIGINT      NOT NULL AUTO_INCREMENT,
    order_id   BIGINT      NOT NULL,
    method     VARCHAR(16) NOT NULL,
    amount     DECIMAL(12,2) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_order (order_id)
) ENGINE=InnoDB;

-- Wide / BLOB table used by M0 to baseline large-transaction limits.
CREATE TABLE unredo_shop.large_rows (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    payload    VARBINARY(8192) NULL,
    note       TEXT         NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB;

GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_shop.* TO 'unredo_executor'@'127.0.0.1';
GRANT SELECT, INSERT, UPDATE, DELETE ON unredo_meta.* TO 'unredo_executor'@'127.0.0.1';
FLUSH PRIVILEGES;
