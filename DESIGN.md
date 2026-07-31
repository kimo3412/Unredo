# Unredo 设计文档

## 1. 项目定义

Unredo 是一个使用 Go 编写的 MySQL 命令行工具。它从 MySQL ROW 格式的 binlog 中读取已经提交的 DML 事务，生成可审查的补偿计划，并在显式确认后以新事务执行补偿操作。

Unredo 的语义更接近 `git revert`，而不是数据库时间倒流：它不会修改、截断或倒放原始日志，也不会回滚 MySQL 的全局状态，而是提交一笔方向相反的新事务。

首版目标用户是 DBA 和理解数据库事务的开发人员。首版不解决“撤销某个业务用户的上一步操作”，只处理可以用 GTID、binlog 文件位置或扫描结果明确定位的数据库事务。

示例：

```bash
# 查看指定 binlog 范围内的事务
unredo txn list --binlog mysql-bin.000123 --from-pos 4

# 查看一笔事务的完整行变化
unredo txn show --binlog mysql-bin.000123 --txn "7f3...:981"

# 生成补偿计划，只读，不修改数据库
unredo plan create --binlog mysql-bin.000123 --txn "7f3...:981" -o undo-981.json

# 检查当前数据库是否仍满足安全执行条件
unredo plan check undo-981.json

# 执行补偿事务
unredo plan apply undo-981.json

# 重新应用原事务的 after-image
unredo action reapply --action-id 01J... --root-plan undo-981.json -o redo-981.json
```

## 2. 产品形态

MVP 是一个本地运行、无常驻服务的 CLI：

- 输入来自远程 MySQL 的 binlog replication stream，后续可增加本地 binlog 文件。
- 事务扫描和计划生成是只读操作。
- 计划以 JSON 文件保存，便于审查、签名、归档和代码评审。
- 只有 `plan apply` 会修改目标数据库；`action reapply` 只生成计划。
- CLI 不提供 Web UI、审批流、用户系统或实时监控。
- CLI 不要求业务应用引入 SDK，也不承诺获得业务用户身份。

后续产品可以在相同核心库上增加 DBA Web 工具台，但不属于 MVP。

## 3. MVP 范围

### 3.1 支持范围

- MySQL 8.0。
- InnoDB。
- `binlog_format=ROW`。
- 推荐并默认要求 `binlog_row_image=FULL`。
- 要求 `binlog_row_metadata=FULL`，用于核对历史事件列名并拒绝无法证明 schema 的旧事件。
- 有主键或非空唯一键的表。
- 已提交的单实例事务。
- `INSERT`、`UPDATE`、`DELETE` 行事件。
- GTID 事务；同时保留 binlog filename/position 作为物理定位信息。
- 单笔事务的预览、生成计划、冲突检查、执行、审计和重新应用。

### 3.2 暂不支持

- InnoDB redo log 的直接解析或修改。
- Statement/Mixed binlog 的语句级逆向。
- DDL，包括 `CREATE`、`ALTER`、`DROP`、`TRUNCATE`。
- 无稳定唯一键表的自动执行；`txn list/show` 和脱敏预览仍然支持，`plan create` 只能生成 `PREVIEW_ONLY` 计划。
- 跨 MySQL 实例的分布式事务。
- XA 事务。
- 非 InnoDB 表的自动执行；事务扫描和只读预览仍可使用。
- 触发器、存储过程和 UDF 产生的外部副作用恢复。
- 文件、缓存、消息、HTTP、支付和邮件等数据库外副作用。
- 自动识别业务用户、request ID 或业务操作名称。
- 对整份计划无差别覆盖的 `--force`；冲突只能通过生成一份显式、逐项、可审计的 resolved plan 处理。
- 无限历史；能否恢复取决于 binlog 保留时间和计划文件是否存在。

### 3.3 多数据库扩展约束

MySQL 是第一个后端，不是核心模型本身。MVP 的交付范围仍然只有 MySQL 8，但代码从第一天起必须遵守以下边界：

- 核心层不得出现 GTID、binlog、WAL、LSN、MySQL column type 等后端概念。
- CLI 不直接调用 MySQL 包，只调用 application service。
- 日志读取、schema 获取、类型解码、冲突检查、SQL 执行和审计存储都通过后端接口提供。
- 计划文件使用通用事务引用，并允许后端保存不透明 cursor 和扩展字段。
- 核心规划器只接收标准化的 before/after row change。
- 后端必须声明能力；核心不得根据数据库名称猜测能力。
- 不支持的能力必须 fail closed，不能用字符串转换或近似语义降级执行。
- 新增数据库后端不应修改既有 CLI 命令和核心 revert/reapply 算法。

预计可复用的部分：CLI 工作流、事务和行变化模型、计划格式、revert/reapply 规划、冲突结果、安全策略和审计关系。

必须由后端实现的部分：日志协议、事务定位、数据类型、schema 读取、锁与条件写入、marker DDL、权限检查和 commit 结果确认。

## 4. 安全原则

