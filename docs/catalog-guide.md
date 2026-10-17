# Catalog 编写指南

`config/catalog.yaml` 是 Gateway 的可信、版本化逻辑数据契约。没有
`view_contract` 的 Product 沿用 source-level Schema Attestation：Gateway 在启动、
readiness 和每个新查询前核对其 Reporting View 的定义、列顺序、类型与 collation。
声明 `view_contract` 的 semantic root 明确排除在该全局 digest 之外，改由任务申请、
OA 激活和每个新查询的 task-scoped transitive View binding 校验；这样一个无关
semantic View 的替换不会使全部任务一起失效。两条路径都关闭式拒绝漂移。目录仍
不是数据库元数据缓存，Reporting View、terminal publication 和只读角色权限必须由
管理员作为一个受审发布单元维护。

## 顶层结构

Catalog 使用严格 YAML 解码，只允许一个文档，未知字段会导致启动失败。V4 顶层结构包含冻结发布物：

```yaml
catalog_version: "2026-07-21.2"
sources: []
snapshot_publications: []
scopes: []
products: []
approval_routes: []
budget_profiles: []
```

`catalog_version` 最长 128 字符，可包含字母、数字、点、下划线和连字符。任何会影响查询含义或授权的变更都应提升版本。ACTIVE 任务记录创建时的版本；当前运行 Catalog 版本不一致时，Gateway 会关闭式拒绝查询，防止配置热变更扩权。

## Source

当前只支持 `postgres`/`postgresql`：

```yaml
sources:
  - name: travel_demo
    datasource_id: taskgate-demo-travel
    type: postgres
    address: business-postgres
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    schema_digest: 02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc
    secretRef: GATEWAY_DB_PASSWORD
```

规则：

- `name` 是小写逻辑标识符：`[a-z_][a-z0-9_]*`。
- `datasource_id` 是稳定小写数据源身份，会由业务库 `reporting.datasource_attestation` 证明，并写入 Grant 与查询 Receipt。
- `address` 只写主机，不得包含空白、路径、`@` 或凭据。
- `port` 必须为 1–65535；数据库名和用户名必须是简单标识符。
- `postgres_major_version` 必须匹配运行时业务 PostgreSQL 主版本。
- `schema_digest` 是 Gateway 启动时要求的 legacy/terminal Reporting View attestation 摘要，包含列顺序、通用 PostgreSQL 类型、collation evidence 和 PostgreSQL View 定义。带 `view_contract` 的 semantic roots 不进入这个 source-wide 摘要，而由下文的四摘要 contract 与 task binding 管理；不要把两者混成一个全库失效开关。
- `secretRef` 必须引用大写环境变量，可写 `GATEWAY_DB_PASSWORD` 或 `env:GATEWAY_DB_PASSWORD`。
- `password`、`passwd`、`pwd`、`dsn`、带 `user:password@host` 的地址等明文秘密会被语法树预检拒绝，错误不会回显秘密。

当前 Gateway 只实例化一个业务 Connector，因此同一任务的产品必须来自同一 `sources[]`。启动时 Gateway 按该 Source 和 `secretRef` 构造业务 DSN，并校验 `datasource_id`、数据库名、角色、PostgreSQL 主版本和 Reporting View Schema 摘要。

## Snapshot Publication（V4）

```yaml
snapshot_publications:
  - name: expense-summary-v1
    source: travel_demo
    source_namespace: travel.expense_summary
    snapshot: travel-demo-2026-v1
    ordinal_sidecar: taskgate_ordinal.expense_summary_v1
    sidecar_digest: <64-hex>
    dictionary_digest: <64-hex>
    manifest_digest: <64-hex>
```

Publication 是实际冻结的数据发布物，不是给可变表添加的标签。可发布的 compiler input 应声明受限的 `source_relation: reporting.<name>`；`cmd/snapshot-index` 从 `SNAPSHOT_POSTGRES_DSN` 使用只读角色，在同一 `REPEATABLE READ` 事务中验证 populated materialized view、NOLOGIN owner、每个唯一物理字段的 PostgreSQL type/collation/version，并以数据库 rows 覆盖 JSON candidate rows 后构建 sidecar、HOT/COLD dictionary 和 bundle。stdout 给出应写回 Candidate Catalog 的 digest；不要手工猜测 digest。Product 的 `source`、`reporting_view`、`fact_namespace`、`snapshot` 必须与 publication 构建来源及其 `source`、`source_namespace`、`snapshot` 一致。

