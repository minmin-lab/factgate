# Compose 启动、Navicat 与 MCP 演示

## 1. 准备环境

需要 Docker Engine 与 Docker Compose v2。人工 MCP 演示还需要本机 MCP 客户端；服务只绑定 `127.0.0.1`，无法从不具备宿主机回环访问能力的云端客户端直接连接。

```bash
cp .env.example .env
openssl rand -base64 32
./scripts/generate-ed25519-env.sh
```

将第一条随机输出填入 `GATEWAY_DATA_KEY`，并为该数据密钥设置稳定的 `GATEWAY_DATA_KEY_ID`；Gateway 用它在对象存储客户端侧加密 Parquet，同一批已保存结果必须继续使用原 key 和 key ID，直到对应 key ID 被擦除。把密钥脚本输出的七个非注释变量填入 `.env`。OA 只持有审批回执私钥，Gateway 只持有对应公钥；Gateway 查询回执使用另一把独立私钥。生产网关也可用 `OA_RECEIPT_KEYRING_JSON` 提供多把 OA 验签公钥及 `valid_from`/`retired_at` 窗口，旧的单公钥变量仍可用于本地演示。Gateway 查询回执验证公钥由 `/.well-known/taskgate/query-receipt-keyring.json` 发布；`GATEWAY_RECEIPT_KEYRING_JSON` 可加入历史公钥和 active key 的有效/退役窗口。配置 `GATEWAY_AUDIT_ANCHOR_URL` 后，Gateway 会用独立 audit-anchor Ed25519 key 定期向外部日志/WORM 服务 POST 当前审计 checkpoint。随后替换 `.env` 中全部空密码、Token 和共享密钥。系统控制库、业务库和结果对象存储使用不同的管理员/应用凭据：

- 控制库：`CONTROL_POSTGRES_ADMIN_PASSWORD`、`CONTROL_DB_PASSWORD`
- 业务库：`POSTGRES_PASSWORD`、`GATEWAY_DB_PASSWORD`

上述数据库口令**必须用 URL-safe 字符集生成**，例如 `openssl rand -hex 32`，不要用 `openssl rand -base64 32`。Compose 把它们原样插入 `postgres://user:${PASSWORD}@host:5432/db` 形式的 DSN 且无法百分号编码，口令中的 `/`、`+`、`@`、`:`、`?`、`#` 会截断 userinfo，snapshot 编译等 sidecar 会在任何测量开始前以 `parse SNAPSHOT_POSTGRES_DSN` 退出。其余密钥（`GATEWAY_DATA_KEY`、各 Ed25519 seed 等）不进 DSN，仍按 base64 生成。
- 对象存储：`MINIO_ROOT_USER`/`MINIO_ROOT_PASSWORD` 仅供 MinIO 与一次性初始化器使用；Gateway 使用 bucket-scoped 的 `GATEWAY_OBJECT_STORE_ACCESS_KEY`/`GATEWAY_OBJECT_STORE_SECRET_KEY`

`.env` 已被 Git 忽略。不要把真实值复制到提交、提示词、日志或截图中。同一批加密查询结果必须继续使用原 `GATEWAY_DATA_KEY` 和 `GATEWAY_DATA_KEY_ID`，否则对象存储中的 Parquet 无法解密；管理员擦除 key ID 后，即使对象仍存在，Gateway 也会拒绝读取。生产部署还应独立设置 `GATEWAY_DELIVERY_SIGNING_KEY`；`GATEWAY_RESULT_DELIVERY_TTL` 默认 `5m`。

## 2. 启动与健康检查

```bash
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8082/health/live
curl -i http://127.0.0.1:8082/health/ready
curl -i http://127.0.0.1:8092/health/ready
```

Compose 会先运行两个一次性 `snapshot-index` compiler。它们只接入内部
`business-data` 网络，以 `gateway_reader` 在只读 `REPEATABLE READ` 事务中扫描
输入声明的冻结 Reporting materialized view，再把 `config/snapshots/*.json` 中的
candidate rows 完全替换为数据库真实 rows。构建器逐列校验 PostgreSQL type、
collation 及版本，并要求 relation 已 populated 且由不在 reader 角色继承链中的
NOLOGIN 角色持有。输入中固定了人类审核过的五类 digest；已有 publication 只有在
四个文件逐字节一致时才可复用，否则启动失败。artifact 写入只读挂给 Gateway 的
named volume，构建器不能连接 public-edge 或 control-plane。

