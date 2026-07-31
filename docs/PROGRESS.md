# Unredo 进度报告

> 状态：M2 完成；M3 核心 reapply 与 plan resolve 闭环完成，发布工程待办。
> 最后更新:2026-07-31

> 2026-07-31 安全加固：apply 现在会针对真实 target 重新检查实例、schema 和 row image；DELETE 使用完整 expect 条件；修复 JSON 字符串、DECIMAL、时间、BLOB、BIT 和 NULL 写回；非交互 apply 强制要求 digest；commit 错误返回 `ErrCommitUnknown`；不再从 `@@global.gtid_executed` 猜测补偿 GTID。新增 UPDATE、DELETE 恢复、BLOB/NULL 和多行冲突零写入的真实 MySQL 集成测试。

> 2026-07-31 M3 核心：实现 `action show`、从根 revert plan 安全生成 `action reapply`、标准 ULID 二进制编码、root/parent/depth 单线链校验。真实 MySQL 已跑通 INSERT → revert → action show → reapply plan check/apply → 原始状态恢复。

> 2026-07-31 冲突逃生通道：实现交互式和 JSON 驱动的 `plan resolve`，支持逐 operation 的 skip/overwrite/abort、conflict digest、父计划审计和 `unsafe_resolved` 双重风险确认。真实 MySQL 已验证二次漂移重新冲突及 overwrite 恢复。

## 1. TL;DR

Unredo 计划成为 MySQL 事务补偿 CLI。本仓库目前已经实现 M0（技术验证）、M1（只读计划）、M2（安全执行），以及 M3 的核心 reapply 和冲突 resolve 流程。revert、首次 reapply、unsafe resolved overwrite 都已用真实 MySQL 端到端验证。

| 阶段 | 完成标准 | 状态 |
|---|---|---|
| M0 | 给定 fixture GTID,能输出与 SQL 结果一致的 before/after image | ✅ |
| M1 | 支持范围内的事务都能生成确定性 plan,但工具没有执行能力 | ✅ |
| M2 | 集成测试证明冲突时零部分写入,同一 plan 不能重复应用 | ✅ |
| M3 | reapply 审计关系完整,发布实验版本 | 核心完成，发布待办 |

## 2. 已经能跑的命令

```powershell
# 假设已 setx 设过密码环境变量
$env:UNREDO_READER_PASSWORD  = 'unredo_reader_pw'
$env:UNREDO_EXECUTOR_PASSWORD = 'unredo_executor_pw'

# 1. 环境检查
.\bin\unredo.exe --config unredo.yaml --profile local doctor

# 2. 列出 binlog 里的事务
.\bin\unredo.exe --config unredo.yaml --profile local txn list `
    --binlog binlog.000048 --from-pos 4 --limit 10 --max-time 5s

# 3. 看一笔事务的 row image
.\bin\unredo.exe --config unredo.yaml --profile local txn show `
    --binlog binlog.000048 --from-pos 4 `
    --txn '2385308c-...:77' --show-values

# 4. 生成 plan 文件
.\bin\unredo.exe --config unredo.yaml --profile local plan create `
    --binlog binlog.000048 --from-pos 4 `
    --txn '2385308c-...:77' --output plan.json

# 5. 验证 plan 还能不能用
.\bin\unredo.exe --config unredo.yaml --profile local plan check plan.json

# 6. 真正执行(写目标库 + 写 action marker)
.\bin\unredo.exe --config unredo.yaml --profile local plan apply plan.json `
    --non-interactive --confirm-sha 493c1e59 --operator alice

# 7. 查看成功 action
.\bin\unredo.exe --config unredo.yaml --profile local action show `
    --action-id 01K...

# 8. 从最新成功 revert 和根 plan 生成 reapply plan
.\bin\unredo.exe --config unredo.yaml --profile local action reapply `
    --action-id 01K... --root-plan plan.json --output reapply.json

# 9. reapply 仍需独立检查和确认执行
.\bin\unredo.exe --config unredo.yaml --profile local plan check reapply.json
.\bin\unredo.exe --config unredo.yaml --profile local plan apply reapply.json `
    --non-interactive --confirm-sha deadbeef --operator alice

