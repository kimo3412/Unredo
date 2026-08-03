# Unredo

Unredo 计划成为一个用 Go 编写的数据库事务补偿 CLI。首个后端面向 MySQL 8 ROW binlog：选择一笔已提交事务，生成自包含的补偿计划，检查当前数据冲突，并以一笔新的事务安全地 revert 或 reapply。

它的语义类似 `git revert`，不会倒放或修改 InnoDB redo log。

> 当前状态：**M0-M2 已完成，M3 核心 reapply、冲突 resolve 与初始化流程已跑通**。性能基准、完整交替 action 链和实验版发布仍待办。

## 计划中的五分钟上手流程

### 1. 检查 MySQL 前置条件

首版要求：

```ini
log_bin=ON
binlog_format=ROW
binlog_row_image=FULL
binlog_row_metadata=FULL
gtid_mode=ON
enforce_gtid_consistency=ON
```

部分配置需要 DBA 修改 MySQL 配置并重启。Unredo 不会静默修改或重启数据库。

### 2. 初始化账号、元数据库与配置

`unredo init` 会生成 profile、随机并持久化 replication `server_id`、输出最小权限 GRANT SQL，并在最后运行 doctor。它不会生成默认账号密码，也不会把密码写入配置：

```bash
unredo init \
  --profile production \
  --address mysql.example.com:3306 \
  --database shop \
  --database billing
```

首次运行时如果 reader/executor 账号还不存在，可以先只生成文件：

```bash
unredo init \
  --profile production \
  --address mysql.example.com:3306 \
  --database shop \
  --skip-doctor
```

DBA 审查并应用生成的 GRANT SQL、设置密码环境变量后，再运行 `unredo doctor`。如需让 init 创建 `unredo_meta`，必须显式提供临时管理凭据引用：

```bash
export MYSQL_ADMIN_PASSWORD='...'
unredo init \
  --profile production \
  --address mysql.example.com:3306 \
  --database shop \
  --apply-meta \
  --admin-user root \
  --admin-password-env MYSQL_ADMIN_PASSWORD
```

管理账号和环境变量引用不会写入 `unredo.yaml`。`init` 不会创建带默认密码的用户、修改 MySQL 全局配置或重启数据库。

向导会：

- 检查 MySQL 版本、ROW/FULL/GTID 和 binlog 保留状态。
- 为 profile 生成并持久化随机 replication server ID。
- 生成 reader/executor 最小权限 GRANT SQL，账号密码由 DBA 或密钥系统创建。
- 生成不含明文密码的 `unredo.yaml`。
- 只有显式 `--apply-meta` 时才初始化 `unredo_meta`。
- 给出尚需 DBA 手工完成的步骤。

`scripts/setup_test_users.sql` 和 `scripts/init_m0_schema.sql` 会删除并重建测试库，只适用于一次性本地测试环境，不能在生产执行。

### 3. 验证环境

```bash
unredo doctor --profile local
```

只有 doctor 通过后才应生成可执行计划。

### 4. 查找并预览事务

```bash
unredo txn list \
  --profile local \
  --binlog mysql-bin.000123 \
  --from-pos 4

unredo txn show \
  --profile local \
  --binlog mysql-bin.000123 \
  --txn "server-uuid:981"
```

首版的 `--binlog` 是 MySQL 服务器上的逻辑 binlog 文件名，通过 replication stream 读取，不是本地文件路径。

无主键表和非 InnoDB 表仍可 `list/show`，但不会生成可执行计划。

### 5. 创建、检查并执行计划

```bash
unredo plan create \
  --profile local \
  --binlog mysql-bin.000123 \
  --txn "server-uuid:981" \
  --output undo-981.json

unredo plan check --profile local undo-981.json
unredo plan apply --profile local undo-981.json
```

计划包含执行所需的行 image、唯一键和 schema 指纹。计划生成后，即使源 binlog 已被 purge，仍可 check/apply；当前数据或 schema 已变化时会报告冲突。

## 冲突与紧急处理

普通计划默认遇到任意冲突就停止。Unredo 不提供对整份计划无差别生效的 `--force`。

冲突必须逐项选择 `skip`、`overwrite` 或 `abort`，不能通过 `--force` 绕过。交互模式：

```bash
unredo plan resolve undo-981.json \
  --operator alice \
  --reason INC-2026-1042 \
  --output undo-981-resolved.json
```

自动化可以提供逐项 JSON，不能用一个全局 overwrite：

```json
{
  "operator": "alice",
  "reason": "INC-2026-1042",
  "resolutions": [
    {"operation_sequence": 1, "decision": "overwrite"},
    {"operation_sequence": 2, "decision": "skip"}
  ]
}
```

```bash
unredo plan resolve undo-981.json \
  --from-json resolutions.json \
  --output undo-981-resolved.json
```

resolved plan 会标记为 `unsafe_resolved`，记录父计划、逐项 conflict digest、操作者和原因。overwrite 会把生成时观察到的当前行固化为新 expect；如果数据再次变化，check/apply 会重新冲突。执行必须同时确认普通 digest 和风险 digest：

```bash
unredo plan apply undo-981-resolved.json \
  --non-interactive \
  --confirm-sha 91c1d203 \
  --accept-risk 91c1d203 \
  --operator alice \
  --reason INC-2026-1042
```

## Reapply

Reapply 需要原始根计划，因为数据库 marker 只保存身份和摘要，不保存可能包含敏感数据的完整 row image：

```bash
unredo action show --action-id 01J...

unredo action reapply \
  --action-id 01J... \
  --root-plan undo-981.json \
  --output redo-981.json

unredo plan check redo-981.json
unredo plan apply redo-981.json
```

`action reapply` 只接受该根计划最新的成功 `REVERT/ORIGINAL_REVERTED` action，并校验 action、根摘要和链深度；它只生成计划，不修改数据库。当前 CLI 支持“根 revert → 首次 reapply”，不支持把 REAPPLY action 再次 reapply。每一步都必须显式创建、检查和执行，`max_action_depth` 为后续交替链设置硬上限。

## MVP 边界

- MySQL 8、InnoDB、ROW/FULL binlog、GTID。
- 支持 INSERT、UPDATE、DELETE。
- DDL、XA、跨实例事务和数据库外副作用不支持自动恢复。
- 首版只支持远程 replication stream；本地 binlog 文件是后续模式。
- 首版面向 DBA 和数据库开发者，不识别业务用户的“上一步”。

项目采用数据库无关 core + backend adapter 结构。未来增加 PostgreSQL 等后端时，CLI 和核心 revert/reapply 工作流保持不变。