Catalog 声明 publication 时，Gateway 必须配置 `GATEWAY_SNAPSHOT_ARTIFACT_DIR` 并严格激活全部 publication：目录/文件不可为 symlink，bundle 必须唯一，Catalog 与 artifact 的 schema/snapshot/dictionary/manifest/sidecar digest 必须一致，HOT 总量不得超过配置上限；sidecar 会逐行对照 HOT，COLD 会在只读 publication mount 上全文件流式核 manifest envelope、seal、长度和 SHA-256。COLD 的逐 Fact semantic roots 由发布前 builder/audit verifier 建立，bundle descriptor 自身不是信任根。任一失败都会阻止 Gateway 启动；详情见 [TaskGate V4](exposure-v4.md)。

只要 Catalog 声明 ordinal publication，全部可达 route 必须使用同一个 ordinal Profile；默认是 `taskgate-exposure-v5`，不能与 V2/V3、V4 或未启用 exposure 的 route 混合。成功激活会在 Control PG 留下单向 deployment marker。之后可以发布新的 V5 Catalog，但不能通过删除 publication、改回 legacy Profile 或换旧 Gateway binary 降级；这类启动或数据库写入会关闭式失败。

## Scope

Scope 定义可申请范围及其上界。当前支持枚举和日期范围：

```yaml
scopes:
  - name: department
    type: enum
    description: 允许访问的部门
    allowed_values: [销售部, 研发部, 财务部]

  - name: expense_date
    type: date_range
    description: 报销发生日期范围
    min: "2026-01-01"
    max: "2026-12-31"
```

- `enum` 必须提供非空、唯一的 `allowed_values`，不能提供 `min/max`。
- `date_range` 必须提供合法的 `YYYY-MM-DD` 最小/最大日期，不能提供 `allowed_values`。
- 某产品列入 `scopes` 的每个名称都是强制范围，并且必须同时是该产品发布的字段。TaskIntent 缺任一强制范围时，任务申请失败，而不是退化为无范围授权。
- 当前 Demo 的两个产品都强制 `department`；`expense_date` 是未绑定产品的扩展示例。

## Product

一个逻辑产品映射一个由管理员维护的 `reporting.*` View：

```yaml
products:
  - name: expense_summary
    source: travel_demo
    reporting_view: reporting.expense_summary
    description: 按月份、部门和费用类型汇总的差旅报销数据
    sensitivity: low
    snapshot: travel-demo-2026-v1
    snapshot_publication: expense-summary-v1
    entity_key: [month, department, expense_type]
    fact_namespace: travel.expense_summary
    stable_relation_role: expense_summary
    scopes: [department]
    allowed_functions: [date_trunc, to_char]
    allowed_operators: ["=", "<>", "<", "<=", ">", ">=", "+", "-", "*", "/"]
    allowed_aggregates: [sum, count, min, max]
    fields:
      - name: month
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 月份，格式 YYYY-MM
      - name: department
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 部门
      - name: total_amount
        type: numeric
        description: 总金额
```

规则与建议：

- `name`、字段名必须为小写逻辑标识符，且不得重复。
- `source` 必须存在。`reporting_view` 必须严格匹配未加引号的 `reporting.<lowercase_name>`，同一 View 不能发布两次。
- 必须提供描述、至少一个字段和至少一个强制 Scope；每个 Scope 必须对应发布字段。
- `snapshot` 是稳定、非空的数据发布版本。V4 terminal Product 必须用 `snapshot_publication` 引用实际冻结、manifest-proven 的发布物；可展开 semantic root 改用 `view_contract`，且二者互斥。它不是自动采集的 MVCC transaction ID。
- V2 产品必须同时提供 `fact_namespace` 与 `stable_relation_role`。所有 `text`/`character`/`character varying` 字段还必须固定数据库实际报告的 deterministic collation 名称及版本；Gateway 会在每次 schema 证明中核对名称、版本和 `collisdeterministic`。数据库 libc/ICU 升级后应先提升 Catalog 版本并重新审批，而不是静默接受排序语义漂移。
- `entity_key` 必须是非空、不重复的已发布字段列表，并在该 snapshot 内唯一、稳定。Gateway 会在 exposure 路径内部读取这些字段用于 FactID，但不会把未获批的 key 字段返回客户端。
- 支持的敏感级别从低到高为 `public`、`low`、`medium`、`high`、`restricted`。
- 产品有效敏感级别取产品级与所有字段级标记的最高值。审批路由按有效级别选择，不能通过给产品标低级而降低高敏字段。
- 字段 `type` 必须是受支持的 PostgreSQL `information_schema.columns.data_type` 通用类型；Gateway 会逐列核对名称、顺序和类型。Catalog V1 不声称核对 `numeric` 精度/小数位、字符长度、Domain 或数组元素类型，因此拒绝 `numeric(10,2)` 等带修饰符写法；需要该边界时应在 Reporting View 中选用独立通用类型并提升协议版本。
- Reporting View 应只暴露业务所需字段。手机号、工资、银行卡等即使“不写入 Catalog”，若仍出现在 View 中，也不应向 `gateway_reader` 暴露。
- 函数和聚合必须是未限定的小写名，且不能是文件、网络、管理、睡眠等禁止函数；运行时还会经过 SQL AST 的危险函数硬拒绝表。
- Exposure V2 只允许 `count`、`sum`、`min`、`max`；`avg` 即使在传统 SQL allowlist 中存在，也不能进入 V2 计划。
- 运算符必须来自安全词汇表。允许列表是上限，不会使 AST 原本不支持的 SQL 特性变为可用。

