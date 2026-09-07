# FactGate

> **FactGate — task-scoped data gateway for AI agents: release facts, not
> rows, under one approved task budget.**

FactGate is a task-scoped data gateway for AI agents. A signed task grant
permits governed query submission, while results become releasable only after
scope validation, cumulative exposure settlement, and auditable publication.

The paper title is **FactGate: A Task-Scoped Data Gateway for AI Agents with
Cumulative Exposure Accounting**. The research name is FactGate in titles,
prose, and figures; it has no abbreviation.

Evidence-bound identifiers such as `taskgate-*`, `TASKGATE-*`,
`taskgate_ordinal`, `/.well-known/taskgate/...`, and the `bdg` arm label and
`bdg_*` field names inside the sealed evaluation contracts and evidence remain
unchanged: they are digest-pinned internal names of the same system, not the
research name. Any namespace migration will happen once, followed by evidence
regeneration.

## Model

- **Immutable reporting publication:** versioned reporting inventory
  held inside the trusted data environment.
- **FactGate:** validates the task and scope, derives Result,
  Dependency, and Outcome effects, settles cumulative novelty, and controls
  artifact promotion.
- **Exposure Ledger:** the formal persistent root-family ledger used for
  cumulative exposure accounting.
- **Settlement receipt:** evidence that accounting committed together with a
  PENDING publication intent. It is not by itself a release certificate;
  releasability additionally requires verified promotion to `AVAILABLE`.

## Problem

自主数据库 Agent 会自适应地拆分问题、重试、分页并委托子 Agent。传统数据库授权主要判断一次查询是否允许执行；PostgreSQL RLS、VPD、ABAC/XACML 等机制本身并不记录一个任务跨多次合法查询已经累计获得了哪些事实。因而，每次请求都合规并不意味着整段任务执行的累计暴露仍在批准边界内。

FactGate 补充而不取代这些访问控制。它关注的问题是：在一个经人工批准的任务及其全部委托后代中，新查询带来的累计数据暴露是否仍然可接受。MCP、工具调用和 HTTP 路由只是当前原型承载这一机制的接口，不是研究贡献本身。

## Key Insight

访问许可不足以表达累计暴露边界，还需要跨查询的 Exposure Accounting。FactGate 把自适应查询、重试、重叠分页和子 Agent 统一绑定到一个 root-family Exposure Ledger；一次执行只为此前未计量的新事实收费。相同事实的规范重放不重复收费，不同规范命题即使都返回空集或 `0`，仍产生不同的 outcome exposure。

Agent 先声明数据产品、字段、Scope 和目的。FactGate Enforcement Layer 从 Catalog 绑定完整预算 Profile，OA 人工批准后才激活任务。正常批准会把 Profile 的完整三维容量交给 Agent；系统不自动寻找“最小预算”，也不把未用额度作为优化目标。准入条件是本次增量提交后，共享 root-family ledger 的每个维度都不超过人工签名边界。

## FactGate Model

人工审批任务表示为 `T=(P,S,B,C)`：

- `P`：绑定 principal、数据产品、字段和 Scope 的获批策略/Grant；
- `S`：任务绑定的不可变 reporting publication；
- `B`：release、positive-output dependency 和 query-outcome 三维 exposure 预算；
- `C`：签名的语义与执行约束。

上游业务数据可以继续变化；已批准任务保持绑定原 publication，新任务可绑定新的已发布版本。当前模型面向版本化、只读 reporting snapshots，不面向 mutable OLTP/CDC serving。

数据库事实至少绑定：

```text
F = (product, snapshot, entity key, field, value version)
```

对任务的已接受查询序列 `Q`，`E(T,Q)` 是三类事实集合的累计大小：

- `release exposure`：实际进入交付结果的事实；
- `positive-output dependency footprint`：按声明的代数规则参与正向输出推导的保守依赖足迹；
- `query-outcome exposure`：规范化 QueryPlan 命题及其发布结果摘要。

API、数据库和回执为兼容性保留字段名 `influence`；它表示 positive-output dependency footprint，不表示最小 causal influence 或完整 physical read set。完整定义、前提和安全性质见[FactGate 形式模型](docs/formal-model.md)和[任务级 Exposure Accounting](docs/exposure-accounting.md)。