# 10. 对冲突逐项生成 unsafe resolved plan
.\bin\unredo.exe --config unredo.yaml --profile local plan resolve plan.json `
    --from-json resolutions.json --output resolved.json

# 11. unsafe plan 必须双重确认同一份新计划的 digest
.\bin\unredo.exe --config unredo.yaml --profile local plan apply resolved.json `
    --non-interactive --confirm-sha deadbeef --accept-risk deadbeef `
    --operator alice --reason INC-2026-1042
```

`plan check` 状态:`READY` / `CONFLICT` / `STALE_SCHEMA` / `SOURCE_MISMATCH`。
`plan apply` 退出 0 即成功,会打印 `action_id` / `affected` 行数;重复 apply 同一份 plan 会被 `plan_id` UNIQUE 约束挡掉,报 `ErrApplyReplayed`。补偿 GTID 在 action/binlog 关联功能完成前留空，不能从全局 GTID 尾部猜测。

## 3. 架构现状

```
cmd/unredo/main.go                 # 入口,默认沉默 binlog 库 INFO
internal/
  cli/                             # Cobra 命令,子命令都注册在这里
    root.go                          # 全局 flags + 子命令注册
    doctor.go                        # 包装 backend.RunDoctor
    txn.go                           # txn list + txn show
    plan.go                          # plan create / check / apply / resolve
    action.go                        # action show / reapply
    init.go                          # M1 init 向导 stub
  config/                          # YAML profile + 密码环境变量
  core/                            # DB-agnostic 类型
    value.go                         # Value, RawJSON, ValueKind, NativeType
    transaction.go                   # TransactionRef, RowChange, Transaction,
                                     # BackendCapabilities, SchemaFingerprint
  ports/                           # 后端接口
    ports.go                         # ChangeSource, SchemaInspector,
                                     # PlanExecutor, ApplyRequest
  registry/                        # 编译期后端注册
  redact/                          # 日志脱敏
  executor/                        # 校验和执行(DB-agnostic)
    check.go                         # Status, SchemaCheck, Conflict, Check
    apply.go                         # ApplyOptions, ErrApplyConflict,
                                     # ErrApplyReplayed
  planner/                         # 计划生成 + 序列化(DB-agnostic)
    planner.go                       # Build (revert/reapply), unique-key
                                     # selection, canonical JSON, digest
    resolve.go                       # conflict digest + resolved plan 构造
    io.go                            # WriteFile / ReadFile / ShortDigest
  backends/mysql/                  # MySQL 8 ROW/FULL/GTID adapter
    backend.go                       # registry init、Check/Apply、action 链校验
    action_store.go                  # action marker 查询
    source/                          # binlog replication 读取
    schema/                          # information_schema 读取 + sha256 fp
    value/                           # 类型解码(integer/decimal/text/.../bit/enum)
    doctor/                          # 环境检查
    check.go                         # CheckReader for executor.Check
    apply.go                         # ApplyWriter (单事务 + marker)
    ulidbin.go                       # ULID <-> 16-byte
migrations/mysql/
  001_init.sql                     # unredo_meta.action_markers (DML)
scripts/                           # 手工 bootstrap
  setup_test_users.sql
  init_m0_schema.sql
  seed_fixtures.sql
  grant_reader_shop.sql
  grant_reader_meta.sql             # binlog 读 action_markers 的 SELECT
testdata/plans/                    # 文档化样例 plan
  m1-fixture.json
  m2-check.json
  m2-apply.json
  m2-apply4.json
tests/integration/                 # build tag: integration
  binlog_test.go                    # 真 MySQL 端到端
```

依赖方向(单向):
```
cli -> app/registry -> core/ports <- backends/mysql
                        ^              ^
                        |              |
                        planner     executor
```

## 4. 关键设计点(已落地)

### 4.1 类型保真

`value.go` 给出 `core.Value {Kind, Null, Encoding, Data, Native}`,不同类型用不同编码:
- `integer` / `float` / `decimal`:raw JSON number / 文本,避免 round-trip
- `decimal`:**始终字符串**,禁止 float round-trip
- `bit`:`big.Int` 字符串
- `binary`:`base64`
- `json`:`json.Marshal` 再序列化,规范化键序和数值表达
- `text` / `varchar`:UTF-8 字符串

MySQL 驱动给的 `[]byte` 在解码器里统一先 `asString`,再 `json.Marshal`,避免 base64 误读。

### 4.2 Plan 文件格式