## Eligibility（资格先于审批）

`eligibility` 是 Product 上可选的资格谓词：Gateway 在 `request_data_task` 创建 OA 草稿**之前**，用申请人（根任务的
Principal subject；委托任务的父 Grant 人类主体）经身份系统证明的属性把它编译成一个"可申请包络"。申请落在包络外立即以
`NOT_ELIGIBLE` 拒绝，不进入审批队列、不建任务、不设例外路由；审批人只能在包络内收窄。资格裁决是机器裁决，人工审批
是人的决定，两者分开记录。

```yaml
products:
  - name: expense_detail
    # ...
    scopes: [department]
    eligibility:
      require:
        - attribute: job_family
          any_of: [hr, finance, finance_controller]
      scope_envelope:
        department:
          from_attribute: department
          widen_if:
            - attribute: job_family
              any_of: [finance_controller]
```

- `require` 列出必须全部成立的条件；一条条件成立当且仅当申请人在评估时刻对该属性至少持有 `any_of` 中的一个值。
- `scope_envelope` 只能约束本产品的强制 enum Scope：可申请值默认是申请人 `from_attribute` 的当前值；任一 `widen_if`
  条件成立时放宽到该 Scope 的全部 `allowed_values`。申请值必须是可申请值的子集，部分越界也算越界。
- `require` 与 `scope_envelope` 至少声明一项；属性名是小写标识符，值是规范 token；未知字段按 Catalog 严格解码拒绝。
- 属性来自部署挂载的快照文件（`PRINCIPAL_ATTRIBUTES_PATH`，格式 `taskgate-principal-attributes-v1`，见
  `config/principal-attributes.yaml`）：standing attributes 加带 RFC 3339 `[valid_from, valid_until)` 窗口的临时授权
  `grants`；窗口外的临时授权不参与评估。快照是身份/HR 系统的输入，Gateway 只校验形状并把其 SHA-256 绑定进裁决，不判断
  属性是否正确。
- 失败关闭：Catalog 声明了 `eligibility` 而未配置快照时 Gateway 拒绝启动；申请人不在快照中按不具资格处理。
- 裁决记录（版本 `taskgate-eligibility-v1`：快照摘要、主体、评估时刻、逐产品的 `require` 命中值与各 Scope 的
  `source=attribute|widened`、`admissible`、`requested`）写入任务的私有 pending context，可在任务记录中复核；签名的
  `AuthorizationManifestV1` 不改版本、不新增字段。

## Semantic View Contract（Phase B）

普通 Product 可以作为 governed terminal publication；若一个 Product 的
`reporting_view` 是需要递归展开的 semantic root，则增加：

```yaml
    view_contract:
      profile_version: taskgate-view-contract-v1
      definition_digest: <64-lowercase-hex>
      dependency_digest: <64-lowercase-hex>
      canonical_plan_digest: <64-lowercase-hex>
      interface_digest: <64-lowercase-hex>
```

四个摘要必须由同一个只读 `REPEATABLE READ` PostgreSQL registry snapshot 和同一
版 compiler 共同生成，不能从 SQL 文本手工拼装：