1. **默认只读**：扫描、展示、生成和检查计划不能写目标数据库。
2. **计划与执行分离**：执行只接受已经落盘的计划文件。
3. **乐观冲突检测**：当前行必须仍匹配原事务的 after-image，才允许按安全模式恢复到 before-image。
4. **整笔事务原子执行**：任意一行检查失败，默认整笔补偿事务回滚。
5. **不猜测旧值**：缺少完整 before/after image 时拒绝生成可执行计划。
6. **不依赖不稳定行位置**：自动执行必须具有主键或可靠唯一键。
7. **保留证据**：计划文件包含原始事务定位、schema 指纹和行变化摘要。
8. **不隐藏补偿事务**：Unredo 自己产生的事务继续进入 binlog，并通过 marker 明确标记。
9. **最小权限**：扫描账号和执行账号可以分开配置。
10. **失败可诊断**：错误必须指出事务、表、键、预期值和实际冲突类型，但敏感值默认脱敏。
11. **危险操作可追溯**：生产事故中的逃生通道必须生成新的 resolved plan，记录逐项决策、操作者和原因，不能临时关闭全部检查。

## 5. 基本语义

### 5.1 反向操作

| 原始事件 | Revert 操作 | 安全前置条件 |
| --- | --- | --- |
| INSERT(after) | DELETE | 当前行完整匹配 after |
| DELETE(before) | INSERT | 唯一键当前不存在 |
| UPDATE(before, after) | UPDATE 为 before | 当前行完整匹配 after |

补偿事件必须按原事务事件的逆序执行。例如原事务先插入父表、后插入子表，恢复时先删除子表、后删除父表。

### 5.2 Reapply

Reapply 不是简单“撤销补偿事务”，而是重新应用原事务的目标状态：

| 原始事件 | Reapply 操作 | 安全前置条件 |
| --- | --- | --- |
| INSERT(after) | INSERT after | 唯一键当前不存在 |
| DELETE(before) | DELETE | 当前行完整匹配 before |
| UPDATE(before, after) | UPDATE 为 after | 当前行完整匹配 before |

每次 reapply 都重新读取当前数据并执行冲突检查，不能假设数据库仍处于上次 revert 后的状态。

### 5.3 冲突定义

以下任一情况都视为冲突：

- 目标行不存在，但计划期望它存在。
- 目标唯一键已经存在，但计划期望它不存在。
- 当前字段值与计划中的预期 image 不一致。
- 表结构与计划生成时的 schema 指纹不一致。
- 唯一键定义发生变化。
- 目标表不再是 InnoDB。
- 执行期间锁等待超时或发生死锁。

首版的普通计划遇到冲突时拒绝整笔执行，不提供对整份计划生效的 `--force`。生产事故中的逃生通道是 `plan resolve`：用户必须对每个冲突明确选择 `skip`、`overwrite` 或 `abort`，并生成一份新的 resolved plan。

- `skip`：不执行该 operation；结果是部分补偿，不再保证恢复原事务的整体业务语义。
- `overwrite`：接受当前状态已经偏离 expect，仍写入计划目标值；可能覆盖后续合法修改。
- `abort`：保留冲突，不允许执行。

resolved plan 必须引用父计划 digest，记录每个 operation 的决定、操作原因和操作者，并标记 `execution_class=unsafe_resolved`。执行时仍保留 schema、实例、唯一键、事务原子性和 affected rows 检查；只有被逐项覆盖的值比较可以放宽。它不能绕过 plan digest、目标实例校验或执行审计。

非交互执行 resolved plan 时必须提供其短 digest 和单独的 `--accept-risk`，不能复用普通计划的确认参数。

### 5.4 Revert/Reapply 链的终止与分支

Unredo 不会自动循环 revert/reapply，也不设置“按到栈底”的隐含行为。每次动作都是用户显式创建、检查和执行的一份新计划。

对同一个根事务，成功动作的目标状态只有两种：

- `ORIGINAL_APPLIED`：原事务的 after-image 是最近一次由 Unredo 建立的目标状态。
- `ORIGINAL_REVERTED`：原事务的 before-image 是最近一次由 Unredo 建立的目标状态。

当当前状态为 `ORIGINAL_APPLIED` 时只允许创建 revert；为 `ORIGINAL_REVERTED` 时只允许创建 reapply。存在未决计划、当前数据已偏离最近状态或 action 结果未知时，禁止继续链式创建，直到用户处理冲突或核验提交结果。

理论上用户可以多次显式交替操作，但系统不后台执行、不自动生成下一步，也不把历史 action 当作可无限 pop 的栈。默认策略可以通过 `max_action_depth` 限制单个根事务的成功动作深度；超过限制需要显式提高 profile 策略并记录原因。失败、取消和仅生成未执行的计划可以形成审计分支，但成功状态链必须保持单线性。

## 6. CLI 设计

CLI 使用“名词 + 动作”结构，所有写操作都要求明确子命令。

### 6.1 全局参数