Compose 还会启动只接入内部 `result-storage` 网络的 MinIO，并运行一次性 `result-object-store-init` 创建私有 bucket 和 bucket-scoped Gateway 用户。MinIO root 凭据不会注入 Gateway；Gateway 把 Parquet 客户端侧分块加密后写入 `result-object-data`，Control PostgreSQL 只保存 artifact 元数据。result bucket 必须保持私有并禁用 versioning；Gateway readiness 会拒绝 versioning 为 Enabled 或 Suspended 的 bucket，因为 delete marker 不等于 TTL/purge 要求的实体删除。

三个请求应返回 `204 No Content`。Gateway readiness 同时检查系统控制 PostgreSQL、业务 PostgreSQL、对象存储 bucket、Reporting View 列/类型与 Catalog 的 Schema Attestation，以及是否存在尚未成功持久化的预算结算。启动时 Gateway 会先恢复已提交但尚未完成 canonical promotion 的 `PENDING` artifacts；恢复失败会阻止 readiness。

| 服务 | 地址 | 暴露范围 |
|---|---|---|
| MCP | `http://127.0.0.1:8082/mcp` | 回环地址 |
| Gateway 健康检查 | `http://127.0.0.1:8082/health/live`、`/health/ready` | 回环地址 |
| 查询回执验证 Keyring | `http://127.0.0.1:8082/.well-known/taskgate/query-receipt-keyring.json` | 回环地址 |
| OA 登录 | `http://127.0.0.1:8092/login` | 回环地址 |
| 系统控制 PostgreSQL | `127.0.0.1:25433` | 回环地址 |
| 业务 PostgreSQL | 默认无宿主机地址 | 仅 Compose 内部网络 |
| MinIO S3 API | `http://result-object-store:9000` | 仅 `result-storage` 内部网络，不发布宿主机端口 |

查看日志：

```bash
docker compose logs -f gateway control-postgres business-postgres result-object-store result-object-store-init oa-demo
```

## Navicat 连接参数

### 系统控制库

| Navicat 字段 | 值 |
|---|---|
| 主机 | `127.0.0.1` |
| 端口 | `25433`，或 `.env` 的 `CONTROL_POSTGRES_PORT` |
| 数据库 | `.env` 的 `CONTROL_POSTGRES_DB`，默认 `taskbound_gateway` |
| 管理用户名 | `postgres` |
| 管理密码 | `.env` 的 `CONTROL_POSTGRES_ADMIN_PASSWORD` |
| Gateway 应用用户名 | `gateway_control` |
| Gateway 应用密码 | `.env` 的 `CONTROL_DB_PASSWORD` |

调试表结构建议使用管理员账号；验证 Gateway 最小权限时使用 `gateway_control`。

### 业务数据源库

默认论文部署不发布业务库端口。只有明确用于本机调试时才运行：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

同一 debug override 还会把 MinIO S3 API 发布到 `127.0.0.1:${MINIO_API_PORT:-29000}`、Console 发布到 `127.0.0.1:${MINIO_CONSOLE_PORT:-29001}`；普通运行不需要也不应发布这两个端口。

| Navicat 字段 | 值 |
|---|---|
| 主机 | `127.0.0.1` |
| 端口 | `25434`，或 `.env` 的 `POSTGRES_PORT` |
| 数据库 | `.env` 的 `POSTGRES_DB`，默认 `travel_demo` |
| 管理用户名 | `.env` 的 `POSTGRES_USER`，默认 `postgres` |
| 管理密码 | `.env` 的 `POSTGRES_PASSWORD` |
| Gateway 只读用户名 | `gateway_reader` |
| Gateway 只读密码 | `.env` 的 `GATEWAY_DB_PASSWORD` |

`gateway_reader` 只能读取 `reporting.datasource_attestation`、已发布的 Reporting View
以及两张只含 entity key/row handle 的 `taskgate_ordinal` companion；不能访问
`legacy.*`，也不能执行写操作。