## Architecture

```text
Versioned Reporting Snapshot + Candidate Catalog
                 │ offline publication compile
                 ├── Business PostgreSQL ordinal sidecar
                 └── HOT hash/ordinal + COLD payload ── publication manifest
                                                        │
Autonomous Agent ──► FactGate Enforcement Layer ────────┤
                         │ authorize / semantic replay
                         │ miss: visible SQL + ordinal companion
                         ▼
                  exact weighted bitmap effect
                         ▼
FactGate control path: bitmap ANDNOT + popcount + exact union
                         ▼
Control PostgreSQL: persist sets + one R/D/O root-head CAS
                         ▼
            encrypted Parquet staging + audit + V6 receipt
                         ▼
S3/MinIO: deterministic canonical object promotion
                         ▼
                 consumed/AVAILABLE → result_id
```

V4 将 canonical FactID 精确编码为不可变 snapshot 的 ordinal bitmap；少量 derived release/outcome facts 使用动态字典。可见结果和 ordinal provenance companion 在同一个只读 `REPEATABLE READ` 事务中执行。Control PostgreSQL 保存 ledger、artifact 元数据、审计和签名回执，不保存 Parquet 或结果行；Parquet 在 FactGate Enforcement Layer 客户端侧加密后写入私有对象存储。

两个 PostgreSQL 使用独立容器、账号和 Volume。S3 兼容对象存储使用独立内部网络、bucket-scoped 执行层凭据和持久 Volume；result bucket 必须私有且禁用 versioning，确保 TTL/purge 删除实际 bytes，而不只是写入 delete marker。

## Exposure Ledger

根任务维护三个单调集合 `K_release`、`K_dependency` 和 `K_outcome`。查询 `q` 的收费向量只包含相对于共享账本的新事实：

```text
delta(T, q) = (|F_release(q)    - K_release(T)|,
               |F_dependency(q) - K_dependency(T)|,
               |F_outcome(q)    - K_outcome(T)|)
```

物理路径用 exact bitmap `ANDNOT`、`OR` 和 `popcount` 实现集合差、并集和基数。所有子 Agent 解析到同一 root head；一次三维 root-head CAS 原子发布三个维度，冲突后读取新 head 并重新计算，不能分维花费。任一维度超过预算时，整次结算关闭式拒绝，结果不会成为 canonical artifact。

确定性的 canonical 对象创建成功即记为 `consumed/AVAILABLE`。普通 Agent 随后只得到 `result_id` 和摘要；下载是否发生、下载次数以及 Agent Host 的临时副本都不会改变 ledger、`consumed` 状态或 receipt。在线顺序是：

```text
reserve
  → replay lookup or execute-and-stream
  → derive exact bitmap effect
  → stage encrypted Parquet
  → three-head CAS
  → canonical promotion
```

## Enforcement

FactGate defines a controlled analytical SQL profile; it does not claim support for full SQL. `taskgate-reporting-sql-v1` intentionally excludes constructs that cannot be compiled within the declared accounting semantics, reducing semantic ambiguity, preventing exposure-accounting bypass, and preserving deterministic compilation to canonical QueryPlan.

### Agent Task Execution workflow

当前 Demo 通过 MCP 2.0 transport 暴露以下方法；这些方法名为兼容 API，不定义 FactGate 的研究边界。

1. 调用 `list_data_products`、`describe_data_product` 和 `get_sql_capabilities`，读取数据产品、字段、稳定角色、Scope 及受控 SQL profile。
2. 调用 `request_data_task`，提交非空 `objective`、`data_products`、各产品的非空 `columns` 和 `scopes`。FactGate Enforcement Layer 按最高敏感级别选择 Catalog Profile，并把完整 Profile 写入审批 Manifest；Agent 不选择或优化预算。
3. 在 OA 提交并完成人工审批。
4. 任务进入 `ACTIVE` 后调用 `query_sql(task_id, request_id, sql)`。Exposure-enabled Grant 只接受可无损 lowering 为 canonical QueryPlan 的分析查询，实际执行的 visible SQL 和 provenance companion 都从该计划重新生成。
5. 子任务通过 `parent_task_id` 和 `delegate_principal_id` 创建；授权维度只能收缩，并与 root task 共享 Exposure Ledger。