```text
--config PATH          配置文件，默认查找 ./unredo.yaml 和用户配置目录
--profile NAME         连接配置名称
--format table|json    输出格式
--log-level LEVEL      error|warn|info|debug
--no-color             禁用彩色输出
--timeout DURATION     命令总超时
```

密码不允许直接写在命令行参数中，避免出现在 shell history 和进程列表。支持环境变量、交互输入或系统密钥存储。

### 6.2 环境检查

新用户首先运行初始化向导：

```bash
unredo init --profile local
```

`init` 的职责：

1. 交互式收集 MySQL 地址和 profile 名称。
2. 使用临时检查凭据读取 MySQL 版本及 binlog/GTID 配置。
3. 为该 profile 生成并持久化随机、非零 replication `server_id`，不把示例固定值复制到所有环境。
4. 生成最小权限 reader/executor 建议 SQL；只有用户显式选择并提供管理凭据时才执行账号创建。
5. 生成 `unredo.yaml`，密码只保存为环境变量或密钥引用。
6. 在明确确认后执行 `migrations/mysql/001_init.sql`。
7. 运行与 `doctor` 相同的最终检查并输出尚需人工完成的步骤，例如修改 MySQL 配置和重启。

`init` 不能静默修改 MySQL 全局配置、重启数据库或把管理凭据写入配置文件。脚本环境后续提供等价的非交互参数。

环境检查命令：

```bash
unredo doctor
```

检查内容：

- MySQL 版本。
- binlog 是否启用。
- binlog format 和 row image。
- binlog row metadata 是否为 FULL。
- GTID 状态。
- binlog 保留范围。
- Unredo replication `server_id` 是否为合法非零值。
- 通过 `SHOW REPLICAS`/`SHOW SLAVE HOSTS` 等可用信息尽力检查 `server_id` 冲突。
- 建立短生命周期 replication stream 的连通性探测，并报告 duplicate server ID 错误。
- 扫描账号权限。
- 执行账号权限。
- `unredo_meta` 是否已初始化。

MySQL 不保证向客户端完整暴露所有已连接 binlog dump client 的 server ID，因此 `doctor` 只能做尽力检查，不能证明全局唯一。`init` 使用随机持久化值降低碰撞概率；生产环境仍应由 DBA 将该 ID 纳入实例的 server ID 分配记录。

### 6.3 扫描事务

```bash
unredo txn list \
  --binlog mysql-bin.000123 \
  --from-pos 4 \
  --to-pos 982311 \
  --database shop \
  --table orders \
  --limit 100
```

输出示例：

```text
GTID          COMMIT_TIME           ROWS  TABLES                    REVERSIBLE
uuid:981      2026-07-30T19:22:10Z  3     shop.orders               yes
uuid:982      2026-07-30T19:22:13Z  8     shop.orders,shop.payments no:DDL
```

`txn list` 是流式扫描，不承诺对所有历史 GTID 随机访问。用户必须给出起始 binlog，或使用配置中的默认扫描起点。未来可增加本地索引，但 MVP 不引入常驻索引服务。

MVP 只支持 `source.mode=replication`。这里的 `--binlog mysql-bin.000123` 是目标 MySQL 服务器上的 binlog 逻辑文件名，由 replication protocol 请求，不是本机文件路径。MVP 不接受 `C:\\...`、`/var/lib/mysql/...` 等路径。

未来本地文件模式使用独立配置 `source.mode=local-file` 和明确参数 `--binlog-path`，不会复用 `--binlog`。能力差异如下：

| 能力 | replication | local-file（未来） |
| --- | --- | --- |
| 从在线实例读取事件 | 支持 | 不支持 |
| 自动核对 instance ID | 支持 | 依赖旁路元数据 |
| 读取当前 schema | 支持 | 需要另配 target 连接 |
| `plan check/apply` | 支持 | 仍必须配置 target |
| 离线取证 | 不适合 | 适合 |
| 检查日志是否被 purge | 服务端直接报告 | 由文件存在性决定 |

### 6.4 查看事务

```bash
unredo txn show \
  --binlog mysql-bin.000123 \
  --txn uuid:981
```

默认展示表、事件数、唯一键和脱敏后的字段差异。`--show-values` 显示完整值，使用时打印敏感信息警告。

### 6.5 创建计划

```bash
unredo plan create \
  --binlog mysql-bin.000123 \
  --txn uuid:981 \
  --mode revert \
  --output undo-981.json
```

计划生成阶段需要读取 `information_schema` 获取表结构和键定义，但不读取或修改业务行。

### 6.6 检查计划

```bash
unredo plan check undo-981.json
```

该命令连接目标库，读取当前 schema 和目标行，输出：

- `READY`：可以安全执行。
- `CONFLICT`：数据已变化。
- `STALE_SCHEMA`：表结构已变化。
- `UNSUPPORTED`：计划包含不支持的事件。
- `SOURCE_MISMATCH`：计划目标实例与当前实例不一致。

### 6.7 执行计划

```bash
unredo plan apply undo-981.json
```