| 字段 | 内容 |
|---|---|
| `definition_digest` | semantic root 的 exact `pg_get_viewdef` bytes；保留 SQL literal 内空白等有意义差异 |
| `dependency_digest` | schema-qualified transitive closure、依赖拓扑、各节点 definition 与 ordered interface |
| `canonical_plan_digest` | 展开后 typed canonical SPJG/JoinMany algebra；不包含 PostgreSQL OID、输入 alias 或中间 View 名 |
| `interface_digest` | root 的有序 output name、canonical type、collation 及 version |

Catalog 加载会检查 profile 和摘要形状；任务申请时才从实时 PostgreSQL 重新发现
本次请求的 roots、编译 closure，并要求四者和 Product `fields` 同时相等。即使一次
无关格式改动没有改变 `canonical_plan_digest`，exact definition/dependency evidence
仍可能变化并要求重新发布、重新审批；这是保守的 revision governance，而不是把
SQL 文本哈希冒充 relational semantic digest。

候选 Catalog 写好 root Product（首次生成时尚可没有 `view_contract`）后，用只读业务
库凭据生成待审核证据：

```bash
go run ./evaluation/cmd/view-contract \
  -catalog ./config/catalog.candidate.yaml \
  -dsn "$TASKGATE_VIEW_CONTRACT_DSN" \
  -products customer_value
```

空 `-products` 会重新生成已有 `view_contract` 的 products；显式列表用于首次生成。
命令只向 stdout 输出 registry revision、四摘要和依赖闭包，不修改 Catalog。管理员应
把输出审入新的版本化 candidate artifact，并在同一发布流程中验证后再激活；不要把
DSN 或输出中的物理依赖名交给 Agent。

### 可展开边界

- 只有 schema-qualified、未加引号的小写普通 View 会递归展开。已治理、已填充的 materialized view 必须对应一个没有 `view_contract` 的 Catalog Product，并作为 opaque terminal；其 seed tables 不进入在线 DAG。
- 所有 terminal 必须来自同一 Source 的 governed Product。裸表、partitioned/foreign/system relation、临时或 unlogged relation、递归/循环依赖以及 relation function 都拒绝。
- 每个 View 只允许 direct projection/rename、column-to-literal `AND` filters、显式 `INNER JOIN ... ON` 的 column equality conjunction、`GROUP BY` 和 `COUNT/SUM/MIN/MAX`。多 equality predicates 和任意 connected JoinGraph 形状都可转换为现有 `join_many`；递归层数不限制为两层，但必须位于下述 depth ceiling 内。
- 一个 closure 最多一道 aggregate barrier；聚合输出之上的 View 只能继续做单输入 direct projection/rename，不能 filter、join 或再次 aggregate。CTE/subquery、set operation、`DISTINCT`、outer/cross/natural/`USING`/non-equality Join、scalar function/cast、window、`HAVING`、`ORDER BY`/pagination 均不在 profile 内。
- Expanded governed sources 最多 16 个；View depth 最多 16、reachable relation nodes 最多 64、dependency edges 最多 128、reachable definition bytes 合计最多 1 MiB。这些是独立的复杂度/DoS ceilings，不是“支持任意深度或任意 SQL”的声明。
- Shared child 在 discovery DAG 中只记录一次，但编译按每次 occurrence 展开以保持 bag semantics；若因此重复同一 Product 或 stable role，则作为 unsupported self-join 关闭式拒绝。

### 查询时展开与 Scope

`view_contract` 不只是漂移摘要。查询一个 semantic root 时，Gateway 用已验证
Artifact 将 public output 通过 `Artifact.Output.FieldID` 合成到 terminal 稳定字段，
然后将展开的 Scan/`join_many` 计划交给现有 `CompileRelational` exposure
和 V4 ordinal compiler。因此 semantic root 不需要、也不得同时声明伪造的
`snapshot_publication`；每个 governed terminal 使用它自己的 publication/sidecar。

签名 Manifest/Grant 对外始终只批准 semantic root 及其 public fields/Scope。
Gateway 每次查询从已绑定 Artifact 派生最小 terminal internal ProductGrants，
它们只是策略编译输入，不写回 Control PG，不加入签名 Grant，也不暴露给
Agent。每个 root scope output 必须映射到确定的 terminal FieldID，并且展开计划
中每个 terminal Product 的 mandatory scope 都必须被已批准谓词覆盖。任一 scope
缺失、映射到错误 stable role/字段或与 terminal Catalog 冲突，都在 reservation 前
fail closed。