客户端生成的 `request_id` 在一个任务内必须唯一。相同 ID 和相同请求只观察首次持久化终态；相同 ID 搭配不同请求会关闭式拒绝。新的 request ID 重放相同规范命题和结果时，三维增量为零。语法、授权或 lowering 失败发生在业务数据库执行和正式 reservation 前，不产生 exposure charge。

最小分析查询示例：

```sql
SELECT month, SUM(total_amount) AS amount
FROM expense_summary
GROUP BY month
ORDER BY month ASC
LIMIT 20;
```

`taskgate-reporting-sql-v1` 包含单产品 projection/filter/order/limit/offset、`COUNT(*)`、`COUNT(column)`、`SUM`、`MIN`、`MAX`，以及由 2–16 个不同 Catalog 稳定角色构成的 connected INNER equi-join graph。每条 edge 可以包含一个或多个 column-to-column equality predicate。16-source 上限是限制生成 SQL 宽度、provenance 行数和 PostgreSQL planning work 的 operational complexity/DoS ceiling；请求还受 1 MiB transport 请求体、PostgreSQL AST 白名单校验、资源预算、超时和行数上限约束。

多产品 `ORDER BY` 只接受带非空 `GROUP BY` 的上述 Join：所有 group key 都必须直接投影，并各自在排序列表中恰好出现一次，方向只允许 `ASC`/`DESC`（省略等价于 `ASC`）；多产品 `LIMIT/OFFSET` 始终拒绝。Visible SQL 采用请求的 group-key 顺序，ordinal companion 独立保持 canonical group/entity 升序。顶层 projection cast 只接受未修饰、非嵌套的 `bigint`/`int8`、`numeric`、`text`；可证明与自然 canonical 类型相同的 identity cast 会被消除。唯一非 identity 形式是同一完整排序 grouped Join 中自然结果为 `numeric` 的精确 `SUM` 以 `postgresql-numeric-text-v1` 产生 PostgreSQL wire `text`；aggregate identity 和记账类型仍为 `numeric`，无 `ORDER BY` 时不得使用。

Full SQL intentionally remains unsupported. Self-join、outer/cross/non-equality/`NATURAL`/`USING` join、断开的 join graph、子查询、CTE、set operation、窗口、`HAVING`、位置式 group/order、显式 `NULLS FIRST/LAST` 和 `ORDER BY USING` 均在该 profile 外。多产品未分组、部分/重复/未投影 group key、aggregate/表达式排序、全部分页、未分组或 Union result encoding 及其它 projection cast 也关闭式拒绝；执行层不会把 `LEFT JOIN` 静默改为 `INNER JOIN`。Resource-only Grant 接受同一个 profile：它与 exposure-enabled 的区别在记账，不在 SQL 接受面。它以前直接执行 Agent 原文，那条路径已经取消——每个 COMPLETED 查询都必须签署它所执行语句的 preparation，而无法 lowering 的语句没有 canonical plan 可供独立重建。

普通 Agent 的 `tools/list` 不默认列出 `execute_plan`。该入口仅保留给 SDK、内部测试、基准、调试和确定性工作流，并与 SQL lowering 共用同一 QueryPlan 编译和 Exposure Accounting 边界，不是策略旁路。

### Result enforcement and delivery

成功响应中的 `exposure` 区分本次 actual facts 与相对 root ledger 的 charged facts；`exposure_budget` 返回上限、已用和剩余值。`query_sql`、`execute_plan` 和 `get_query_result` 的普通响应不包含 `rows` 或对象键，而是返回 `result_id`、列定义、`row_count`、`column_count`、`expires_at` 和计费摘要。

少量检查使用 `preview_result(result_id, offset, limit)`，其中 `limit` 最大为 100。完整文件通过 `deliver_result(result_id, format="parquet")` 获取默认 5 分钟的短期下载 URL。非 loopback 的 `GATEWAY_PUBLIC_BASE_URL` 必须使用 HTTPS；日志和 APM 必须脱敏 capability URL 中的 `token`。