也可以用容器内 `psql` 验证：

```bash
docker compose exec control-postgres \
  psql -U postgres -d taskbound_gateway

docker compose exec business-postgres sh -c \
  'PGPASSWORD="$GATEWAY_DB_PASSWORD" psql -U gateway_reader -d "$POSTGRES_DB" -c "SELECT count(*) FROM reporting.expense_summary"'
```

## 3. 连接 MCP 客户端

客户端连接参数：

```toml
[mcp_servers.taskbound_gateway]
url = "http://127.0.0.1:8082/mcp"
bearer_token_env_var = "TASKBOUND_GATEWAY_TOKEN"
required = true
startup_timeout_sec = 10
tool_timeout_sec = 120
```

Alice 会话的环境变量值必须与 `.env` 中的 `TASKBOUND_ALICE_TOKEN` 一致：

```bash
export TASKBOUND_GATEWAY_TOKEN='<Alice Token>'
codex
```

MCP `serverInfo.version` 为 `2.1.0`。Alice 可见 22 个工具（17 个任务/查询工具、3 个知识检索工具、2 个文档下载工具），Carol 只可见两个审计工具：

| 身份 | 工具 |
|---|---|
| Alice | `list_data_products`、`describe_data_product`、`get_sql_capabilities`、`request_data_task`、`list_my_tasks`、`get_task_status`、`wait_for_approval`、`get_task_context`、`rebind_data_task`、`query_sql`、`get_query_result`、`preview_result`、`deliver_result`、`get_budget`、`list_receipts`、`complete_task`、`revoke_task`；知识检索 `search`、`fetch`、`aggregate`；下载 `request_download_task`、`download_document` |
| Carol | `list_audit_events`、`get_audit_receipt` |

`execute_plan` 保留给 SDK、内部测试、基准、调试和确定性工作流，不在普通 Alice `tools/list` 中默认列出。

## 4. 结构化申请与人工审批

先调用 `list_data_products`。响应会给出逻辑产品字段，以及全部 Scope 的类型、允许值或日期边界。可用 `describe_data_product(name)` 读取字段 collation、Catalog 稳定角色和 SQL 白名单，用 `get_sql_capabilities()` 读取 `taskgate-reporting-sql-v1` 的受支持特性、2–16 源 connected INNER equi-join graph 边界和通用请求/资源上限。申请不得省略产品、字段或 Scope：

```json
{
  "objective": "按月份分析销售部差旅报销",
  "data_products": ["expense_summary"],
  "columns": {
    "expense_summary": ["month", "total_amount"]
  },
  "scopes": {
    "department": ["销售部"]
  }
}
```

每个申请产品必须有非空字段列表；未知产品、字段、Scope 或越界值会被拒绝。

对声明了 `eligibility` 的产品，Gateway 还会在建草稿之前用申请人的属性快照（`config/principal-attributes.yaml`，
由 `PRINCIPAL_ATTRIBUTES_PATH` 挂载）编译"可申请范围"：资格条件不满足或 Scope 超出可申请范围时直接返回 `NOT_ELIGIBLE`，
不建任务、不进审批队列。例如默认 Catalog 把 `expense_detail` 的 `department` 限定为申请人自己的部门；Alice 属于
销售部，申请 `{"department": ["研发部"]}` 会被当场拒绝，而不是交给 Bob 决定（详见 catalog-guide 的 Eligibility 节）。

除 Alice 与 Carol 外，部署可以用 `PRINCIPALS_PATH` 指向一份主体文件，增加更多 query 主体（例如资格实验需要的不同部门、
不同 clearance 的申请人）。文件格式 `taskgate-principals-v1`：每项 `id`（`principal_` 开头）、`subject`、`role`（只能是
`query`）、`token_sha256`（bearer token 的 SHA-256，小写十六进制）；文件里只有摘要，没有 token。Gateway 启动时像引导 Alice
一样逐个引导，未知键、重复或与 Alice / Carol 冲突的 id、subject、token 摘要都使启动失败；已持久化的同 id 主体与文件不一致
时同样拒启动。认证时网关对出示的 token 求 SHA-256，与文件里的摘要做常数时间比较。这些主体的属性仍写在
`PRINCIPAL_ATTRIBUTES_PATH` 的快照里。
Gateway 按产品的最高敏感级别选择 Catalog Profile，并把完整资源与三维
exposure 上限写入审批 Manifest。Agent 不申请更小预算，也不因剩余额度获得收益；
批准额度必须在被全部消耗时仍然满足企业安全政策。