当前 runtime composition 只接受一个 semantic root 作为外层 source。查询时将它再与
其他 Product Join，或在 root 上方使用 `ORDER BY`/`LIMIT`/`OFFSET`，均关闭式
拒绝；这不影响 root closure 内部最多 16 源的 connected equi-join。已有
aggregate barrier 的 Artifact 之上仅可投影/重排已计算的 public outputs。

### Task binding 与变更发布

四摘要校验通过后，Gateway 为任务中每个 product 生成
`canonical_plan_digest + dependency_digest + interface_digest` contract，按逻辑
product name 排序并计算 `taskgate-task-view-binding-v1`。这个 digest 进入
`AuthorizationManifestV1`、OA 签名 Grant、Control PG immutable binding set 和后续
query record；反向 dependency rows 只用于定位受影响 task，不向 Agent 暴露物理名。
OA 回调在激活前重新校验，查询执行前再重算；Connector 还会在实际 SQL 的同一个
只读 `REPEATABLE READ` 事务中核 registry revision。

任何 root/child definition、dependency、expanded plan 或 public interface 漂移都会
返回 `VIEW_SEMANTIC_CHANGED`，使旧任务的正交状态单向进入 `REQUIRE_REBIND`。
Catalog/Grant 不允许原地换摘要；管理员应发布更新后的 Catalog，然后由调用者创建
新 task 并重新完成人工 OA 审批。旧 task 已完成的 exact request replay、result artifact
和 receipt 保持原有保留/验证语义。Delegated child 必须继承相同 binding，不能借委托
切换 View revision。

`evaluation/cmd/schema-digest` 只计算 source-level legacy/terminal digest，并自动跳过
semantic roots；它不能替代上述四摘要编译。发布工具必须把四摘要和 Catalog 修改放在
同一个 candidate artifact 中，并在切换前运行 compiler/property/真实 PostgreSQL
regression suite。

## Approval Route

每个被产品实际使用的有效敏感级别都必须有唯一审批路由：

```yaml
approval_routes:
  - sensitivity: low
    mode: manual
    approver: bob
    budget_profile: summary-manual-v5
  - sensitivity: high
    mode: manual
    approver: bob
    budget_profile: detail-manual-v5
```

- 只允许 `mode: manual`；自动任务审批已关闭。
- 每条路由必须配置非空 `approver`；OA Demo 只接受 `bob`。
- `budget_profile` 必须引用已定义 Profile。
- 多产品申请以最高有效敏感级别路由。

## Budget Profile

```yaml
budget_profiles:
  - name: summary-manual-v5
    max_queries: 10
    max_rows: 500
    max_db_time: 30s
    query_timeout: 5s
    task_ttl: 30m
    max_release_facts: 1000
    max_influence_facts: 5000
    max_outcome_facts: 10
    exposure_profile_version: taskgate-exposure-v5
    predicate_footprint:
      version: taskgate-predicate-footprint-v1
      max_raw_literals_per_query: 1000
      max_unique_atoms_per_query: 9
      max_atom_payload_bytes: 4096
      max_total_atom_payload_bytes: 36864
```

资源数值必须为正；可选 exposure 上限必须同时为零/省略或同时为正。
`query_timeout` 不能大于 `max_db_time`。时间使用 Go duration 字符串，如
`500ms`、`5s`、`15m`。含义：

| 字段 | 含义 |
|---|---|
| `max_queries` | 完成或数据库执行失败的允许查询次数上限 |
| `max_rows` | 所有成功结果累计行数上限 |
| `max_db_time` | 累计记账数据库时间上限 |
| `query_timeout` | 单次 PostgreSQL `statement_timeout` 上限 |
| `task_ttl` | 从批准生成 Grant 起的可用时长 |
| `max_release_facts` | 根任务族可新增的已交付结果 FactID 上限 |
| `max_influence_facts` | 根任务族可新增的 positive-output dependency FactID 上限；字段名为兼容标签 |
| `max_outcome_facts` | 根任务族可新增的 V5 predicate atom 与 composite outcome FactID 总上限 |
| `exposure_profile_version` | 固定 FactID、lineage 和收费语义的版本标识 |
| `predicate_footprint` | V5 签名的 raw literal、unique atom 与 payload 大小上限 |