交互式终端中显示摘要并要求输入计划中短 hash。CI 或脚本可以使用：

```bash
unredo plan apply undo-981.json \
  --non-interactive \
  --confirm-sha 53ab92c1
```

不设计泛化的 `--yes`，避免误执行错误文件。

### 6.8 查看与重新应用操作

```bash
unredo action show --action-id 01J...
unredo action reapply \
  --action-id 01J... \
  --root-plan undo-981.json \
  --output redo-981.json
unredo plan check redo-981.json
unredo plan apply redo-981.json
```

reapply 需要根 plan 中保存的完整 before/after image，marker 表只保存身份和 digest，不能替代计划文件。`action reapply` 会校验 root plan digest 与 action marker 是否一致，只生成新计划，不直接执行。如果根 plan 丢失且原 binlog 也已 purge，MVP 无法安全 reapply。

截至 2026-07-31，CLI 已实现根 revert 后的首次 reapply：还会校验所给 action 是该根摘要下最新的成功 `REVERT/ORIGINAL_REVERTED` action，生成带 `root_plan_digest`、`parent_action_id` 和 `chain_depth=1` 的新计划。apply 前 MySQL adapter 会再次核验链关系。当前尚未开放从 REAPPLY 生成 chained revert 的入口，因此实现层面不会形成无限交替；后续开放时仍受 §5.4 状态机和 `max_action_depth` 约束。

### 6.9 解决冲突

```bash
unredo plan resolve undo-981.json \
  --output undo-981-resolved.json
```

默认进入交互模式，逐项展示冲突及脱敏 diff。非交互模式必须提供结构化 resolution 文件，不能用一个全局 flag 将所有冲突批量 overwrite。

执行 resolved plan：

```bash
unredo plan apply undo-981-resolved.json \
  --non-interactive \
  --confirm-sha 91c1d203 \
  --accept-risk 91c1d203
```

## 7. 计划文件

计划使用版本化 JSON。它既是执行输入，也是不可变审计证据的一部分。

简化结构：

```json
{
  "format_version": 1,
  "plan_id": "01J4...",
  "mode": "revert",
  "execution_class": "safe",
  "created_at": "2026-07-30T20:00:00Z",
  "source": {
    "backend": "mysql",
    "instance_id": "mysql-server-uuid",
    "native_transaction_id": "...:981",
    "cursor": {
      "kind": "mysql-binlog",
      "data": {
        "file": "mysql-bin.000123",
        "start_pos": 44120,
        "end_pos": 44981
      }
    }
  },
  "schema_fingerprints": {
    "shop.orders": "sha256:..."
  },
  "operations": [
    {
      "operation_id": "op-3",
      "sequence": 3,
      "database": "shop",
      "table": "orders",
      "type": "update",
      "key": {"id": 123},
      "expect": {"id": 123, "status": "paid"},
      "write": {"id": 123, "status": "pending"}
    }
  ],
  "digest": "sha256:..."
}
```

实现要求：

- digest 基于规范化 JSON 计算，不包含 digest 字段本身。
- `source.backend` 决定使用哪个后端验证和执行计划。
- `native_transaction_id` 只用于展示和审计，核心层不解析其内部格式。
- `cursor` 对核心层是不透明数据，只能交还给相同 backend；MySQL 使用 binlog position，PostgreSQL 后端可以使用 LSN。
- `backend_extensions` 可以保存版本化的后端专用信息，但不得改变通用 operation 的语义。
- 字段值需要无损表达 MySQL 类型，不能全部转成 JSON number。
- `DECIMAL`、`BIGINT`、时间、位字段和二进制值使用带类型编码。
- 计划文件权限默认限制为当前用户，因为它可能包含敏感数据。
- 日志输出默认只显示键和变更字段摘要，不输出完整计划内容。

计划是自包含执行工件：生成后所需的 before/after image、唯一键、schema 指纹和目标实例身份都保存在计划内。`plan check/apply` 不需要重新读取源 binlog；binlog filename、position、GTID/LSN 等 cursor 只用于来源证明和审计。

因此源 binlog 在 plan create 后被 purge，不影响该 plan 的 check/apply。仍可能阻止执行的因素是计划损坏、目标实例不匹配、schema 变化、当前数据冲突或计划格式已不受当前工具版本支持。工具必须提供向后兼容的 plan reader 或显式离线迁移命令，不能静默重新解释旧计划。

resolved plan 在通用字段之外还包含：

```json
{
  "execution_class": "unsafe_resolved",
  "parent_plan_digest": "sha256:...",
  "resolution_reason": "incident INC-2026-1042",
  "resolutions": [
    {
      "operation_id": "op-17",
      "decision": "skip",
      "conflict_digest": "sha256:..."
    }
  ]
}
```

resolution 必须绑定生成时观察到的 conflict digest；如果 apply 时当前值再次变化，该 resolution 失效并重新报冲突。

## 8. 审计与补偿事务标记