`format_version=1`,关键字段:
- `plan_id` ULID(也用于 `action_markers.plan_id` BINARY(16))
- `mode` revert | reapply
- `execution_class` safe | unsafe_resolved
- `source.{backend, instance_id, native_transaction_id, cursor}`
- `schema_fingerprints` 每张表 sha256
- `operations[]` 每条含 `key` / `expect` / `write`
- reapply 子计划额外含 `root_plan_digest` / `parent_action_id` / `chain_depth`
- `digest` sha256 over canonical JSON,排除自身字段

`WriteFile` 用 canonical-JSON 重排键后再缩进美化,`ReadFile` 重算 digest 校验。`ShortDigest` 返回 hex 前 8 位,匹配 `--confirm-sha` 和 `--accept-risk`。

### 4.3 唯一键选择

`planner.selectUniqueKey`:优先 `PRIMARY KEY`(列都 NOT NULL 且都在 row image 里),否则第一个全部 NOT NULL 的 UNIQUE key,否则第一个出现在 image 里的 UNIQUE key。M2 检查和 apply 路径都走这个选择,确保 plan 和执行用的是同一把钥匙。

### 4.4 冲突检测

`executor.Check` 聚合所有冲突,返回:
- `READY`:schema fp 一致、每条 op 期望的行都满足
- `CONFLICT`:任一行不匹配、行缺失、行已存在、key 缺失
- `STALE_SCHEMA`:实际 fp 与 plan 不同
- `SOURCE_MISMATCH`:当前 instance UUID 与 plan 记的不同

`valuesEqual` 先 `json.Unmarshal` 两边再比,容忍 `16` vs `"16"`、数字 coercion。`StaleSchema` 与 `Conflict` 的优先级:StaleSchema 优先报。

### 4.5 安全执行

`ApplyWriter.Apply`:
1. 拿一条 `*sql.Conn`
2. `SET innodb_lock_wait_timeout = 5` + `START TRANSACTION`
3. **先写 action marker**(`UNIQUE(plan_id)` 挡重放)
4. 逐条 op:
   - `SELECT 1 FROM ... WHERE key ... FOR UPDATE` 锁
   - DELETE:`DELETE ... WHERE key` 必须 1 行
   - UPDATE:`SET write_cols WHERE key AND expect_image` 必须 1 行
   - INSERT:`INSERT ...` 写全行,UNIQUE 错 = 冲突
5. `COMMIT`
6. 返回 action ID；补偿 GTID 暂不猜测，后续由 marker/binlog 精确关联
7. 任意步骤出错 → `defer rollback()` 整笔回滚

MySQL 1062 → `ErrApplyReplayed`,0 affected → `ErrApplyConflict`,都带上下文信息。

### 4.6 时区陷阱

`MySQL` 服务端 `default-time-zone=+08:00`。**binlog 写的是 writer 会话时区下的 TIMESTAMP 文本**(设计如此,不能改),`SELECT` 默认也是该时区。**所以 `SELECT` 跟 binlog 必须共享同一个默认时区**,否则 `plan check` 一定会报 created_at 不一致。

当前 DSN 故意**不**设置 `time_zone='+00:00'`,让 read/write 走同一个 `+08:00`。代价:跨时区 instance 的 plan 不能直接用 — M2 trust model 是 writer/reader 同实例、同默认时区。

### 4.7 Reapply action 链

`action show` 从 `unredo_meta.action_markers` 读取成功提交的 action；时间通过 Unix 微秒读取并输出为 UTC，避免把 MySQL 会话时区误标成主机时区。

`action reapply` 要求同时提供 action ID 和原始根 revert plan。生成前会验证：

- 根 plan digest 自校验通过，且是 `safe/revert`；
- action 是该根摘要的最新成功 action；
- action 状态是 `REVERT/ORIGINAL_REVERTED`；
- action 的 plan/root digest 都与根 plan 一致；
- 下一层深度不超过 `max_action_depth`。

生成的 plan 反转根 revert operations 的执行顺序和方向；主键发生变化的 UPDATE 会用 revert 后的主键定位。apply 时 MySQL backend 再校验 parent/root/depth，`uq_root_depth` 阻止并发成功分叉。当前 CLI 只开放根 revert 后的首次 reapply；后续若增加 chained revert，仍复用相同的单线状态机和深度上限。