V5 的三个 exposure 上限必须同时为正且必须声明 `predicate_footprint`；禁用时三者均为零且 Profile 为空。
V2 兼容 Profile 的 outcome 上限保持为零。默认 Catalog 启用 V5。
Profile 是经治理、允许被完整消耗的容量边界。Agent 不能选择更小预算；Gateway
按审批路由绑定整个 Profile。若完整额度并不安全，应由 Catalog 管理者降低并发布
新版本，而不能依赖 Agent 节约。
资源预算先预留再按实际量结算；exposure 则在结果产生后按相对根任务已知集合
的 novel facts 结算。资源预算达到硬上限会归档任务，exposure 超限会拒绝且
不释放当前结果；后续调用仍可选择更低成本或完全重复的已知事实。

## 最小完整示例

下面示例是一个没有 semantic root 的 terminal/legacy Product，因此只使用
source-level `schema_digest`。如需把另一个普通 View 作为 Nested View root，应按上节
增加 `view_contract`，而不是把它加入同一个全局 schema digest。

```yaml
catalog_version: "2026-07-21.2"

sources:
  - name: demo
    datasource_id: taskgate-demo-travel
    type: postgres
    address: business-postgres
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    schema_digest: 02b4a211cfbab7347fdce28e2dd76406b1118c5f18e1d2146cc2e85a38ccf1cc
    secretRef: GATEWAY_DB_PASSWORD

snapshot_publications:
  - name: monthly-spend-v1
    source: demo
    source_namespace: demo.monthly_spend
    snapshot: demo-2026-v1
    ordinal_sidecar: taskgate_ordinal.monthly_spend_v1
    sidecar_digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    dictionary_digest: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    manifest_digest: cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc

scopes:
  - name: department
    type: enum
    description: 部门范围
    allowed_values: [销售部]

products:
  - name: monthly_spend
    source: demo
    reporting_view: reporting.monthly_spend
    description: 部门月度费用
    sensitivity: low
    snapshot: demo-2026-v1
    snapshot_publication: monthly-spend-v1
    fact_namespace: demo.monthly_spend
    stable_relation_role: monthly_spend
    entity_key: [month, department]
    scopes: [department]
    allowed_operators: ["=", ">", ">="]
    allowed_aggregates: [sum, count]
    fields:
      - name: month
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 月份
      - name: department
        type: text
        collation: en_US.utf8
        collation_version: "2.36"
        description: 部门
      - name: amount
        type: numeric
        description: 金额

approval_routes:
  - sensitivity: low
    mode: manual
    approver: bob
    budget_profile: small-manual

budget_profiles:
  - name: small-manual
    max_queries: 5
    max_rows: 100
    max_db_time: 10s
    query_timeout: 2s
    task_ttl: 15m
    max_release_facts: 200
    max_influence_facts: 1000
    max_outcome_facts: 5
    exposure_profile_version: taskgate-exposure-v5
    predicate_footprint:
      version: taskgate-predicate-footprint-v1
      max_raw_literals_per_query: 100
      max_unique_atoms_per_query: 4
      max_atom_payload_bytes: 4096
      max_total_atom_payload_bytes: 16384
```

## Knowledge Products（KB-1，知识产品）

知识产品把一个受治理的文档集合接到同一套契约上（设计记录 `docs/kb-design.md`）。它由 `cmd/kb-publish` 冻结为一个
**索引发布期**：三张关系（documents / chunks / values）各发布为一个普通的 snapshot-publication Product，Catalog 顶层再用
两个块把它们捆起来：

```yaml
knowledge_products:
  - name: policy_kb
    description: 部门制度、合同、审批单与报告
    sensitivity: high
    collection: policy_v1                 # kb.collections 中的集合名
    source_namespace: kb.policy           # 三个产品的 fact_namespace 必须是 kb.policy.{documents,chunks,values}
    products: {documents: policy_kb_documents, chunks: policy_kb_chunks, values: policy_kb_values}
    index_publication: policy-kb-2026-10-v1   # 三个产品的 snapshot 必须等于它
    document_types: [contract, approval, policy, report]
    attributes:                           # 过滤原子可命名的封闭属性词表
      - {name: kb_department, type: enum, scope: kb_department}   # 绑定到强制 Scope → 任务范围注入为过滤原子
      - {name: classification, type: enum, scope: classification}
      - {name: job_role, type: enum, allowed_values: [staff, manager, director, executive, hr]}
      - {name: labels, type: text_set}
      - {name: valid_until, type: date}
    segmentation: {function: section-window-v1, window_chars: 800}
    embedding: {model: hash-projection-v1, dimension: 256}
    extraction_rules:
      - {document_type: contract, field: amount, type: numeric, source: frontmatter, scope: document}
    retrieval: {k_max: 20, exact: true, aggregates: [count, sum], generation_admitted: false}
    scopes: [kb_department, classification]
    eligibility: {scope_envelope: {kb_department: {from_attribute: department}, classification: {from_attribute: clearance}}}
kb_publications:
  - epoch: policy-kb-2026-10-v1
    knowledge_product: policy_kb
    manifest_digest: <kb-publication.json 的 manifest_digest>
    segmentation_digest: <sha256 of canonical JSON of segmentation>
    embedding_digest: <...>
    extraction_rules_digest: <...>
```