Unredo 不维护线性 undo stack，而维护不可变操作账本。核心关系为：

```text
原始事务 T100
  └─ Revert action A1
       └─ 补偿事务 T150
            └─ Reapply action A2
                 └─ 重新应用事务 T190
```

在目标实例中创建独立数据库：

```sql
CREATE DATABASE IF NOT EXISTS unredo_meta;

CREATE TABLE unredo_meta.action_markers (
    action_id       BINARY(16)   NOT NULL,
    plan_id         BINARY(16)   NOT NULL,
    parent_action_id BINARY(16)  NULL,
    root_plan_digest BINARY(32)  NOT NULL,
    action_type     ENUM('REVERT', 'REAPPLY') NOT NULL,
    target_state    ENUM('ORIGINAL_APPLIED', 'ORIGINAL_REVERTED') NOT NULL,
    chain_depth     INT UNSIGNED NOT NULL,
    source_native_transaction_id VARCHAR(255) NOT NULL,
    plan_digest     BINARY(32)   NOT NULL,
    execution_class ENUM('SAFE', 'UNSAFE_RESOLVED') NOT NULL,
    reason          VARCHAR(1024) NULL,
    tool_version    VARCHAR(32)  NOT NULL,
    operator_name   VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (action_id),
    UNIQUE KEY uq_plan_id (plan_id),
    KEY idx_parent_action (parent_action_id),
    UNIQUE KEY uq_root_depth (root_plan_digest, chain_depth)
) ENGINE=InnoDB;
```

以上是 MySQL adapter 的 migration，不属于 core schema。apply 在加锁后检查同一根计划的最新成功 action 与 `parent_action_id` 一致，再写入下一节点，从而阻止两个并发计划同时形成成功分支。`max_action_depth` 在写 marker 前检查。

执行补偿时，marker 与业务数据修改处于同一个 MySQL 事务：

```sql
START TRANSACTION;
INSERT INTO unredo_meta.action_markers (...);
-- 带前置条件的数据补偿操作
COMMIT;
```

这样可以保证：

- marker 和补偿修改一起提交或回滚。
- binlog 解析器可以识别 Unredo 自身事务。
- 工具崩溃后仍可从数据库和 binlog 重建关系。
- 补偿事务不会被误展示为普通业务事务。

MVP 只支持 marker 表和目标表位于同一 MySQL 实例的执行模式。

## 9. 执行算法

### 9.1 Plan Create

1. 连接 MySQL，校验 server UUID 和 binlog 配置。
2. 从用户指定的 binlog 文件和位置启动复制流。
3. 读取 `FORMAT_DESCRIPTION_EVENT` 和 `TABLE_MAP_EVENT`。
4. 以 GTID/XID 聚合行事件，直到事务 commit。
5. 若事务包含 DDL、缺失行 image 或无法确定唯一键，标记不可执行。
6. 读取当前表结构，生成 schema 指纹。
7. 将事件逆序转换为补偿 operation。
8. 规范化序列化、计算 digest，以受限权限写入计划文件。

### 9.2 Plan Check

1. 校验计划版本、digest 和目标 server UUID。
2. 读取并比较 schema 指纹。
3. 按唯一键读取目标行。
4. 使用 MySQL 类型语义比较当前值和 expect image。
5. 汇总全部冲突，不修改数据。
6. 输出机器可读结果，并使用非零退出码表示不可执行。

### 9.3 Plan Apply

1. 重复执行全部 check，不能复用较早的检查结论。
2. 开启事务，设置合理的锁等待超时。
3. 写入 action marker。
4. 按计划顺序对目标行加锁并再次校验。
5. 使用带前置条件的 SQL 执行补偿。
6. 检查每条语句 affected rows 是否符合预期。
7. 全部成功后 commit；任意失败则 rollback。
8. 返回 action ID；补偿事务 GTID 必须通过 marker 与 binlog 精确关联，不能从全局 GTID 尾部猜测。关联功能未实现时该字段留空。

执行时不能使用简单的“先 SELECT、后无条件 UPDATE”。校验与写入之间必须处于同一事务，并通过锁或条件 SQL 防止 TOCTOU 竞争。

## 10. Go 工程结构

建议使用 Go 1.24 或项目初始化时可获得的最新稳定版本，并在 `go.mod` 固定版本。

```text
Unredo/
├─ cmd/
│  └─ unredo/
│     └─ main.go
├─ internal/
│  ├─ cli/             Cobra 命令、输出和退出码
│  ├─ config/          配置、profile、backend 和凭据引用
│  ├─ app/             list/create/check/apply 等用例编排
│  ├─ core/            数据库无关的事务、变化、计划和冲突模型
│  ├─ ports/           source/schema/executor/action store 接口
│  ├─ planner/         数据库无关的 revert/reapply 规划
│  ├─ registry/        编译期后端注册与 profile 解析
│  ├─ backends/
│  │  └─ mysql/
│  │     ├─ source/    binlog replication 与事件解码
│  │     ├─ schema/    MySQL schema、唯一键和指纹
│  │     ├─ types/     MySQL 类型与通用 Value 转换
│  │     ├─ executor/  MySQL 锁、条件写入、marker 和提交
│  │     ├─ audit/     MySQL action 查询与关联
│  │     └─ doctor/    MySQL 环境、配置和权限检查
│  ├─ redact/          日志脱敏
│  └─ testutil/        后端契约和集成测试辅助
├─ migrations/
│  └─ mysql/
│     └─ 001_init.sql
├─ testdata/
│  ├─ binlog/
│  └─ plans/
├─ docs/
├─ DESIGN.md
├─ go.mod
├─ go.sum
└─ README.md
```