`request_data_task` 返回 `task_id`、`oa_url`、审批模式和 `AuthorizationManifestV1` 摘要。用 `.env` 的 `OA_ALICE_PASSWORD` 以 `alice` 登录 OA，打开草稿并提交。所有敏感级别（包括低敏 `expense_summary`）都会停在 `AWAITING_APPROVAL`；必须由 `bob` 登录 OA 明确批准或缩小后批准，任务才会进入 `ACTIVE`。

任务 ACTIVE 后，普通 Agent 提交 SQL：

```json
{
  "task_id": "task_...",
  "request_id": "summary-sql-001",
  "sql": "SELECT month, SUM(total_amount) AS amount FROM expense_summary GROUP BY month ORDER BY month ASC LIMIT 20"
}
```

Exposure-enabled `query_sql` 使用 PostgreSQL AST 解析 `taskgate-reporting-sql-v1`，将 SQL alias 映射为 Catalog 稳定角色，并无损 lowering 为 canonical QueryPlan。单产品支持 projection/filter/group/order/limit/offset 与 `COUNT/SUM/MIN/MAX`；多产品支持 2–16 源内的任意 connected INNER equi-join graph 形状，每条 edge 可包含多个 column-to-column equality predicate。Lowering 对 nodes、edges、predicates 及 equality 两端规范排序并转换为现有 `join_many`，再 deterministic binary fold 为现有二元代数。16-source 上限是 operational complexity/DoS ceiling，所以 10 表 Join 在支持范围内；请求还受 1 MiB MCP 请求体、AST 白名单校验和现有资源预算/超时/行数上限约束。

多产品排序按结构开放：查询必须是带非空 `GROUP BY` 的 connected INNER equi-join，每个 group key 都要直接投影，并在 `ORDER BY` 中恰好出现一次且仅出现一次；方向只允许 `ASC`/`DESC`（省略等价于 `ASC`），不能带 `LIMIT/OFFSET`。Visible SQL 遵循请求的 group-key 顺序，ordinal companion 独立保持 canonical group/entity 升序。顶层 projection cast 只接受未修饰、非嵌套的 `bigint`/`int8`、`numeric`、`text`：与自然 canonical 类型相同的 identity cast 会被消除；唯一非 identity 形式是同一完整排序 grouped Join 中自然结果为 `numeric` 的精确 `SUM` 通过 `postgresql-numeric-text-v1` 以 PostgreSQL wire `text` 展示，记账类型仍为 `numeric`，无 `ORDER BY` 时不得使用。

Self-join、outer/cross/non-equality/`NATURAL`/`USING` join、断开的 join graph、子查询、CTE、set operation、窗口、`HAVING`、位置式 group/order、显式 `NULLS FIRST/LAST` 和 `ORDER BY USING` 关闭式拒绝。多产品查询还拒绝未分组、部分/重复/未投影 group key、aggregate/表达式排序和全部分页；未分组或 Union result encoding 及其它 projection cast 同样拒绝。Gateway 不会静默修改查询语义。

lowering 成功后，Gateway 丢弃原始 SQL 作为执行来源，只执行 QueryPlan 重新生成的 visible SQL 和 ordinal provenance companion，两者还会再经完整 SQL policy。高级 `execute_plan` 也共用这一编译/记账边界，并且仍可表示同产品双分支 `union_distinct`；该 set operation 不属于 SQL profile。Gateway 不调用外部模型。