规则：

- 三个产品必须存在、带 `snapshot_publication`，`fact_namespace` = `<source_namespace>.<kind>`，`snapshot` = epoch，并各自发布
  知识 Fact 身份所需字段（documents: `doc_id/version/content_sha256/chunk_count`；chunks: `+chunk_index/text/text_sha256`；
  values: `+field/value_type/canonical_value/value_text/value_numeric/value_date`），且都带知识产品的全部强制 Scope。
- `scopes` 非空，只能是 enum Catalog Scope；每个强制 Scope 必须有一个同名 enum 属性绑定它（`attributes[].scope`）。
- `extraction_rules` 的 `document_type` 必须在 `document_types` 内，类型限 `text/numeric/integer/bigint/date`，目前只支持
  `source: frontmatter`、`scope: document`；`retrieval.aggregates ⊆ {count,sum,min,max}`，`generation_admitted` 必须为 false。
- 抽取规则的 `scope` 由定义 Catalog 的人按字段业务含义选择：`document`（每份文档版本一个值 Fact，适合金额、日期、编号）或
  `collection`（同一值在集合内按 (field, canonical_value, kb_department, classification) 去重为一个 Fact，适合对方名称、项目代号等大量重复的
  枚举值）。有任何 `collection` 规则时，知识产品必须声明第四个关系 `products.collection_values`（kb-publish 会发布它），aggregate 对该字段的
  请求改在这张关系上结算，依赖与见证集随之变为去重后的值（E-KB5 实测：同一 count 请求，文档级 116 行 / 580 依赖，集合级 10 值 / 50 依赖）。
- `kb_publications` 的三个声明摘要必须等于对本 Catalog 中 `segmentation` / `embedding` / `extraction_rules` 声明的规范 JSON
  做 SHA-256 的结果（Catalog 内自洽，不读文件）；`manifest_digest` 是 kb-publish 清单的摘要。
- 启动激活：Gateway 在加载 snapshot 发布物之后读取 `$GATEWAY_SNAPSHOT_ARTIFACT_DIR/kb/<epoch>/kb-publication.json`，核对清单
  自摘要、`kb_publications` 绑定、三个 bundle 的 product/publication/relation/sidecar 身份与 manifest/dictionary/sidecar 摘要；
  任一不符即拒绝启动。
- `eligibility` 与 Product 的同一结构（见上节 Eligibility），按知识产品的强制 Scope 校验；申请其 chunks / documents / values
  任一关系时，Gateway 在建草稿前按该知识产品的包络编译资格（关系继承知识产品的 eligibility），包络外即 `NOT_ELIGIBLE`。
- 检索语言（`internal/retrieval`）是封闭的：`search`（AND 过滤原子 + 一个文本或向量查询承诺 + `k ≤ k_max`）、`fetch`
  （本根家族已返回过的块引用）、`aggregate`（过滤原子 + 抽取字段上的一个白名单聚合）。原子算子按属性类型限定（enum/text:
  `= <> in`；date/numeric: `= <> < <= > >=  in`；text_set: `has`），字面量用 exposure 规范编码；任务强制范围按 `attributes[].scope`
  注入为原子（进入请求承诺 h_N，不计为调用方原子），调用方对该属性的原子必须与范围相交。语言之外一律关闭式拒绝：
  `RETRIEVAL_FILTER_UNSUPPORTED`、`RETRIEVAL_QUERY_UNSUPPORTED`（含任何未知字段，如重排器、more-like-this）、
  `RETRIEVAL_K_EXCEEDED`、`RETRIEVAL_SCOPE_EXCEEDED`、`CHUNK_NOT_RETURNED`、`AGGREGATE_UNSUPPORTED`。