依赖方向必须保持单向：

```text
cli -> app -> core/ports
                  ↑
             backend adapters
```

`core` 不能导入 `backends/*`；`app` 通过 ports 工作；`registry` 在程序启动时组装具体后端。

### 10.1 核心模型与后端接口

数据库日志必须先转换为通用行变化，才能进入规划器：

```go
type TransactionRef struct {
	Backend             string
	InstanceID          string
	NativeTransactionID string
	Cursor              json.RawMessage
}

type RowChange struct {
	Sequence  int
	Table     TableRef
	Operation Operation
	Key       Row
	Before    Row
	After     Row
}
```

核心 ports 至少包括：

```go
type ChangeSource interface {
	Scan(context.Context, ScanScope) (TransactionIterator, error)
	Find(context.Context, TransactionRef) (*Transaction, error)
	Capabilities(context.Context) (Capabilities, error)
}

type SchemaInspector interface {
	InspectTable(context.Context, TableRef) (TableSchema, error)
	Fingerprint(context.Context, TableRef) (SchemaFingerprint, error)
}

type PlanExecutor interface {
	Check(context.Context, Plan) ([]Conflict, error)
	Apply(context.Context, Plan, Action) (ExecutionResult, error)
}

type ActionStore interface {
	Find(context.Context, ActionID) (*Action, error)
}
```

接口传递 core 类型，不能泄露某个驱动或日志解析库的类型。后端错误需要转换为稳定的领域错误，例如 `ErrTransactionNotFound`、`ErrUnsupportedCapability` 和 `ErrCommitUnknown`。

### 10.2 能力协商

不同数据库以及同一数据库的不同配置提供的日志信息并不相同。后端初始化后必须报告能力：

```go
type Capabilities struct {
	FullBeforeImage       bool
	FullAfterImage        bool
	StableTransactionID   bool
	TransactionBoundaries bool
	AtomicActionMarker    bool
	SchemaAtEventTime     bool
	SupportsReapply       bool
}
```

规划器根据能力决定事务是 `EXECUTABLE`、`PREVIEW_ONLY` 还是 `UNSUPPORTED`。例如未来 PostgreSQL logical decoding 后端缺少完整 old tuple 时，可以展示变化，但不能生成可执行 revert 计划。

### 10.3 后端注册

首版采用编译时注册，不使用 Go `plugin`：

```go
registry.Register("mysql", mysql.NewBackend)
// 后续：registry.Register("postgres", postgres.NewBackend)
```

Go plugin 对平台、构建版本和依赖版本有额外限制，目前没有必要承担这类复杂度。新增后端以独立 package 和工厂注册完成。

### 10.4 建议依赖

- CLI：`github.com/spf13/cobra`。
- MySQL 驱动：`github.com/go-sql-driver/mysql`。
- binlog replication：优先评估 `github.com/go-mysql-org/go-mysql`，但在采用前用测试样本验证 MySQL 8 类型覆盖和 GTID 行为。
- 配置：标准库加轻量 YAML 库；避免引入庞大配置框架。
- 日志：Go `log/slog`。
- ID：ULID 或 UUIDv7，最终选择一种并固定编码。

binlog 解析是项目核心风险。第三方库只能作为协议和事件解码层，事务语义、类型保真、计划安全和测试必须由 Unredo 自己负责。

## 11. 配置示例

```yaml
version: 1

profiles:
  local:
    backend: mysql
    source:
      mode: replication
      address: 127.0.0.1:3306
      user: unredo_reader
      password_env: UNREDO_READER_PASSWORD
      server_id: auto
    target:
      address: 127.0.0.1:3306
      user: unredo_executor
      password_env: UNREDO_EXECUTOR_PASSWORD
    policy:
      require_gtid: true
      require_full_row_image: true
      require_primary_key: true
      max_transaction_rows: 1000
      max_transaction_bytes: 64MiB
      max_plan_bytes: 128MiB
      max_action_depth: 20
      lock_wait_timeout: 5s
```

`server_id: auto` 由 `unredo init` 解析为 profile 专属的随机非零 uint32 并持久化；运行时不能每次随机变化。