对声明 `view_contract` 的 semantic root，Gateway 还会把 public outputs 按已绑定
`Artifact.Output.FieldID` 合成到递归展开的 Scan/`join_many` 计划，再进入
同一 `CompileRelational`/V5 exposure 路径（Release/Influence 仍使用 V4 ordinal）。对外 Grant 仍只含 root；每次查询派生
最小 terminal internal grants，并把 root Scope 映射到每个 terminal mandatory
scope。当前 semantic root 必须是外层唯一 Product，不接受它与其他
query-time Product 的 Join，也不接受 root 上的 `ORDER BY`/`LIMIT`/`OFFSET`；
这不限制 View closure 内部在 16-source/16-depth ceilings 下的任意 connected
INNER equi-join 或每 edge 多 equality predicates。已聚合 closure 之上只允许纯投影。

默认 Catalog 定义 `taskgate-exposure-v5` budget profiles，且所有 approval
route 都是人工审批。正常批准把完整 Catalog Profile 交给 Agent，不做最小预算选择。Gateway 会在一个只读
`REPEATABLE READ` 事务中缓冲可见查询并流式读取 ordinal companion，再以 exact bitmap 与 Outcome Merkle radix set 对共享 root head 结算。响应中的 `exposure` 给出本次
`actual_*_facts` 与真正新增的 `charged_*_facts`；`exposure_budget` 给出共享
根账本。内部补取的 `entity_key` 不会出现在客户端结果中。

成功的 `query_sql`/`execute_plan` 以及后续 `get_query_result(task_id, query_id)` 都只返回 `result_id`、列定义、行列数、`expires_at`、计时和计费摘要，不返回完整 `rows`、对象键或永久下载地址。例如：

```json
{
  "result_id": "res_123",
  "row_count": 1254307,
  "column_count": 8,
  "expires_at": "2026-08-01T12:00:00Z"
}
```

需要抽查数据时调用 `preview_result`；`limit` 默认为 20，最大为 100，并支持非负 `offset`，同时受 `GATEWAY_RESULT_PREVIEW_MAX_BYTES` 的解密读取上限保护（默认 64 MiB）。需要完整文件时调用 `deliver_result(result_id, format="parquet")`，返回默认 5 分钟、且不会超过 artifact TTL 的 Gateway 签名 capability URL；它由 Gateway 鉴权并流式解密，不是 S3 直链。交付流另受 `GATEWAY_RESULT_DOWNLOAD_TIMEOUT`（默认 30 分钟）和 `GATEWAY_RESULT_DOWNLOAD_CONCURRENCY`（默认 4）约束，容量占满时返回可重试的 `503`。canonical Parquet 对象创建成功时结果已经计为 `consumed`；生成 URL、实际下载、未下载或有效期内重复下载都不会再次扣预算，也不会改变 Receipt。

`GATEWAY_PUBLIC_BASE_URL` 使用 HTTP 时只允许 loopback 开发地址；生产的非 loopback 基址必须使用 HTTPS。capability URL 在到期前是 bearer secret；反向代理、访问日志、APM 和客户端遥测不得记录完整查询串，至少必须对 `token` 脱敏。

当前实现的 Connector/可见结果到 Parquet 的部分路径仍会在内存中持有完整结果，并且 preview 默认无法读取大于 64 MiB 的 artifact。因此 `GATEWAY_CONNECTOR_MAX_ROWS=1200000` 是关闭式行上限，不是百万行路径已具备有界内存的证明；生产部署应先完成有界流式 Parquet writer/reader 和容量基准。

## 5. 人工审批、规划与委托

申请高敏 `expense_detail` 时，Alice 提交草稿后任务停留在 `AWAITING_APPROVAL`。以 `bob` 和 `OA_BOB_PASSWORD` 登录 OA 批准或拒绝。批准前查询返回 `TASK_NOT_ACTIVE`；拒绝后任务归档为 `ARCHIVED(rejected)`。

启用 exposure 的 Grant 默认使用 `query_sql`。语法、授权或 lowering 失败会在数据库执行和正式预留之前返回结构化错误，其中包含稳定 `code`、安全的 `reason/location`、支持的替代方案、`retryable_after_rewrite` 和 SQL profile；这类失败不扣查询、行、DBMS、release、dependency 或 outcome 预算。执行已开始后的 timeout 或故障继续遵守现有 failure-settlement 规则。

