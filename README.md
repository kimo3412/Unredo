# Unredo

Unredo 计划成为一个用 Go 编写的数据库事务补偿 CLI。首个后端面向 MySQL 8 ROW binlog：选择一笔已提交事务，生成自包含的补偿计划，检查当前数据冲突，并以一笔新的事务安全地 revert 或 reapply。

它的语义类似 `git revert`，不会倒放或修改 InnoDB redo log。

> 当前状态：**M0-M2 已完成，M3 完整交替 action 链、冲突 resolve、初始化流程、COMMIT 未知恢复、补偿 GTID 精确关联、大事务基准与边缘类型回归已跑通**。实验版发布仍待办。

## 安装与版本核验

实验版优先从 [GitHub Releases](https://github.com/kimo3412/Unredo/releases) 下载与系统匹配的压缩包。发布流水线提供 Windows amd64、Linux amd64/arm64 和 macOS amd64/arm64 产物；每个包只包含可执行文件和 README，并在 `checksums.txt` 中提供 SHA-256：

```bash
sha256sum -c checksums.txt --ignore-missing
unredo version
unredo --version
unredo --format json version
```

`unredo version` 会显示版本、Git commit 和可复现的构建时间。正式 tag 构建会把这些信息写入二进制；直接 `go install github.com/girimi/unredo/cmd/unredo@latest` 或本地 `go build` 得到的无注入构建会显示 `0.1.0-dev/unknown`，适合开发，不应冒充发布产物。

从源码构建要求 `go.mod` 声明的 Go 版本：

```bash
git clone https://github.com/kimo3412/Unredo.git
cd Unredo
go test ./...
go build -o unredo ./cmd/unredo
```

推送 `v*` tag 后，发布流水线会先运行单元测试、双模式 vet 和真实 MySQL 8.4 集成回归，再交叉编译、打包、生成校验和并创建 GitHub Release；任一安全回归失败都不会发布产物。

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

成功执行会输出 action ID、affected rows 和补偿事务 GTID。GTID 不取 `@@global.gtid_executed` 的尾部：Unredo 在 apply 前捕获 binlog 起点，提交后从该位置扫描，并且只接受包含本次 16 字节 action marker 的事务，因此不会把并发事务误认为补偿事务。MySQL 8.0 的 `SHOW MASTER STATUS` 和 MySQL 8.2+ 的 `SHOW BINARY LOG STATUS` 均受支持。

GTID 关联是提交后的审计增强，不参与事务成败判断。如果补偿已经提交，但 binlog 被立即 purge、replication 连接失败或关联超时，命令仍按成功返回 action ID，同时输出 `GTID correlation failed` warning，GTID 留空；不得因为 GTID 为空而重试补偿。

### 已验证的 MySQL 类型边界

真实 MySQL 8.4.8 端到端回归已覆盖 FLOAT、DOUBLE、BIT(64)、ENUM、SET、utf8mb4 文本、VARBINARY、NULL 与空字符串、TIMESTAMP(6)、DATETIME(6)、TIME(6)、JSON、DECIMAL(30,10) 和 `BIGINT UNSIGNED` 最大值。回归会先修改全部字段，再生成并执行 revert，最后逐字段验证原值。

实现不会用 `float64` 中转任意精度整数或 DECIMAL；计划规范化使用 `json.Number` 保留 `18446744073709551615` 等大整数。JSON 谓词把参数显式转换为 MySQL JSON 后比较，BIT 则以固定宽度二进制参数写回并比较，避免字符串参数在 MySQL 中产生不同的隐式转换语义。遇到 NaN、Infinity、非法 ENUM/SET 索引、未知 SET 位或超出 BIT 宽度的值会 fail closed。

### 大事务保护与实测阈值

默认策略保持为 `max_transaction_rows: 1000`、`max_transaction_bytes: 64MiB`、`max_plan_bytes: 128MiB`。2026-08-03 在 Windows、Go 1.26.5、MySQL 8.4.8 本地实例上的端到端参考结果如下；数据用于选择安全默认值，不是跨机器性能承诺：

| 行数 | 每行二进制负载 | 解码 | 生成并写 plan | check | 原子 apply | plan 大小 | 峰值堆 | 累计分配 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1,000 | 512 B | 48 ms | 58 ms | 211 ms | 582 ms | 1.85 MB | 17.8 MB | 93.7 MB |
| 10,000 | 512 B | 122 ms | 522 ms | 2.03 s | 6.48 s | 18.58 MB | 163.9 MB | 925.4 MB |
| 1,000 | 8,192 B | 146 ms | 261 ms | 217 ms | 869 ms | 12.09 MB | 133.0 MB | 695.8 MB |

实测同时修复了 checker 每行创建连接导致约 5,000 行后耗尽客户端临时端口的问题；现在一次 check 复用连接和 schema cache。计划缩进也不再把完整 JSON 解析成第二棵通用对象树。

超过任一阈值时，Unredo 丢弃完整 row image，保留真实行数和涉及表的摘要，因此仍可 `txn list/show`，但 `plan create` 会 fail closed。负数和零值不能用于绕过安全阈值。若显式提高限制，应先在相同 MySQL 版本、表宽和运行机器上复测：

```powershell
$env:UNREDO_RUN_LARGE_BENCHMARK='1'
$env:UNREDO_BENCH_ROWS='1000,10000'
$env:UNREDO_BENCH_PAYLOAD_BYTES='512'
go test -tags=integration -run TestLargeTransactionMetrics -count=1 -v -timeout 300s ./tests/integration
```

### COMMIT 结果未知时的恢复

如果执行 `COMMIT` 时连接中断，客户端无法仅凭报错判断事务是已提交还是已回滚。Unredo 会输出预先生成的 `action_id`、`COMMIT_UNKNOWN` 和恢复命令。此时不要直接重试 `plan apply`，并保留本次执行使用的原始计划文件：

```text
status:       COMMIT_UNKNOWN
action_id:    01K...
plan:         undo-981.json
retry:        FORBIDDEN until verification
verify:       unredo action verify --action-id 01K... --plan undo-981.json
```

使用同一个 config/profile 和原始计划核验结果：

```bash
unredo action verify \
  --profile local \
  --action-id 01K... \
  --plan undo-981.json \
  --wait 5s
```

核验会先确认当前 target 的实例 UUID 与计划一致，再检查与数据修改在同一事务中写入的 action marker：

- `COMMITTED`：marker 与 action ID、计划 ID、计划摘要、源事务及目标实例完全匹配；补偿已经提交，不得重试。
- `NOT_COMMITTED`：在等待窗口内未发现 marker；再次执行前必须重新运行 `plan check`。
- `INDETERMINATE`：目标实例不一致、marker 与计划不匹配、查询失败或超时；命令以非零状态退出，禁止重试，需由 DBA 排查。

`--wait` 默认是 5 秒，用于覆盖提交后 marker 暂时不可见的短窗口。自动化可用全局 `--format json` 获取结构化结果。

该恢复链路包含真实 MySQL 协议级故障测试：测试代理先把 `COMMIT` 送达服务器，再吞掉成功响应并断开客户端连接，以验证已提交事务能恢复为 `COMMITTED`，且同一计划不会被成功执行第二次。

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

## Revert/Reapply action 链

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

重新应用后如果需要再次回退，必须从最新的 `REAPPLY/ORIGINAL_APPLIED` action 显式生成 chained revert：

```bash
unredo action revert \
  --action-id 01J... \
  --root-plan undo-981.json \
  --output undo-981-depth-2.json

unredo plan check undo-981-depth-2.json
unredo plan apply undo-981-depth-2.json
```

后续可继续从最新 action 显式交替生成 `reapply → revert → reapply`。两个生成命令都会校验根计划、源事务、目标状态、父 action、最新成功 action 和链深度；它们只写计划文件，不修改数据库。

同一方向不能连续执行，旧 action 不能创建成功分支。每个新计划仍必须单独 `check/apply`，profile 的 `max_action_depth` 是硬终止条件；超过上限时不会生成计划文件。提高该值是显式策略变更，不存在自动循环或全局无限撤销栈。

## MVP 边界

- MySQL 8、InnoDB、ROW/FULL binlog、GTID。
- 支持 INSERT、UPDATE、DELETE。
- DDL、XA、跨实例事务和数据库外副作用不支持自动恢复。
- 首版只支持远程 replication stream；本地 binlog 文件是后续模式。
- 首版面向 DBA 和数据库开发者，不识别业务用户的“上一步”。

项目采用数据库无关 core + backend adapter 结构。未来增加 PostgreSQL 等后端时，CLI 和核心 revert/reapply 工作流保持不变。