检索工具与结算（K3.3）：Gateway 向 query 角色列出 `search`、`fetch`、`aggregate` 三个 MCP 工具（参数：`task_id`、`request_id`、
`knowledge_product`，以及各自的 `filters` / `query{text|vector}` / `k`、`chunks[]`、`field` + `aggregate`）。`search` 与 `fetch` 由
`physicalquery.PrepareRetrieval` 准备：先把规范形变成 `chunks ⋈ documents` 的关系计划交给与 `execute_plan` 相同的编译器，再给两条语句各包一层
命中集 CTE（在 `kb.chunks` 上按精确余弦距离排序、同一调用方原子、身份三列全序、`LIMIT k`；`fetch` 按身份枚举、不排序）；密封、预留、
执行、序数推导、结算、回执、重放全部复用 `execute_plan` 的链条，响应列为 `doc_id, version, chunk_index, text`。效果口径见
docs/kb-design.md §5（Release 为四个可见单元格；Dependency 另含块行、文档键单元格与文档行、每个调用方过滤属性一个块单元格）。
`aggregate` 直接是 values 关系上的一个计划（`field` 与 `value_type` 过滤 + 白名单聚合），不新写结算。任务必须同时授权 chunks 与
documents 两个 Product（aggregate 授权 values），且携带 V5 暴露 profile。文本查询由受信嵌入服务编码：Gateway 读 `KB_EMBEDDING_ENDPOINT`
（compose 中默认为空），未配置时文本查询以 `RETRIEVAL_QUERY_UNSUPPORTED` 拒绝、向量查询仍可用；嵌入器在使用前按知识产品声明的
模型 / 维度 / 权重 sha256 核对。业务库须把 pgvector 装在 `pg_catalog`（`db/init/kb-schema.psql`）：连接器为每条语句钉
`search_path = pg_catalog`，距离算子只有在那里才可解析。

发布流程：`kb-publish -spec <spec.json> -artifact-dir <snapshot artifact root>` 写入 kb.* 行、建冻结物化视图、编译三个 bundle 到
`<root>/<publication>/`，并在 `<root>/kb/<epoch>/` 留下清单、三个 compiler input 与 `catalog-fragment.yaml`；
`kb-publish -merge -base config/catalog.yaml -fragment <root>/kb/<epoch>/catalog-fragment.yaml -out candidate.yaml` 把片段追加进
候选 Catalog 并校验；随后按下节发布清单的第 3–8 步（snapshot-index 以三个 input 重编、sidecar-install 与其他发布物同批安装、
重算 `schema_digest`、提升版本、作者签署）。

## 发布清单

1. 在 PostgreSQL 创建或更新 `reporting.*` View，只选择允许发布的列；semantic root 及全部 ordinary child 必须落在上节 restricted fragment。
2. 只向专用只读角色授予目标 View 的 `SELECT`，确保对底层业务 Schema 无权限。
3. 冻结 snapshot，运行 `cmd/snapshot-index`，完整验证 bundle 后把计算出的 publication digests 写入 Candidate Catalog。
4. 更新 Catalog，确认 Product 与 publication 的 source/namespace/snapshot、逻辑字段、类型、Scope 和稳定 `entity_key` 一致。对 semantic root，在同一个 registry snapshot 中生成并核对 definition/dependency/canonical-plan/interface 四摘要；不要手填或复用旧摘要。
5. 为产品最高有效敏感级别配置审批路由和允许完整消耗的预算 Profile；不要依赖 Agent “节约”来补救过大的 Profile。
6. 提升 `catalog_version`。版本切换会使旧 ACTIVE 任务拒绝查询，应先规划任务排空或重新申请。
7. 运行 `make verify`，确认 nested/direct equivalence、determinism、alias/join-order invariance、drift、cycle/limit/aggregate-barrier 和真实 PostgreSQL replacement tests，再用 `docker compose up --build -d --wait` 启动；这些测试是 bounded profile 的实现证据，不是任意 SQL 的形式化证明。
8. 分别用允许、越权字段、缺 Scope、未知 handle/ordinal、损坏 dictionary、物理表名、危险函数、child View replacement 和不支持的 aggregate wrapper 做负向验证。

不要在 Catalog 中写密码、DSN、OA Token、模型 Key 或任何真实查询结果。