原始 SQL 派生的 request digest 仅用于审计和 `(task_id, request_id)` 幂等；对同一 equi-join graph 的 alias 改名、Join 交换/括号/遍历顺序、edge 顺序、predicate 顺序和 equality 操作数交换必须得到同一 canonical QueryPlan、`plan_digest`、FactID 和 semantic replay key。原始 SQL 文本哈希不是 exposure 命题身份。

`request_data_task` 还支持 `parent_task_id` 和 `delegate_principal_id`。子任务必须
由父任务所有者发起，所有授权维度只能收缩，且父子共享同一 exposure 账本。
默认 Compose 只注册 Alice 这个 query Principal，因此跨主体演示需要部署者先
注册第二个已启用 query Principal；同主体也可创建受父 Grant 约束的子任务。

## 6. 预算、结果与审计

- `list_my_tasks`：列出自己名下的任务（自己发起的任务，以及管理员为自己开的授权单），可按 `state` 过滤；每页最多 100 条，用 `next_cursor` 续读。
- `get_task_context`：获批产品、字段、Scope、凭证与期限。
- `get_budget`：查询数、累计行数和累计 DB 毫秒的上限、已用、预留和剩余值，并在 exposure 启用时返回根任务三维账本。
- `get_query_result`：Alice 按 `task_id + query_id` 读取自己的 `result_id` 与结果摘要，不返回完整行。
- `preview_result`：Alice 按 `result_id` 读取最多 100 行的有界预览。
- `deliver_result`：Alice 按 `result_id` 获取由 Gateway 流式解密 Parquet 的短 TTL 下载 URL；URL/下载不参与计费。
- `list_receipts`：V4 ordinal exposure 结算使用 V6；旧 V5/V4/V3 verifier 仅用于兼容历史回执。
- `complete_task`：主动归档任务。
- `revoke_task`：阻止新查询；已在途查询不会被宣称立即取消，仍受原超时和 Grant 到期约束。

任务被撤销、过期或完成归档后，旧 artifact 仍可由任务所有者读取摘要、预览或交付，直到 artifact 到期、TTL/purge 删除 canonical 对象，或管理员擦除对应结果 key ID；查询回执和审计证据不会随对象清理或 key ID 擦除删除。设置 `GATEWAY_RESULT_RETENTION_TTL` 会启动定期对象清理；设置 `GATEWAY_ADMIN_TOKEN` 会启用 `/admin/v1/retention/purge`、`/admin/v1/retention/legal-holds/{task_id}` 以及 `/admin/v1/result-encryption-keys/{key_id}/erase` 的本机管理员接口。active legal hold 会阻止该任务的对象进入 TTL/手动 retention 清理，但不会延长 `expires_at` 或继续开放 metadata/preview/delivery，也不会自动阻止单独的 key ID 擦除流程；生产环境应把该接口接入组织级审批/KMS 流程。禁用 Principal 会阻止该身份继续列出或调用任何 MCP 工具，即使客户端仍持有旧 Bearer Token。达到任一资源预算硬上限时，当前合法查询会在允许范围内生成 canonical Parquet 并返回摘要，随后任务归档为 `budget_exhausted`；exposure 超限则不会产生 canonical 对象。Carol 只能读取审计事件和凭证，不能读取原始行。

## 7. 停止、恢复与重置

```bash
docker compose down
```

再次启动时，Gateway 从 `control-pg-data` 恢复任务、Grant、预算、artifact 元数据与审计链，并从独立的 `result-object-data` 继续访问客户端侧加密 Parquet；业务数据保存在 `business-pg-data`。不确定是否完成的 RESERVED 查询按完整资源预留量保守计费并标记 `INDETERMINATE`，释放未结算的 exposure reservation，并在同一恢复事务中写入查询回执；同一 `request_id` 禁止自动重执行。对于结算已提交的 `PENDING` artifact，Gateway 只在 staging 与已提交哈希/ETag 证据一致时，使用确定性 canonical key 幂等完成 promotion 并标记 `AVAILABLE/consumed`，不退款、不重跑 SQL、不重复写 Receipt。staging 丢失或 canonical 证据冲突会使 readiness fail closed，必须先恢复正确对象证据或执行受审修复；系统不会自动放弃或退款。PROCESSING 回调变为可重试。OA Demo 草稿仍在内存中，OA 容器重启后会丢失。

