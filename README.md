# Unredo

Unredo 计划成为一个用 Go 编写的数据库事务补偿 CLI。首个后端面向 MySQL 8 ROW binlog：选择一笔已提交事务，生成自包含的补偿计划，检查当前数据冲突，并以一笔新的事务安全地 revert 或 reapply。

它的语义类似 `git revert`，不会倒放或修改 InnoDB redo log。

> 当前状态：**M0-M2 已完成,核心 revert 闭环跑通**(plan create / check / apply + 集成测试 + 重放保护)。M3 (reapply) 与发布待办。详细进度见 [docs/PROGRESS.md](./docs/PROGRESS.md),完整范围和安全模型见 [DESIGN.md](./DESIGN.md)。

## 计划中的五分钟上手流程

### 1. 检查 MySQL 前置条件

首版要求：

```ini
log_bin=ON
binlog_format=ROW
binlog_row_image=FULL
gtid_mode=ON
enforce_gtid_consistency=ON
```

部分配置需要 DBA 修改 MySQL 配置并重启。Unredo 不会静默修改或重启数据库。

### 2. 运行初始化向导

```bash
unredo init --profile local
```

向导将：

- 检查 MySQL 版本、ROW/FULL/GTID 和 binlog 保留状态。
- 为 profile 生成并持久化随机 replication server ID。
- 生成 reader/executor 最小权限 SQL。
- 生成不含明文密码的 `unredo.yaml`。
- 经确认后初始化 `unredo_meta`。
- 给出尚需 DBA 手工完成的步骤。

账号创建和 migration 默认需要确认；管理凭据不会保存到配置文件。

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

需要事故处置时，可以逐项选择 skip、overwrite 或 abort，并生成可审计的 resolved plan：

```bash
unredo plan resolve undo-981.json \
  --output undo-981-resolved.json
```

resolved plan 会标记为 unsafe，记录父计划、冲突摘要、操作者和原因，并要求单独的风险确认。它可能产生部分补偿或覆盖后续修改，应只在理解影响后使用。

## Reapply

Reapply 需要原始根计划，因为数据库 marker 只保存身份和摘要，不保存可能包含敏感数据的完整 row image：

```bash
unredo action reapply \
  --action-id 01J... \
  --root-plan undo-981.json \
  --output redo-981.json
```

Revert/reapply 不会自动循环。每一步都必须显式创建、检查和执行新计划。

## MVP 边界

- MySQL 8、InnoDB、ROW/FULL binlog、GTID。
- 支持 INSERT、UPDATE、DELETE。
- DDL、XA、跨实例事务和数据库外副作用不支持自动恢复。
- 首版只支持远程 replication stream；本地 binlog 文件是后续模式。
- 首版面向 DBA 和数据库开发者，不识别业务用户的“上一步”。

项目采用数据库无关 core + backend adapter 结构。未来增加 PostgreSQL 等后端时，CLI 和核心 revert/reapply 工作流保持不变。
