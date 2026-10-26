# OA 与数据源适配器接口

Gateway 将 OA 草稿创建和数据执行抽象为 Go 接口，但当前 HTTP 协议、Catalog 和 SQL 策略仍针对 Demo OA 与 PostgreSQL。替换适配器时必须保留 Gateway 的确定性授权边界，不能把敏感级别、预算或 SQL 放行交给外部系统。

## OA 出站接口

Gateway 只要求 OA Adapter 创建草稿：

```go
type ApprovalAdapter interface {
    CreateDraft(context.Context, DraftRequest) (DraftResponse, error)
}
```

内置客户端调用：

```http
POST http://oa-demo:8092/api/drafts
Authorization: Bearer <OA_SERVICE_TOKEN>
Content-Type: application/json
```

请求：

```json
{
  "authorization_manifest": {
    "version": "1",
    "task_id": "task_...",
    "human_subject": "alice",
    "agent_id": "principal_alice",
    "declared_objective": "查询销售部月度报销总额",
    "products": ["expense_summary"],
    "approved_columns": {"expense_summary": ["department", "month", "total_amount"]},
    "mandatory_scope": {"department": ["销售部"]},
    "sensitivity": "low",
    "budget": {
      "max_queries": 10,
      "max_result_rows": 500,
      "max_db_ms": 30000,
      "per_query_timeout_ms": 5000,
      "task_ttl_ms": 1800000,
      "max_release_facts": 1000,
      "max_influence_facts": 5000,
      "max_outcome_facts": 10,
      "exposure_profile_version": "taskgate-exposure-v5",
      "predicate_footprint": {
        "version": "taskgate-predicate-footprint-v1",
        "max_raw_literals_per_query": 1000,
        "max_unique_atoms_per_query": 9,
        "max_atom_payload_bytes": 4096,
        "max_total_atom_payload_bytes": 36864
      }
    },
    "catalog_version": "2026-07-25.2",
    "catalog_sha256": "<64 位小写十六进制 SHA-256>",
    "callback_context": "callback_...",
    "nonce": "<32 位小写十六进制随机数>"
  },
  "manifest_digest": "<RFC 8785 + TASKGATE-MANIFEST-V1 域分隔摘要>",
  "approval_mode": "manual",
  "approver": "bob"
}
```

所有审批请求都必须包含 `"approver":"bob"`。OA 必须返回 HTTP `201 Created`；响应不允许未知字段，`draft_id` 与 `url` 必须非空。Demo 同时返回 `state: draft`：

```json
{
  "draft_id": "oa_...",
  "state": "draft",
  "url": "http://127.0.0.1:8092/tasks/oa_..."
}
```

内置客户端超时 5 秒，响应体上限 64 KiB。创建失败时 Gateway 不创建本地任务；若 OA 已创建草稿而后续控制 PostgreSQL 写入失败，当前 Demo 没有补偿删除协议，生产适配器应提供幂等创建和补偿/对账机制。

OA 展示 Gateway 已确定的目标、Agent/人类身份、产品、字段、强制范围、敏感级别、Catalog 完整资源预算、release/influence/outcome 上限、有效期、目录版本与 Manifest Hash。创建、提交和决定前都会重验 Hash。人工审批可明确 `approve/reject/narrow`；Demo 表单可缩小产品、字段、枚举/日期范围、期限和预算。该动作是可信审批人的显式授权收缩，不是 Agent 自选预算或自动利用率校准。协议层也校验敏感级别只能降低、绝不能提高，但 Demo 表单不提供敏感级别编辑控件。任何扩权都会被 OA 与 Gateway 双重拒绝。

委托任务的 Manifest 还包含 `root_task_id` 和 `parent_task_id`。OA 签发的
`TaskGrantCoreV1` 必须原样绑定该 lineage；Gateway 会将 child Grant 与已验签
parent Grant 做逐维 delegation narrowing，而不是只信任 OA 表单。

## OA 回调接口

OA 向 Gateway 调用：

```http
POST http://gateway:8082/api/v1/oa/callback
Content-Type: application/json
X-OA-Event-ID: evt_...
X-OA-Timestamp: <Unix 秒>
X-OA-Signature: v1=<hex HMAC>
```

签名覆盖原始请求字节：

```text
HMAC-SHA256(OA_CALLBACK_SECRET, timestamp + "." + raw_request_body)
```

Body 契约：