### Task-scoped Nested View DAG

声明 `taskgate-view-contract-v1` 的 Product 可作为 semantic root。执行层只为本次任务请求的 roots 在 PostgreSQL `REPEATABLE READ` snapshot 中发现传递依赖：ordinary View 递归展开；已治理、已填充的 materialized view 是映射到 Catalog Product 的 opaque terminal publication；raw、foreign、system relation 以及递归或循环依赖关闭式拒绝。

可接受 closure 必须能 flatten 为现有 canonical SPJG/`join_many` QueryPlan：direct projection/rename、column-to-literal `AND` filter、connected INNER equi-join、`GROUP BY` 及 `COUNT/SUM/MIN/MAX`。约束包括最多一道 aggregate barrier、16 个 expanded base sources、View depth 16、reachable nodes 64、dependency edges 128 和 1 MiB closure definition bytes。这是受控语法范围，不是任意 SQL View 支持。

当前 query-time 边界要求 semantic root 是唯一外层数据产品，不能再与另一 Product join；其上方暂不接受 `ORDER BY`/`LIMIT`/`OFFSET`。跨过 aggregate barrier 后，外层只能纯投影已计算 public outputs。Catalog 分别固定 exact definition、transitive dependency、typed canonical plan 和 ordered output interface 四个摘要。发现 drift 后，任务单向进入 `REQUIRE_REBIND`；恢复访问需要基于更新 Catalog 新建任务并重新审批，旧 Grant 不会原地改绑。

高级 `execute_plan` 使用 Catalog 稳定角色而不是输入 SQL alias。例如：

```json
{
  "from": {
    "join_many": {
      "sources": [
        {"product": "expense_detail", "role": "expense_detail"},
        {"product": "expense_summary", "role": "expense_summary"}
      ],
      "on": [
        {"left": "expense_detail.department", "right": "expense_summary.department"}
      ]
    }
  },
  "columns": [
    "expense_detail.receipt_no",
    "expense_summary.total_amount"
  ]
}
```

## Evaluation

仓库提供以下可复现入口：

```bash
make verify
make formal
make eval-exposure
make eval-smoke
make paper
make logs
```

`make verify` 执行格式检查、`go vet`、真实 PostgreSQL `go test -race ./...`、镜像构建和隔离 Compose 端到端验收。`make formal` 检查抽象 ledger 与 bitmap refinement artifacts。

`make paper` 以草稿模式校验已归档 evidence（schema-3 源文件、Catalog 和 evidence-tooling 从其记录的历史 submission commit Git blobs 重算）、生成对应 LaTeX 宏并编译 substantially revised TKDE working manuscript，不执行实验，也不要求当前草稿源码冻结。如需显式刷新 exposure evidence，可运行 `make paper-refresh-exposure`；该入口会调用 `evaluation/run-exposure.sh`，且不属于默认论文构建依赖。定稿并审阅 evidence 后，使用独立的 `make paper-final-check` 要求工作树已 clean、当前 measured paths 与记录的 submission commit 一致、生成宏与 `HEAD` 一致，然后以 final 模式编译；它不刷新实验。同一作者的早期预印本 SessionBound（arXiv:2607.00751v1，从未投稿、未被接收）不在本仓库构建；本稿以真名单匿名投稿，并以新 arXiv 编号取代该预印本。

`make eval-exposure` 覆盖 ground-truth FactID、独立 oracle、split/merge、overlapping pagination、retry、join multiplicity、snapshot update 和 anti-arbitrage cases。它还执行 1,024 个唯一规范化 PostgreSQL baseline/rewrite SQL pair；这些 pair 是补充性等价改写压力测试，不应表述为 1,024 个独立数据集或 exposure invariance 的单独证明。

`evaluation/exposure-performance/results.json` 汇总三次 fresh-stack 本地 trial 的 31,296 个 RQ4 observations，其中 7,896 个是 full-path operations，23,400 个是 direct/paired snapshot/paired-plus-algebra ablations。该 campaign 使用仓库内十行 fixture，不是 TPC、多节点或生产规模结果。