大事务限制是 M0 前的保守候选值，不是已经验证的性能承诺。最终默认值必须通过真实 MySQL fixture 测量峰值内存、解码后大小、JSON/base64 膨胀、check 查询成本和 apply 锁持有时间后确定。限制同时按行数和字节数生效，任一超限即停止保留完整 row image，事务标记为 `TOO_LARGE`，仍可显示元数据摘要但不能生成可执行计划。用户可在 profile 中显式提高限制。

CLI 根据 profile 的 `backend` 从 registry 构建完整后端。未来增加 PostgreSQL profile 时沿用相同命令，不新增 `postgres-txn` 一类数据库专用 CLI：

```yaml
profiles:
  future-postgres:
    backend: postgres
    source:
      address: 127.0.0.1:5432
```

读取账号建议权限：

- `REPLICATION SLAVE` 或目标 MySQL 版本对应的复制读取权限。
- `REPLICATION CLIENT`。
- 对 `information_schema` 所需元数据的访问权限。

执行账号只应获得允许恢复的 schema 上的 `SELECT/INSERT/UPDATE/DELETE`，以及 `unredo_meta.action_markers` 的必要权限。具体权限必须通过 `doctor` 实测，不在代码中假设超级用户。

## 12. 类型处理

类型保真决定恢复是否可信。MVP 至少覆盖：

- 有符号和无符号整数。
- `DECIMAL`，使用字符串或定点表示，禁止 float round-trip。
- `FLOAT/DOUBLE`，比较策略必须考虑二进制值，不使用格式化文本比较。
- `CHAR/VARCHAR/TEXT` 及字符集。
- `BINARY/VARBINARY/BLOB`。
- `DATE/TIME/DATETIME/TIMESTAMP`，保留小数秒和时区语义。
- `ENUM/SET`。
- `BIT`。
- `JSON`，首版可以按 MySQL 返回的规范值比较，但需要专门测试键序和数值表达。
- `NULL` 与空字符串的严格区分。

不支持的类型必须使计划不可执行，而不是降级成字符串猜测。

核心层使用带类型的通用 `Value`，而不是直接保存 MySQL driver value：

```go
type Value struct {
	Kind     ValueKind
	Null     bool
	Encoding string
	Data     json.RawMessage
	Native   *NativeType
}
```

`Kind` 表示 integer、decimal、text、binary、datetime、json 等通用类别；`Native` 保存 `mysql:decimal(20,4)`、未来的 `postgres:numeric(20,4)` 等精确信息。通用层负责稳定序列化，具体后端负责无损解码、值比较和 SQL 参数绑定。

## 13. 表结构指纹

schema 指纹至少包含：

- database/table 名称。
- 列顺序、名称和 MySQL 完整类型。
- nullable、字符集和 collation。
- 主键和唯一键列顺序。
- 生成列信息。
- 表引擎。

计划生成后如果 schema 指纹变化，MVP 一律拒绝执行，即使变化看起来与目标列无关。后续版本可以引入兼容性分析。

## 14. 测试策略

### 14.1 单元测试

- 每种 row event 的正向和反向映射。
- 多表、多行事件逆序。
- MySQL 类型编码、解码和比较。
- 计划规范化及 digest 稳定性。
- 源 binlog 已 purge 时仍能读取、检查和执行自包含计划。
- schema 指纹稳定性。
- 脱敏和错误信息不泄露完整字段值。

### 14.2 集成测试

使用真实 MySQL 容器，不用 mock 替代 binlog：

1. 初始化 ROW/FULL/GTID MySQL。
2. 执行 fixture 事务。
3. 从真实 binlog 生成计划。
4. 执行 check 和 apply。
5. 验证数据恢复、marker 和补偿 binlog。
6. 执行 reapply 并验证最终状态。

覆盖场景：

- 单行及多行 INSERT/UPDATE/DELETE。
- 单事务多表混合变更。
- 外键依赖。
- NULL、二进制、Unicode、decimal 和时间字段。
- 生成计划后发生并发修改。
- schema 变化。
- 重复 apply 同一 plan。
- 无主键表和非 InnoDB 表可以 list/show，但不能生成可执行计划。
- resolved plan 的 skip/overwrite、二次冲突和风险确认。
- revert/reapply 成功链保持单线性，未决或未知 action 阻止继续。
- 执行中死锁、超时和进程中断。
- binlog rotation。
- plan create 后源 binlog purge。
- replication server ID 冲突和连通性探测。
- 大事务分别触发行数、解码字节和计划字节限制。
- 不完整事务和损坏输入。
- 触发器产生的附加 row events。

### 14.3 端到端安全断言

- 同一个 plan 不得成功 apply 两次。
- 任意冲突不得产生部分恢复。
- marker 不得在业务修改失败时单独提交。
- 日志默认不得出现密码和完整敏感 row image。
- Ctrl+C 时必须回滚未提交事务。

### 14.4 后端契约测试

所有后端必须通过同一套契约测试：

- 能报告真实 capabilities。
- native transaction 能无损转换为 core transaction。
- scan/find 返回稳定事务边界和顺序。
- schema fingerprint 对相同结构稳定、对相关结构变化敏感。
- check 不写数据。
- apply 原子执行并防止同一 plan 重复提交。
- commit 结果不确定时可以通过 action marker 恢复判断。
- 不支持的类型和能力统一 fail closed。