### 4.8 Conflict resolution

`plan resolve` 总是先对真实 target 重新执行 check，只接受 `CONFLICT`，不允许解决 `STALE_SCHEMA` 或 `SOURCE_MISMATCH`。每个冲突 operation 必须且只能给一个 skip/overwrite/abort 决策；结构化输入中的 conflict digest 可选，但输出计划一定记录按本次实际观察计算的 digest。

- skip：从新计划移除 operation；
- overwrite：把完整 current row 固化为 expect，再使用正常条件 DML；
- abort：不生成计划；
- row missing UPDATE 可转为 INSERT，row exists INSERT 可转为 UPDATE；
- resolved plan 使用新 plan ID/digest，记录 parent digest、resolver、reason 和逐项决策。

后端不提供“忽略 expect”的执行模式。resolved plan 仍走实例、schema、row check、锁、affected rows 和单事务 marker 路径，执行类别记录为 `UNSAFE_RESOLVED`。

## 5. 测试覆盖

### 5.1 单元测试

| 包 | 文件 | 覆盖 |
|---|---|---|
| `internal/core` | `value_test.go` | Value.Equal/Validate, OperationKind.Valid, Capabilities.All |
| `internal/executor` | `check_test.go` | READY / CONFLICT / row_missing / STALE_SCHEMA / SOURCE_MISMATCH / fp error |
| `internal/executor` | `apply_test.go` | ApplyOptions.Validate 7 个分支,ApplyRequest 类型守门 |
| `internal/planner` | `planner_test.go` / `resolve_test.go` | revert/reapply、PK-changing UPDATE、resolved skip/overwrite/转换、stale conflict digest |
| `internal/backends/mysql` | `check_test.go` / `ulidbin_test.go` | driver 解码、DSN 时区、标准 ULID 16-byte round-trip |

跑法:
```powershell
go test ./...
```

### 5.2 集成测试

`tests/integration/binlog_test.go` (build tag `integration`),需要真 MySQL。自动跳过当 MySQL 不可达。

```powershell
make test-integration
```

覆盖链路包括：

- INSERT → plan create/check/apply → 同 plan 重放被阻止；
- UPDATE、DELETE、BLOB、NULL 的真实写回；
- 多行事务中一行冲突时整笔零写入；
- INSERT → revert → action show → reapply create/check/apply → 原始行恢复，并核对 parent/root/depth/state；
- REAPPLY action 不能再次传给 `action reapply`。
- UPDATE conflict → resolved overwrite → 二次漂移重新 CONFLICT → 双重风险确认 apply → `UNSAFE_RESOLVED` marker。

### 5.3 测试基础设施约定

- `unredo_reader`:REPLICATION SLAVE/CLIENT,information_schema SELECT,unredo_shop SELECT,unredo_meta SELECT
- `unredo_executor`:unredo_shop 与 unredo_meta 的 DML
- 根密码走 root(只在 `unredo_reader` / `unredo_executor` 权限管理时用)

`scripts/grant_reader_meta.sql` 是后来加的,因为 marker 表的 row 事件会出现在 binlog 里,inspector 要能查它的列定义。

## 6. 已经踩过的坑(防再犯)

1. **binlog ENUM 是 int64 索引**,不是字符串。`isSystemTable` 过滤 `unredo_meta` 后整个问题消失。
2. **TIMESTAMP 时区漂移**:DSN 别加 `time_zone='+00:00'`。
3. **`[]byte` 走 `json.Marshal` 是 base64**:integer 解码要先 `parseIntegerLiteral` 再 `json.Marshal` 数字。
4. **`conn.ExecContext` 返回 2 个值**:`_, err := ...`,写 `err := ...` 编不过。
5. **不能从 `@@global.gtid_executed` 尾部推断自己的 GTID**：并发事务可能先提交；必须通过 action marker 与 binlog 精确关联。
7. **`ulid.Entropy()` 返回 `[]byte`**,不是 uint64。
8. **Cobra 子命令不要 Use 字段里带父前缀**,否则 help 里会出现重复项(我们用嵌套 `txn` / `plan` / `action` 修好)。
9. **plan 文件要写 canonical JSON**,否则 digest 不能重算。

## 7. 当前限制

### 7.1 M2 范围内未做的