当前 evaluation 还包括同 root delegation/concurrent settlement tests、受控 View compiler properties，以及版本化 daily publication harness。攻击相关 evidence 证明已实现的确定性 split/merge、重叠分页、重试和 outcome probing cases 会进入同一累计 ledger；仓库尚未报告由 live LLM 驱动、贯穿全新 roots 并与 authorization-only baseline 对照的完整端到端攻击 campaign。实验范围和待补项目见[TKDE 实验执行指南](docs/experiment-guide.md)。

本轮新增的可执行方法文档分别定义了[基线比较](docs/evaluation-baseline.md)、
[自适应 Agent 攻击](docs/adversarial-agent-evaluation.md)、
[多 Agent 共享账本](docs/multi-agent-evaluation.md)和
[10K–100M 性能矩阵](docs/performance-evaluation.md)。其中空表表示尚待作者在固定
环境执行的实验，不是零值或已测结果。

## Limitations

- Exposure Ledger 是任务及其委托后代的计量状态，不是主体的完整知识状态或总体隐私损失；系统没有跨独立 root approvals 的 principal/tenant 全局 ledger。
- Positive-output dependency footprint 只在声明的受控代数内成立，不是最小 causal provenance，也不覆盖全部 physical reads、负信息或排序位置泄露。
- 一个 outcome 单位不等于一个信息 bit；背景知识、timing 和预算接受/拒绝造成的推断不进入当前模型。
- controlled analytical SQL profile 和 bounded Nested View DAG 有意不覆盖 full SQL、任意 SQL provenance、mutable OLTP/CDC serving 或跨引擎等价。
- 属性测试是限定语法内的实现证据，不是任意 SQL 语义保持的 mechanized proof；当前公开性能数据也不能外推为生产 SLO。

这些边界以及与数据库 provenance 系统的区别见[provenance comparison](docs/provenance-comparison.md)和[threat model](docs/threat-model.md)。

## Production Gap

当前仓库是单实例 research demo，不是可直接上线或安全横向扩容的生产执行层。默认 deployment 每个 publication epoch 只有一个 FactGate Enforcement Layer 实例；PostgreSQL 行锁和 root-head CAS 处理该实例内的并发，但尚无 multi-instance execution lease 或等价的分布式 settlement protocol。

Connector/visible-result-to-Parquet 路径仍可能在内存持有完整结果，且 `preview_result` 默认拒绝大于 64 MiB 的 artifact。百万行生产使用前需要有界 streaming Parquet writer/reader、固定环境容量基准、外部 KMS/HSM/Secret Manager、严格 publication retention/routing、独立 WORM audit service 和运维监控。提高 `GATEWAY_CONNECTOR_MAX_ROWS` 不是有界内存证明。完整生产差距见[威胁模型与生产化差距](docs/threat-model.md)。

## Operational usage

### Quick start

宿主机需要 Docker Engine 与 Docker Compose v2：

```bash
cp .env.example .env
# 按 docs/getting-started.md 生成加密密钥和两个独立 Ed25519 密钥
# 同时替换 .env 中的全部 Token、共享密钥、对象存储和数据库密码
docker compose up --build -d --wait
docker compose ps
curl -i http://127.0.0.1:8082/health/ready
curl -i http://127.0.0.1:8092/health/ready
```

Compose 和现有 `gateway` 二进制通过兼容环境变量 `GATEWAY_CONNECTOR_MAX_ROWS` 默认设置 `1200000`，使 345,000-row provenance V4 maximum-point workload 不会被 connector ceiling 截断。降低该值是部署取舍；超限时系统关闭式拒绝，不释放部分结果。

| Service | Address |
|---|---|
| FactGate Enforcement Layer（MCP transport） | `http://127.0.0.1:8082/mcp` |
| OA Demo | `http://127.0.0.1:8092/login` |
| Control PostgreSQL | `127.0.0.1:25433` / `taskbound_gateway` |
| Business PostgreSQL | 仅内部网络 / `travel_demo` |
| Parquet object storage | 仅 `result-storage` 内部网络 / `taskgate-results` |

如需本机数据库客户端调试，可显式启用非论文部署覆盖：