```json
{
  "event_id": "evt_...",
  "task_id": "task_...",
  "draft_id": "oa_...",
  "status": "approved",
  "actor": "bob",
  "occurred_at": "2026-07-21T12:00:00Z",
  "catalog_version": "2026-07-21.1",
  "callback_context": "callback_...",
  "manifest_digest": "<与草稿一致的 SHA-256>",
  "approved_grant": {"version":"1", "task_id":"task_...", "...":"TaskGrantCoreV1"},
  "approval_receipt": {
    "version":"1", "receipt_id":"receipt_...", "task_id":"task_...",
    "decision":"approve", "manifest_digest":"<SHA-256>",
    "approved_grant_digest":"<SHA-256>", "approver_id":"bob",
    "issued_at":"2026-07-21T12:00:00Z", "key_id":"oa-ed25519-v1",
    "signature":"<unpadded base64url Ed25519>"
  }
}
```

支持的状态与 actor：

| 状态 | 允许 actor | 状态变化 | 额外要求 |
|---|---|---|---|
| `submitted` | `alice` | `AWAITING_SUBMISSION → AWAITING_APPROVAL` | 无 |
| `approved`（自动） | 已认证申请人 `alice` | `AWAITING_APPROVAL → ACTIVE` | 完整 Grant + Ed25519 回执 |
| `approved`（人工） | Catalog 指定的 `bob` | `AWAITING_APPROVAL → ACTIVE` | 完整 Grant + Ed25519 回执 |
| `narrowed` | Catalog 指定的 `bob` | `AWAITING_APPROVAL → ACTIVE` | 严格收缩 Grant + `decision=narrow` 回执 |
| `rejected` | Catalog 指定的 `bob` | `AWAITING_APPROVAL → ARCHIVED(rejected)` | 无 Grant；`decision=reject` 回执 |

**审批链任务**（目录路由带 `chain`，见 catalog-guide.md）：

- 每一级审批人各发一次 `approved`、`narrowed` 或 `rejected` 回调，各带自己签名的回执。actor 必须是 TaskGate 当前等待的那一级审批人。
- 每级的 `approved_grant` 是上一级回调所带的 Grant，以本级回执时间重新签发：`expires_at` 等于本级 `issued_at` 加 TTL。`approved` 的摘要必须与之相同；`narrowed` 必须是它的严格收缩。第一级的基准是 Manifest。
- 还有剩余环节时，任务仍为 `AWAITING_APPROVAL`，响应形如：

  ```json
  {"ok": true, "granted": false, "pending": true, "next_approver": "sales-director", "next_role": "department_director", "remaining_steps": 2}
  ```

  OA 应把草稿转给 `next_approver`。OA Demo 的做法：
  - 草稿回到 `pending`；
  - 之前的审批人只能查看；
  - 页面列出审批记录与上一级批准的范围。
- 最后一级被接受时，签发授权，任务进入 ACTIVE，响应为 `{"ok": true}`。
- 审批人没有该环节权限时，任务归档，响应为 `{"ok": true, "granted": false, "terminal_reason": "approver_not_authorized"}`。
- 重放同一事件，返回首次保存的响应。

Gateway 会同时校验：

- Header 时间戳在当前时间前后 5 分钟内，签名使用常量时间比较。
- `occurred_at` 也在前后 5 分钟内。
- Header Event ID 与 Body 一致。
- task、draft、Catalog 版本、随机 callback context 与持久化任务一致。
- 回调快照 Hash 与本地 pending 一致，且本地 pending 的字段、范围、预算等重算 Hash 后仍一致。
- actor、审批模式和当前任务状态允许该动作。
- Ed25519 Key ID 已配置，签名有效，回执绑定 Manifest 与最终 Grant 摘要；HMAC 只负责传输认证和防重放。

`event_id + raw body SHA-256` 用于幂等：首次处理锁定回调与任务行，并把审批事件、Grant、预算、状态和 HTTP 响应写入同一个控制 PostgreSQL 事务；验证当前请求签名后，完全相同的已完成事件即使原始事件时间已过窗口或当前 Catalog 已更新，也返回首次保存的响应。复用 Event ID 但改变 Body 返回冲突。Gateway 重启会把单独 Claim 后未完成的事件标记为可重试。

OA Demo 对回调最多尝试 3 次。企业 OA 应采用持久化 Outbox、指数退避、告警和人工对账，不能依赖进程内 goroutine。

## OA Demo 页面与会话