不要为 `staging/` 配置仅按对象年龄的 S3 lifecycle；这可能删掉 `PENDING` 恢复仍需要的证据。Gateway orphan sweeper 等待 `GATEWAY_RESULT_STAGING_ORPHAN_TTL`（默认 24h），并在查询 Control 引用后只清理真正孤儿 staging；它也会对超期的 S3 multipart fragments 按确切 upload ID 执行 abort。

彻底重置当前版本数据：

```bash
docker compose down --volumes --remove-orphans
```

该命令删除当前 Compose 项目的 `control-pg-data`、`business-pg-data`、`snapshot-index-artifacts`、仅保存认证密文临时文件的 `gateway-encrypted-spool` 和保存规范加密 Parquet 的 `result-object-data`。旧版本曾使用的 `gateway-data` Volume 不再被 Compose 引用，本次改造不会自动删除它；如需恢复或清理，请先用 `docker volume ls` 确认确切名称并手工处理。

## 8. 常见问题

| 现象 | 原因与处理 |
|---|---|
| Gateway 启动即退出 | 检查必填环境变量、32 字节数据密钥、Catalog、控制库、业务库、MinIO bucket 和 bucket-scoped 凭据 |
| 修改 Alice/Carol Token 后 Gateway 拒绝启动 | 控制 PostgreSQL 已保存原 Principal 摘要；恢复旧 Token，或明确接受丢失 Demo 历史后重置 `control-pg-data` |
| 旧结果无法解密 | `GATEWAY_DATA_KEY` 或 `GATEWAY_DATA_KEY_ID` 与对象写入时不同；恢复原密钥和 key ID；若 key ID 已被管理员擦除，则只能读取查询记录、回执和审计证据 |
| ACTIVE 任务目录版本冲突 | Catalog 版本已变化；重新申请新版本任务 |
| OA 页面没有旧草稿 | OA Demo 为内存实现，重启不持久化草稿 |
| Navicat 无法连接业务库 | 默认部署不发布该端口；仅调试时显式叠加 `compose.debug.yaml`，再使用 `127.0.0.1:25434` |

## 知识产品（KB-1）

1. 业务库使用 `pgvector/pgvector:pg16` 镜像；`db/init/30-kb-schema.sh` 在 pgvector 可用时安装 `kb.*` 表（扩展装在 `pg_catalog`）。
2. 用 `kb-publish -spec <spec.json> -artifact-dir <snapshot artifact root>` 冻结一个 collection：写入 `kb.*`、建物化视图、编译三个
   bundle，并在 `<root>/kb/<epoch>/` 留下清单与 `catalog-fragment.yaml`；嵌入模型为 `bge-m3` 时需 `KB_EMBEDDING_ENDPOINT` 指向
   受信嵌入服务（`/model`、`/embed`），`KB_EMBEDDING_CACHE_DIR` 可选用于断点续算。
3. `kb-publish -merge` 把片段并入 Catalog，按 docs/catalog-guide.md 的发布清单重编 snapshot-index、安装 sidecar、重算 `schema_digest`。
4. Gateway 启动时激活知识发布物：三个 bundle 与其他 publication 一样放在 `GATEWAY_SNAPSHOT_ARTIFACT_DIR`，kb-publish 输出的
   `<root>/kb` 目录单独挂给 `GATEWAY_KB_ARTIFACT_DIR`（snapshot 目录里多一项未声明条目即拒启动）；并按 Catalog 选择查询嵌入器
   （`KB_EMBEDDING_ENDPOINT`，compose 默认为空：文本查询拒绝、向量查询可用）。业务库的 `KB_REQUIRE_PGVECTOR` 默认 0，
   知识产品部署应设为 1，镜像缺 pgvector 时初始化即失败。
5. 任务同时申请 chunks 与 documents 两个 Product（aggregate 申请 values），批准后用 `search` / `fetch` / `aggregate`；结算与回执
   与 `execute_plan` 相同。端到端示例见 `internal/gateway/retrieval_postgres_test.go`（环境变量 `KB_E2E_*`）。