```bash
docker compose -f compose.yaml -f compose.debug.yaml up --build -d --wait
```

Navicat 凭据见[本地启动与数据库调试](docs/getting-started.md#navicat-%E8%BF%9E%E6%8E%A5%E5%8F%82%E6%95%B0)。Business PostgreSQL 与 MinIO API/Console 只在该 debug override 中绑定宿主机回环地址。

### Demo identities

| Identity | Interface | Permission | Credential source |
|---|---|---|---|
| Alice | Agent API（MCP transport） | 申请任务、查询自己的 `ACTIVE` 任务、读取结果元数据、有界预览、短期交付和 receipt | `TASKBOUND_ALICE_TOKEN` |
| Carol | Agent API（MCP transport） | 读取 audit events 和 query receipts，不能读取原始结果 | `TASKBOUND_CAROL_TOKEN` |
| Alice | OA | 提交自己的 OA draft | `alice` / `OA_ALICE_PASSWORD` |
| Bob | OA | 审批人工任务 | `bob` / `OA_BOB_PASSWORD` |

### Retention and recovery notes

新 `result_artifacts` 行只保存 artifact 元数据；客户端侧加密 Parquet 保存在 S3/MinIO。Control transaction 先登记 `PENDING`，执行层再幂等提升 staging object 并标记 `AVAILABLE/consumed`。恢复只在 staging 与已提交 evidence 一致时继续 promotion，不重新执行 SQL 或重复收费；staging 丢失或 canonical evidence 冲突时 readiness 关闭式失败，需要受审修复。

`GATEWAY_RESULT_RETENTION_TTL` 触发先删除过期 canonical bytes、再写 metadata tombstone，同时保留 query record、receipt 和 audit evidence。每个 artifact 绑定 `GATEWAY_DATA_KEY_ID`；Demo 的 key-ID erasure 不会销毁外部 KMS 中的真实 key material。Legal hold 阻止 TTL/manual retention cleanup，但不延长 `expires_at`，也不会自动阻止独立 key-erasure 流程。

Receipt verifier 可读取 `/.well-known/taskgate/query-receipt-keyring.json`。`GATEWAY_AUDIT_ANCHOR_URL` 可将签名 audit-chain checkpoint POST 到外部日志或 WORM service；其保留和不可篡改性由部署环境保证。

`docker compose down` 保留 `control-pg-data`、`business-pg-data`、`snapshot-index-artifacts`、`gateway-encrypted-spool` 和 `result-object-data`。`docker compose down --volumes` 会删除当前 Compose 项目的这五个 Volume。旧版 `gateway-data` Volume 不再挂载，也不会自动删除。

### Documentation

- [累计数据暴露模型](docs/exposure-model.md)
- [版本化 Publication 与每日同步](docs/versioned-publication.md)
- [数据库安全控制基线评测方法](docs/evaluation-baseline.md)
- [自适应 Agent 攻击评测方法](docs/adversarial-agent-evaluation.md)
- [多 Agent 共享账本评测方法](docs/multi-agent-evaluation.md)
- [10K–100M 性能评测方法](docs/performance-evaluation.md)
- [FactGate 形式模型](docs/formal-model.md)
- [FactGate 与数据库 provenance 系统的边界](docs/provenance-comparison.md)
- [TKDE 实验执行指南](docs/experiment-guide.md)
- [FactGate V4: Snapshot-Indexed Hybrid Bitmap Ledger](docs/exposure-v4.md)
- [FactGate V5: Predicate Atom Footprint 与 Composite Outcome](docs/exposure-v5.md)
- [架构与安全边界](docs/architecture.md)
- [任务级 Exposure 语义、在线算法与支持边界](docs/exposure-accounting.md)
- [Compose 启动、Navicat 与 Agent API 演示](docs/getting-started.md)
- [Catalog 编写指南](docs/catalog-guide.md)
- [OA 与数据源适配器接口](docs/adapters.md)
- [受控 SQL profile 与 QueryPlan 安全规则](docs/sql-security.md)
- [威胁模型与生产化差距](docs/threat-model.md)
- [本轮 TKDE 改造实施计划](docs/codex_taskgate_tkde_revision_plan.md)