- `GET/POST /login`：Alice/Bob 登录。
- `GET /tasks`、`GET /tasks/{draftID}`：按角色列出和查看草稿。
- `POST /tasks/{draftID}/submit`：仅 Alice，可提交自己的 draft。
- `POST /tasks/{draftID}/decision`：仅 Bob，可批准/拒绝分配给 Bob 的 pending 人工任务。
- 修改操作使用 CSRF Token；会话 Cookie 为签名值、`HttpOnly`、`SameSite=Lax`，有 TLS 时才设置 `Secure`。
- 密码启动时转换为 bcrypt Hash，明文从内存配置结构清除。

OA Demo 的用户与草稿都在内存中，重启即丢失；它只是协议演示，不是生产 OA。

## 数据源接口

Gateway 服务层使用：

```go
type DataConnector interface {
    Query(context.Context, dataconnector.QueryRequest) (dataconnector.Result, error)
    Ping(context.Context) error
    Attestation(context.Context) (dataconnector.Attestation, error)
}
```

Exposure 在线路径另外要求 Connector 实现：

```go
type PairedQueryConnector interface {
    QueryPairStream(context.Context, dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error)
}
```

`QueryPairStream` 必须缓冲可见结果、把 companion rows 逐个交给 `ProvenanceSink`，并让二者观察同一个一致性快照。Sink 即使已经看到前缀，也只能在整个调用成功后使用结果；任何错误都必须丢弃该前缀。
内置 PostgreSQL Connector 在一个显式只读 `REPEATABLE READ` 事务中执行
两者，并分别报告可见与 provenance 数据库耗时/行数而不保留 companion 全量 rows。Connector 不实现该接口时，
exposure Grant 会在执行前关闭式拒绝。

`QueryRequest` 只包含经过 SQL 策略生成的 SQL、单次 `StatementTimeout` 与 `MaxRows`，刻意不提供客户端参数、DSN 或任意 Session 设置入口。结果包含逻辑列名、PostgreSQL类型 OID、二维行、行数、数据库耗时和截断标记。

内置 PostgreSQL Connector 使用 `pgx/v5`：

1. 从 Catalog Source 和 `secretRef` 构造业务库 DSN，建立最多 4 个连接的 Pool，并在启动时 Ping。
2. Connection Runtime Params 固定 `default_transaction_read_only=on`、`search_path=pg_catalog`、最大 `statement_timeout=5s`。
3. 启动、readiness 和每次查询预算预留前校验 `datasource_id`、`current_database()`、`current_user`、PostgreSQL 主版本和 Reporting View Schema 摘要；该摘要覆盖列名、列顺序、PostgreSQL 通用类型和 `pg_get_viewdef` 规范化后的 View 定义。
4. 普通 Query 开启显式 `READ ONLY` 事务；V4 streamed pair 使用同一个 `READ ONLY, REPEATABLE READ` 事务，并用事务本地 `set_config` 缩小超时、重设 `search_path`。
5. Connector 将请求行数限制压到自身 10,000 行硬上限以内；Demo 的任务 Profile 上限更低。
6. 错误对客户端只暴露稳定码，不回显 DSN、原始 SQL 或物理对象；内部 Cause 只供可信日志处理。

PostgreSQL 中的 `gateway_reader` 还有独立权限边界：只授予 `reporting.datasource_attestation`、`reporting.expense_summary` 与 `reporting.expense_detail` 的 `SELECT`，不授予 `legacy.*` 权限，并设置角色级只读和 5 秒超时。

## 新适配器要求

新增 OA Adapter 时：

- 保持 Draft 与回调的关联 ID、Catalog 版本、actor、审批凭证和幂等语义。
- 服务鉴权与回调验签使用独立凭据；不得信任浏览器字段决定敏感级别或预算。
- 回调先持久化再确认，支持重放、乱序、延迟和对账。

新增 Data Connector 时：

- 只接受策略产出的执行计划，禁止直接执行 Agent 输入。
- 独立实施只读事务/账号、Server-side Timeout、行上限和连接池上限。
- 返回稳定且不泄密的错误；不得把凭据、内部对象名或驱动诊断发给 MCP 客户端。
- 提供 Ready Ping 和取消传播；查询结果必须可确定地编码后加密。
- 对 exposure Profile 提供同快照 paired execution，保留两条查询各自的截断和耗时证据；不能生成完整 provenance 时必须拒绝而不是退回估算收费。

当前 SQL 策略使用 PostgreSQL 专用 `pg_query_go/v6` AST 和 PostgreSQL CTE 渲染。支持 MySQL、SQL Server 等不仅是实现 `DataConnector`，还必须新增对应方言的 AST 策略、引用解析、Scope 注入和攻击语料测试。当前服务也只有一个 Connector，尚不支持多 Source 路由或跨源 Join。