MySQL 是第一套契约实现；未来后端必须新增自己的真实数据库集成测试，不能只复用 mock。

## 15. 退出码

稳定退出码方便脚本和未来 UI 调用：

```text
0   成功
2   CLI 参数或配置错误
3   环境不受支持
4   事务未找到
5   事务不可逆
6   计划损坏或 digest 不匹配
7   数据冲突
8   schema 冲突
9   权限不足
10  执行失败且已回滚
11  执行结果不确定，需要人工核验
```

“结果不确定”用于网络在 commit 附近中断的情况。此时不能盲目重试，必须通过 plan ID/action marker 查询实际提交状态。

## 16. 里程碑

### M0：技术验证

- 初始化 Go 工程和 Cobra CLI。
- 提供 `unredo init` 配置向导骨架和五分钟快速上手文档。
- Docker 启动 MySQL 8 ROW/FULL/GTID 测试环境。
- 读取 binlog 并打印事务边界。
- 解码常用 MySQL 类型的行变化。
- 明确第三方 binlog 库的兼容边界。
- 使用不同字段宽度和 BLOB fixture 基准测试大事务限制，确定正式默认值。

完成标准：给定 fixture GTID，能稳定输出与 SQL 结果一致的 before/after image。

### M1：只读计划

- `doctor`。
- `txn list/show`。
- schema 读取和唯一键选择。
- `plan create`。
- JSON 规范化、digest 和脱敏输出。

完成标准：支持范围内的事务都能生成确定性计划，但工具没有执行写入能力。

### M2：安全执行

- `unredo_meta` migration。
- `plan check/apply`。
- 锁和条件写入。
- marker/action 查询。
- commit 不确定状态恢复。

完成标准：集成测试证明冲突时零部分写入，同一计划不能重复应用。

### M3：Reapply 与发布

- `action show/reapply`。（核心已实现）
- 安装包和跨平台构建。
- 完整权限文档和故障排查。
- 大事务保护与性能基准。

完成标准：revert/reapply 审计关系完整，并发布首个实验版本。

## 17. 主要风险

### 17.1 binlog 类型覆盖

MySQL 类型、版本和事件细节复杂，是最大技术风险。处理方式是先做 M0、维护真实 binlog fixture，并对不支持类型 fail closed。

### 17.2 binlog 不等于业务语义

Unredo 只能恢复行状态，不能判断业务操作是否应该撤销，也不能恢复外部副作用。CLI 必须持续使用“事务补偿”而不是“业务撤销”的措辞。

### 17.3 大事务

完整 row image 可能使计划文件和内存使用很大，而且 JSON 中的二进制 base64 会进一步膨胀。MVP 同时设置最大行数、解码字节和计划字节限制，超过限制只允许查看摘要，不生成可执行计划；默认值由 M0 基准测试确定。

### 17.4 Schema 演进

binlog TABLE_MAP 只提供有限的列信息。必须结合生成时的当前 schema；如果从很久以前的日志恢复，而表结构已经变化，首版拒绝执行。

### 17.5 Commit 结果不确定

客户端在 COMMIT 后、收到响应前断线时，不知道事务是否成功。通过 plan ID 唯一约束和同事务 marker 查询解决，禁止直接重试补偿 SQL。

## 18. MVP 验收标准

满足以下条件才可以称为可用 MVP：

- 在受支持配置的 MySQL 8 实例上通过 `doctor`。
- 能从指定 binlog 范围定位 GTID 事务。
- 对支持类型无损生成 before/after image。
- 对 INSERT/UPDATE/DELETE 生成确定性补偿计划。
- 数据未变化时能原子恢复整笔事务。
- 数据或 schema 已变化时拒绝执行并报告具体冲突。
- 每次写操作都有同事务 action marker。
- 同一计划不能被重复执行。
- 网络中断后可以判定补偿事务是否实际提交。
- 默认输出和日志不会泄露密码或完整敏感数据。
- 所有核心行为在真实 MySQL 集成测试中覆盖。

## 19. 后续方向

MVP 稳定后再评估：

- 本地事务索引，加速按时间、表和键检索。
- 守护进程和 DBA Web UI。
- RBAC、审批和计划签名。
- 本地 binlog 文件及对象存储归档读取。
- MariaDB 兼容层。
- PostgreSQL logical decoding 后端。
- 应用 SDK 和业务身份关联。
- 对部分 schema 变化的兼容性分析。

这些方向应复用 transaction、plan、checker 和 executor 的核心接口，但不能反向污染 CLI MVP 的安全边界。

增加新数据库的预期步骤为：实现 ports、注册 backend factory、提供 migration、通过后端契约测试并补充数据库专用文档。正常情况下不修改 CLI 命令、计划通用 operation 或核心 revert/reapply 规划器；如果必须修改，视为抽象边界失效，需要先做设计评审。