- **`unredo_meta.action_markers` 不在 M0 自动 init**:要走 `scripts/init_m0_schema.sql` 或 `migrations/mysql/001_init.sql` 手工建。`unredo init` 命令也是 M1。
- **`init` 子命令是 stub**:`unredo init` 只打印提示。
- **交替 action 链尚未完整开放**：当前支持根 revert 后的首次 reapply；再次 chained revert 的 CLI 入口尚未实现。

### 7.2 安全模型边界

- 单 MySQL 实例,不支持跨实例事务
- 仅 InnoDB 表(其他表 list/show 正常,plan 报 `PREVIEW_ONLY`)
- 无主键 / 无可靠唯一键的表只能 list/show,plan 报 `PREVIEW_ONLY`
- DDL / DCL / XA / 触发器外部副作用 不在补偿范围
- plan 文件不加密,落盘 0600,需要 DBA / OS 层保护
- `plan apply` 不会重试:`commit unknown` 必须靠 `action_markers` 查实际状态
- 跨时区 plan:目前 reject,需要 M3+ 引入 plan-level 时区声明
- `max_transaction_rows` / `max_transaction_bytes` / `max_plan_bytes` 等大事务阈值在 `config.DefaultPolicy()` 给了占位值,**还没用真实 fixture 基准过** — DESIGN.md M0 完成标准之一是"等真实 MySQL fixture 测量峰值内存、解码后大小、JSON/base64 膨胀、check 查询成本和 apply 锁持有时间后确定"。

### 7.3 类型覆盖

M0+M1 覆盖:`bigint` / `int unsigned` / `decimal` / `varchar` / `text` / `timestamp(6)` / `date` / `tinyint` / `smallint` / `mediumint` / `int` / `year` / `float` / `double` / `real` / `char` / `tinytext` / `mediumtext` / `longtext` / `binary` / `varbinary` / `tinyblob` / `blob` / `mediumblob` / `longblob` / `time` / `datetime` / `json` / `bit` / `enum` / `set`。

尚未走真实 fixture 测的边缘:`FLOAT` 文本保真、`BIT(>64)`、`ENUM` 在 binlog 里的位索引写回时的列宽溢出、跨字符集(MySQL `binary` collation 与 utf8mb4 的字节序列差异)、`NULL` vs 空字符串、`TIMESTAMP` 的小数秒四舍五入、`JSON` 的 key 顺序与数字 vs 字符串的归一化。

## 8. 仓库状态

分支 `main`。本轮验证：`go test ./...`、`go vet ./...`、真实 MySQL integration suite 和 `unredo doctor` 均通过。

## 9. 下一步建议

按 ROI 排序:

1. **`unredo init` 交互向导**(1 天)
   - 生成 profile、随机 server_id、权限 SQL 和 migration 提示
   - 默认不保存管理密码，不静默修改服务端配置

2. **大事务阈值基准**(0.5 天)
   - 写脚本灌 N=10k/100k/1M 行,记录解码耗时、内存峰值、JSON 大小
   - 用测量值替换 `DefaultPolicy` 的占位

3. **完整交替链与提交未知核验**(1 天)
   - 从最新 REAPPLY 生成 chained revert，并复用 root/parent/depth 状态机
   - 为 `ErrCommitUnknown` 增加按 action ID 核验入口

4. **发布工程与安装说明**(半天)
   - 当前 README 是 DESIGN 摘要
   - 加安装(从源码 `go install` / 跨平台构建)、快速上手、最小权限 SQL 模板

建议下一步先实现 `plan resolve` 或 `unredo init`，再做阈值基准和实验版发布。

## 10. 一些工具和约定备忘

- **PowerShell**,不是 bash(Windows 主机)
- Go 1.26.5 at `D:\tool\Go\bin`
- MySQL 8.4.8 at `D:\tool\mysql\bin\mysql.exe`,root 密码 `123456`
- 密码不进命令行,只走 `UNREDO_READER_PASSWORD` / `UNREDO_EXECUTOR_PASSWORD` 环境变量
- 默认 Go 入口:`go run ./cmd/unredo` 或 `make build` 后 `./bin/unredo.exe`
- binlog 库(`go-mysql-org/go-mysql`)的 INFO 日志默认被 `cmd/unredo/main.go` 静默,要 verbose 时 `set UNREDO_BINLOG_VERBOSE=1`
