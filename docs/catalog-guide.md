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
- `enum` 可选 `order`（KB1-LOOP-152）：把该维度声明为有序维度，从低到高列出全部 `allowed_values`，必须恰是它的一个排列（不多、不少、不重复），
  否则目录校验失败；`date_range` 不能声明 `order`。`allowed_values` 本身的写法不变（不要求按等级排）。
  声明后，`request_data_task`、`request_download_task`（显式给 `scopes` 时；缺省取资格包络时同样补一次）与 `issue_data_authorization`
  在申请时把该维度向下补全：取申请值中等级最高者，补上 `order` 里低于它的各级，再做资格判定、审批路由、委托子集校验并写入 manifest；
  返回值的 `scope_closure` 按维度列出补进的值（没有补进时不给此键），审批人仍可删减。
  补全后的整个集合受资格包络约束：主体的可申请值若不是向下封闭的（缺了中间某级），申请更高一级会因补进的低级不在包络内而 `NOT_ELIGIBLE`。
  `order` **只影响申请时的补全，不改放行判定**：授权写了哪些值就只放行哪些值，查询、检索、下载与回执、审计都按授权里的完整集合，不读 `order`。
  目录没有任何 `order` 时，申请、返回值与工具说明都与原来一致。`describe_data_product` 对声明了 `order` 的强制范围给 `scope_order`，
  `list_data_products` 的 `scopes` 带 `order`；三个申请工具的说明里由目录生成一句"填最高一级即可"的提示。例：

  ```yaml
  - name: classification
    type: enum
    description: 密级
    allowed_values: [confidential, internal, public, secret, top_secret]
    order: [public, internal, secret, confidential, top_secret]
  ```

  申请 `classification: [secret]` 得到 `[internal, public, secret]`，`scope_closure: {classification: [public, internal]}`。
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
- 运算符必须来自安全词汇表。允许列表是上限，不会使 AST 原本不支持的 SQL 特性变为可用。LIKE 写 `like` 或 `~~` 都可，ILIKE 写 `ilike` 或 `~~*`（NOT LIKE 写 `not like` 或 `!~~`，NOT ILIKE 写 `not ilike` 或 `!~~*`）；声明 `like` 只放行 LIKE，不顺带放行 ILIKE 或 NOT LIKE。IN 写 `in`（只放行 IN 列表）或 `=`（同时放行等值比较与 IN）；NOT IN 写 `not in` 或 `<>`；BETWEEN 写 `between`，NOT BETWEEN 写 `not between`（各自只放行该形式，BETWEEN SYMMETRIC 不可授予）。词形大小写与首尾空白不计。AND/OR/NOT 与 IS [NOT] NULL 不是算子，不用声明，也不能用 `allowed_operators` 限制。子查询谓词同样按算子门控——`IN (SELECT …)` 按 `in` 或 `=`，`NOT IN (SELECT …)` 按 `not in` 或 `<>`，`op ANY/ALL (SELECT …)` 按 op；EXISTS 与标量子查询不是算子，由子查询自身的列与产品校验约束。

## Eligibility（资格先于审批）

`eligibility` 是 Product 上可选的资格谓词：Gateway 在 `request_data_task` 创建 OA 草稿**之前**，用申请人（根任务的
Principal subject；委托任务的父 Grant 人类主体）经身份系统证明的属性推出一个"可申请范围"。申请落在可申请范围外立即以
`NOT_ELIGIBLE` 拒绝，不进入审批队列、不建任务、不设例外路由；审批人只能在可申请范围内收窄。资格裁决是机器裁决，人工审批
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
- 每条路由必须配置非空 `approver`。OA Demo 的审批人有 `bob`，以及配置了 `OA_DANA_PASSWORD` 时的 `dana`；每个审批人只能看到并审批点名给自己的草稿。
- 按范围条件选审批人（可选 `scope_condition`）：例如 `{classification: [leadership]}`，任务强制范围含任一所列值时命中。
  - 有条件的路由按声明顺序优先于同一敏感级、同一产品集合的无条件路由，并且必须有这样一条无条件兜底路由。
  - 条件只能写目录的枚举范围及其允许值（排序、去重）。
  - 条件只决定谁来审批和用哪个预算，不改变授予的内容。
- 审批人权限：
  - 审批人永远不能是任务的申请人本人（`APPROVER_NOT_AUTHORIZED`）。
  - 路由带 `verify_approver: true` 时，审批人必须在主体属性快照中，并且按其属性推出的范围（与申请人同一套规则），对所申请的全部范围都有资格。例如没有 leadership 密级的人不能审批 leadership 任务。不满足即在建 OA 草稿前以 `APPROVER_NOT_AUTHORIZED` 拒绝。
  - 审批人的资格裁决与申请人的一并记入任务的私有记录。
  - 两道门各管一边：申请人的可申请范围决定能申请什么，审批人的资格决定谁有权批。
- 申请人自选审批人（`request_data_task` 与 `request_download_task` 的可选 `approver`）：覆盖目录路由，相当于审批流被手工改派或配错。
  - 自选的审批人仍不得是申请人本人，否则请求时即以 `APPROVER_NOT_AUTHORIZED` 拒绝；其余情况照常建 OA 草稿，返回值的 `approver_authorized` 预告该审批能否生效。
  - 审批回调到达时，TaskGate 按数据产品规则（按其属性推出的范围，与申请人同一套规则）对实际审批人、针对批准后（可能已缩小）的范围再核一次。不通过则不签发授权，任务以终止原因 `approver_not_authorized` 归档。
  - 走 verify_approver 路由的审批同样在回调时复核。所以即使 OA 流程配错、让无权者批准，也拿不到授权；审批人缩小掉自己无权批的范围后，批准有效。

```yaml
approval_routes:
  - {sensitivity: high, mode: manual, approver: bob, budget_profile: kb-large-v1, verify_approver: true}
  - {sensitivity: high, mode: manual, approver: dana, budget_profile: kb-large-v1, verify_approver: true,
     scope_condition: {classification: [leadership]}}
```

### 审批链（`chain`，KB1-LOOP-141；演示功能，不属论文主张）

路由可以不点名审批人，改由组织架构逐级审批：

1. 申请人所在部门的审批人（`directors` 里该部门的人：部门主任，或演示里的部门管理员）；
2. 申请范围中其余每个部门的审批人，按部门值排序；
3. 之后按 `escalations` 的顺序逐层：申请范围命中该层的触发值（如 `classification: confidential` 或 `leadership`）时，该层上每个所涉部门的负责人，按部门排序，同一人负责的部门合为一步。

```yaml
approval_routes:
  - sensitivity: high
    mode: manual
    budget_profile: kb-large-v1
    chain:
      requester_attribute: department        # 申请人的部门属性，须恰有一个值
      department_scope: kb_department        # 列出被申请部门的枚举范围
      directors: {finance: finance-admin, sales: sales-admin, ...}         # 须覆盖该范围的全部值
      director_authority: {attribute: department, role: {attribute: job_family, any_of: [dept_admin]}}
      escalations:                           # 升级层，按链上顺序；KB1-LOOP-147
        - role: director                     # 本层步骤的角色名，小写标识符，层间唯一，不得是 requester_director / department_director
          label: 部门主任                     # 展示名，可省略
          # level: 0                         # 可选，长期授权模型下的职权层级（见下节）；省略即序号加 1
          scope: classification
          values: [confidential, leadership]
          leaders: {finance: finance-director, sales: sales-director, ...}  # 须覆盖全部部门
          leader_authority: {attribute: department, role: {attribute: job_family, any_of: [director]}}
        - role: leader
          label: 分管领导
          scope: classification
          values: [leadership]
          leaders: {finance: leader-admin, sales: leader-business, ...}
          leader_authority: {attribute: oversees, role: {attribute: job_family, any_of: [leader]}}
```

- 旧写法 `escalation:`（单个升级层，角色名固定为 `leader`）仍受理，等价于只有一层的 `escalations`；两者不得同时出现。
- 链路由不写 `approver` 与 `scope_condition`。组织架构须覆盖部门范围的全部值，缺一个即目录无效。
- **权限核验**：每一级审批人都要在主体属性快照中核验。
  - `authority.attribute` 须含该步的全部部门；
  - `role` 须成立。
  - 目录点名的审批人在建草稿前就核，不满足即 `APPROVER_NOT_AUTHORIZED`，目录配错时失败关闭。
  - 每一级在审批回调到达时再核一次，不满足则任务以 `approver_not_authorized` 归档，不签发授权。
  - 申请人不能出现在自己的链上。
- **缩小与重算**：
  - 每一级批准或缩小的是上一级的授权，在本级时刻重新签发，有效期从本级起算；
  - TaskGate 按该级留下的范围重算剩余环节，已批过的人不再出现；
  - 没有剩余环节时签发授权，任务变为 ACTIVE；
  - 任一级拒绝，任务即归档。
- **申请人自选审批人**：`approver` 只替换第一级，并照常在回调时核验。
- **可见性**：`request_data_task` 返回 `approval_chain`（每步含 `role`、`role_label`、`approver`）；`get_task_status` 与 `wait_for_approval` 返回当前审批人和每一步的状态（approved / current / pending）；每级审批回调的答复含 `next_role` 与 `next_role_label`。

### 长期授权模型（`delegated_authorization`，KB1-LOOP-147/148；演示功能，不属论文主张）

顶层块 `delegated_authorization` 打开演示的粗粒度长期授权模型。块本身进目录摘要，所以每张 manifest 与回执都绑定了它是否打开；没有这个块的目录（正式轮）行为完全不变。

```yaml
delegated_authorization:
  issuer_role: {attribute: job_family, any_of: [dept_admin]}  # 代开单（issue_data_authorization）所需角色，必填
  child_self_confirm: true          # 子任务由树的人类主体本人确认
  authority_attribute: authority_departments  # 列出主体有职权的部门的属性；省略即 authority_departments
  center_scope: center              # 中心范围（枚举）；省略即不启用中心规则
  center_attribute: authority_center  # 列出主体所辖中心的属性；省略即 authority_center
  personal_scope: employee_id       # 个人记录范围（枚举）；申请人同名属性是本人的值；省略即不启用
  agent_submits: true               # agent 代人类主体提交：任务创建即 AWAITING_APPROVAL，人只在 OA 批准；省略即 false
  pending_ttl: 72h                  # 待审任务（AWAITING_SUBMISSION / AWAITING_APPROVAL）自创建起的最长等待；到期归档为 expired 并通知 OA 撤回草稿；省略即永不过期
products:
  - name: attendance_records
    steward_department: hr          # 数据管家部门，须是每条审批链 department_scope 的取值
    scopes: [kb_department, classification, center, employee_id]
    ...
```

以下规则只在块存在时生效，只作用于申请人自己发起的任务；代开单（`issue_data_authorization`）照旧从发单人那一级起排、不走这些规则。审批人照常在建草稿前与每级回调时核验权限。

1. **职权层级**。链上每一步有层级：两类部门审批人（`requester_director`、`department_director`）为 0；升级层的层级取该层可选的 `level`（整数，0 到层数），省略时为它在 `escalations` 里的序号 i（从 0 数）加 1。例如作者定部门主任与部门管理员同层，演示目录把主任层写 `level: 0`。`level` 只影响下面的职权判断，不改变链上的顺序与谁出现在链上。申请人的层级是组织架构里点到他的各处层级的最大值（是 `directors` 的人为 0，都不是为 -1）。排链时：
   - 某一步的层级不高于申请人的层级、且该步的部门都在申请人的职权部门（`authority_attribute`）内：申请人本人代表这一步，删去；
   - 某一步点到申请人本人、而本人对该步部门无职权：按部门改由组织图里该步之后、不是本人的第一层的人审批；
   - 申请范围含职权外的部门（跨部门）时，申请人本部门那一步若按第一条被删，改由组织图顺序里申请人所在位置之后、不是本人的第一层的人审批，与 `level` 无关（部门管理员跨部门 → 本部门主任；主任跨部门 → 分管领导）；
   - 例（主任层 `level: 0`）：管理员申请本部门秘密 → 本人确认；机密 → [分管领导]；绝密 → [分管领导, 总经理]；管理员跨部门内部 → [本部门主任, 对方管理员]；主任跨部门 → [分管领导, 对方管理员]；
   - 同一人只出现一次（保留第一次出现的位置，同角色时合并部门）；找不到可替换的人即 `APPROVER_NOT_AUTHORIZED`。
   - 链上一步不剩时，任务由申请人本人确认（self-service）：审批人是申请人，返回 `self_service: true` 与 `self_confirm: true`，申请人登录 OA 提交并批准即生效；此时不能用 `approver` 指定他人（`INVALID_REQUEST`）。
   - 中心：申请只含本部门、只含本人所辖中心（`center_scope` 的值都在 `center_attribute` 内）、不触发任何升级层，且所有产品都声明了 `center_scope` 时，同样由申请人本人确认；超出本中心则按上面的规则（通常是本部门管理员审批）。
   - 职权快照在申请时记入任务的私有路由记录，审批人缩小范围后每一级都按同一份记录重算。
2. **个人记录自助**。根任务的所有产品都声明了 `personal_scope`，且该范围的申请值恰好等于申请人同名属性的值集合（非空）时，任务由申请人本人确认，不论部门。只申请别人的记录由可申请范围挡下（`NOT_ELIGIBLE`），与本规则无关；含未声明该范围的产品（如文档）的混合任务不适用本规则。
3. **数据管家**。任务的所有产品都声明了 `steward_department` 时，申请范围里既不在申请人职权内、也不是申请人本部门的部门，一律换成管家部门再排链（例：HR 管理员申请全公司考勤 → 只剩 hr → 本人确认；财务管理员申请财务与销售的考勤 → 本部门主任 + HR 管理员）。混合任务（含未声明管家的产品）按部门常规排链。
4. **成员确认**。`request_data_task` 带 `parent_task_id` 并用 `delegate_principal_id` 委托给另一个人的查询主体时：任务的人类主体仍是父任务的（委托收缩检查不变），审批人是受托成员本人，可申请范围按成员推出；成员可在 OA 提交并批准，回执上的审批人即成员；返回 `member_confirm: true`，`get_task_status` 的 `waiting_for` 为 `member_submission` / `member_confirmation`。委托给自己（或同一人类主体）的子任务仍按 `child_self_confirm` 由本人确认。

块打开时另有以下几项：

- `request_data_task` 可选 `budget_profile`：取目录里的预算档替换路由的预算档，须与路由预算档的 `exposure_profile_version` 相同（返回 `budget_source: requested_profile`）；块不存在时传入即 `INVALID_REQUEST`。子任务仍与父授权取交。
- `issue_data_authorization`（代开单）：数据范围只能是发单人自己的部门（数据属主），链从发单人的部门起排、第一步须是发单人；员工可以属于任何部门，但员工本人的可申请范围须覆盖所开范围。
- `list_data_products` 返回 `delegated_authorization`，说明以上规则与所用属性名；`request_data_task` 的工具说明与入参 schema 也随之列出 `budget_profile`。
- `list_my_tasks`（KB1-LOOP-150）：每条任务另给 `kind`（`project` 项目，即申请时选了预算档的根任务；`task` 日常任务，即按路由缺省预算档的根任务；`child` 项目内子任务；`authorization` 代开授权单；块打开后新建的任务在私有请求上下文里记 `budget_profile` 与 `budget_profile_chosen`，此前的根任务没有这两项，分不出项目与日常任务，给 `root`）、`budget_profile`（建任务时记下的预算档名，子任务的实际预算是它与父授权取交；没有记录的旧任务取预算与之完全相同的档名，没有就省略）、`products`、`scopes`（ACTIVE/ARCHIVED 且有授权记录时取授权的范围，即审批人缩小后的范围，否则取申请范围）、`approver`（审批链任务待审批时是当前一步的审批人，否则是记录的第一审批人）、`self_confirm`、`member_confirm`、`agent_submitted`，代开单另给 `issued_by` 与 `human_subject`；私有请求上下文无法解码的任务只给基本字段并标 `detail_unavailable: true`。可选入参 `root_task_id`（只列该根任务及其后代中自己名下的）与 `kind`（在取回的一页内过滤，`next_cursor` 仍按未过滤的页给出）。只列本人主体名下的任务：代开单列在员工名下，开单人的列表里没有。块不存在时条目不变，传 `root_task_id` 或 `kind` 即 `INVALID_REQUEST`。
- `agent_submits: true`（KB1-LOOP-149，作者 2026-10-28 定）：任何场景下提交都由 agent 代人类主体完成，人只审批。`request_data_task`、`request_download_task` 与 `issue_data_authorization` 建的任务创建即 `AWAITING_APPROVAL`（返回 `agent_submitted: true`），OA 草稿以 `submitted: true` 建为待审批、不发 `submitted` 回调（adapters.md），网关审计记一条 `TASK_SUBMITTED_BY_AGENT`（actor 是发起调用的主体，payload 含 `human_subject`、`agent_id`、`approver`、`self_confirm`、`member_confirm`、`draft_id`，代开单另含 `issuer_subject`）。`next_action` 直接是审批那一类（`await_approver_decision`，本人或成员确认为 `human_confirm_in_oa`），不会出现 `human_submit_in_oa`；上述工具与 `get_task_status`、`wait_for_approval`、`revoke_task` 的说明、以及 `list_data_products` 的 `delegated_authorization`（`agent_submits: true`）随之注明。开关缺省关，关时行为不变。
- 撤回与待审过期（KB1-LOOP-137 待办，2026-10-28）：块存在时，任务被 revoke_task 撤销（含根任务级联归档的后代）或按 `pending_ttl` 过期后，网关向 OA `POST /api/drafts/{id}/withdraw`（同 /api/drafts 的服务令牌），OA 把草稿标为已撤销/已过期并移出待办；通知失败只记日志，迟到的回调仍被网关以 409 拒绝。`pending_ttl` 是正的 Go duration，进目录摘要。
- 可切换的 agent 身份（KB1-LOOP-151，作者 2026-10-28 定）：演示时一把 MCP 令牌代表谁，由人在 OA「我的 agent」处切换，agent 自己不能切——令牌先以摘要登记（`POST /admin/v1/demo/agent-identities`，管理令牌之下，体为 `label`、`token_sha256`、`principal_id`、`by`，网关不见令牌明文），OA 以与审批回调相同的签名调 `POST /api/v1/oa/agent-identity`（`list` 列出绑定与候选主体，即已启用、属性快照认得的查询主体；`switch` 改绑，每次绑定记入 `demo_agent_identity_switches`），登记过的令牌先于静态主体表认证，切换从下一次请求起生效；块打开时另有 `whoami` 工具（主体、角色、部门、岗位、中心、密级、职权部门、员工号，可切换令牌另给 `agent_label`、`bound_by`、`bound_at`），且每个结构化工具结果顶层带 `acting_as`（`principal_id`、`subject`、可切换令牌的 `agent_label`、部门与岗位的首值）。块不存在时两个接口答 404，`whoami` 不列出、调用得 `FORBIDDEN`，结果不带 `acting_as`。

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
  知识 Fact 身份所需字段（documents: `doc_id/version/content_sha256/chunk_count`；chunks: `+chunk_index/text_sha256`，正文 `text` 不是发布字段，网关交付时读业务表并按摘要核对；
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
- 启动激活：Gateway 在加载 snapshot 发布物之后读取 `$GATEWAY_KB_ARTIFACT_DIR/<epoch>/kb-publication.json`，核对清单
  自摘要、`kb_publications` 绑定、三个 bundle 的 product/publication/relation/sidecar 身份与 manifest/dictionary/sidecar 摘要；
  任一不符即拒绝启动。清单目录与 `GATEWAY_SNAPSHOT_ARTIFACT_DIR` 分开：snapshot 目录里只能有 Catalog 声明的 publication，
  多一项即拒启动，所以 kb-publish 输出的 `<root>/kb` 要单独挂给 `GATEWAY_KB_ARTIFACT_DIR`；该目录同样只能含 Catalog 声明的
  epoch，不认的条目、符号链接或文件都拒绝激活。
- `eligibility` 与 Product 的同一结构（见上节 Eligibility），按知识产品的强制 Scope 校验；申请其 chunks / documents / values
  任一关系时，Gateway 在建草稿前按该知识产品的 eligibility 推出可申请范围（关系继承知识产品的 eligibility），可申请范围外即 `NOT_ELIGIBLE`。
- 检索语言（`internal/retrieval`）是封闭的：`search`（AND 过滤原子 + 一个文本或向量查询承诺 + `k ≤ k_max`）、`fetch`
  （只接受本根任务族此前 `search`/`fetch` 已返回过的块引用，网关对照该根族账本的 Release 集合逐块核对，否则
  `CHUNK_NOT_RETURNED`）、`aggregate`（过滤原子 + 抽取字段上的一个白名单聚合）。原子算子按属性类型限定（enum/text:
  `= <> in`；date/numeric: `= <> < <= > >=  in`；text_set: `has`——语言接受，但检索计划尚无渲染，`search` 以
`RETRIEVAL_FILTER_UNSUPPORTED`（reason `OPERATOR_NOT_EXECUTABLE`）拒绝，MCP schema 也不再列出它），字面量用 exposure 规范编码；任务强制范围按 `attributes[].scope`
  注入为原子（进入请求承诺 h_N，不计为调用方原子），调用方对该属性的原子必须与范围相交。语言之外一律关闭式拒绝：
  `RETRIEVAL_FILTER_UNSUPPORTED`、`RETRIEVAL_QUERY_UNSUPPORTED`（含任何未知字段，如重排器、more-like-this）、
  `RETRIEVAL_K_EXCEEDED`、`RETRIEVAL_SCOPE_EXCEEDED`、`CHUNK_NOT_RETURNED`、`AGGREGATE_UNSUPPORTED`。

检索工具与结算（K3.3）：Gateway 向 query 角色列出 `search`、`fetch`、`aggregate` 三个 MCP 工具（参数：`task_id`、`request_id`、
`knowledge_product`，以及各自的 `filters` / `query{text|vector}` / `k`、`chunks[]`、`field` + `aggregate`）。`search` 与 `fetch` 由
`physicalquery.PrepareRetrieval` 准备：先把规范形变成 `chunks ⋈ documents` 的关系计划交给与 `execute_plan` 相同的编译器，再给两条语句各包一层
命中集 CTE（在 `kb.chunks` 上按精确余弦距离排序、同一调用方原子、身份三列全序、`LIMIT k`；`fetch` 按身份枚举、不排序）；密封、预留、
执行、序数推导、结算、回执、重放全部复用 `execute_plan` 的链条，响应列为 `doc_id, version, chunk_index, text_sha256, text`
（前四列是封印语句的结果，`text` 是交付时按 `text_sha256` 核对后补上的正文；按设计不回相似度分数，名次即返回顺序）。效果口径见
docs/kb-design.md §5（Release 为四个可见单元格；Dependency 另含块行、文档键单元格与文档行、每个调用方过滤属性一个块单元格）。

文档下载任务（K7；docs/kb-design.md §12）：检索与下载是两种任务。
- `request_download_task`（`objective`、`knowledge_product`、`documents[]` 为 1–20 个 `{doc_id, version}`、可选 `include_values`、
  可选 `scopes`，缺省为申请人的可申请范围）：产品与列由网关固定——chunks 与 documents，`include_values` 时加 values。
  清单以排序后的 `"doc_id@version"` 存入授权单的保留范围键 `kb_documents`，审批人在 OA 看到并可删减（沿用子集缩小规则）。
  普通 `request_data_task` 不能携带该键；下载任务不能委托。
- `download_document`（`task_id`、`request_id` 最长 100 字符、`knowledge_product`、`doc_id`、`version`）：
  - 依次以 `<request_id>:document`、`:chunks`、`:values` 三个准备操作读取文档行、全部分块（fetch 形）与批准时的抽取值，各自结算并出回执；
  - 分块数不等于文档行的 `chunk_count`（例如某节在任务范围外）即整份拒绝，不释放任何分块；
  - 全部结算后，由结算行确定性地组装 `taskgate-kb-document-file-v1` Markdown 文件（标题、抽取值、按序正文；不含存储的 body、
    节标题与原始前置字段），返回文件内容、`file_sha256`、逐块 `text_sha256` 清单与短期 `download_url`
    （`/api/v1/kb/documents/download`，HMAC 能力绑定文件摘要）；MCP 结果另附一个嵌入资源。
- 原文件通道（§12.8）：`request_download_task` 加 `original_file: true` 时，documents 的列加 `content_sha256`（即原文件权，审批人可删去），并隐含 values；
  `download_document` 加 `format: "original"` 时，三部分照常结算（document 部分多释放 content_sha256），再按身份读出存储的源文档，
  核其 SHA-256 等于已释放的 content_sha256 后原样交付；未批准原文件即 `DOWNLOAD_NOT_APPROVED`。节标题与未抽取的前置字段随文件整体释放。
- 拒绝码：`TASK_KIND_MISMATCH`（下载任务中的 search/fetch/aggregate/query_sql/execute_plan，或检索任务中的下载）、
  `DOWNLOAD_NOT_APPROVED`（文档不在批准清单内）、`DOCUMENT_NOT_DOWNLOADABLE`（文档不存在、在范围外或有范围外的节）。
`aggregate` 直接是 values 关系上的一个计划（`field` 与 `value_type` 过滤 + 白名单聚合），不新写结算。任务必须同时授权 chunks 与
documents 两个 Product（aggregate 授权 values），且携带 V5 暴露 profile。文本查询由受信嵌入服务编码：Gateway 读 `KB_EMBEDDING_ENDPOINT`
（compose 中默认为空），未配置时文本查询以 `RETRIEVAL_QUERY_UNSUPPORTED` 拒绝、向量查询仍可用；嵌入器在使用前按知识产品声明的
模型 / 维度 / 权重 sha256 核对。业务库须把 pgvector 装在 `pg_catalog`（`db/init/kb-schema.psql`）：连接器为每条语句钉
`search_path = pg_catalog`，距离算子只有在那里才可解析。

发现面、就绪预告与拒绝细节（KB1-LOOP-137；代码出处见各条）：
- `list_data_products` 增 `knowledge_products`，各关系产品带 `knowledge_product: {name, role}`；`describe_data_product` 接受知识产品名。
  知识产品页给出 `retrieval`（`k_max`、三种查询方式及限额：text 为归一化后至多 4096 字节、keywords 至多 8 个且每个至多 64 字符、
  vector 的维度）、`attributes`（类型、`allowed_values`、实际可执行的 search 算子、`filter_columns`）、`extracted_fields`（可用聚合与所读列）
  与 `task_requirements`。其中 search/fetch 的列取自检索计划本身（physicalquery.RetrievalRequiredColumns：chunks 的
  `doc_id, version, chunk_index, text_sha256` 加每个过滤属性的列，documents 的 `doc_id, version`；作用域列不必授权），aggregate 的要求
  直接运行 aggregatePlan 得出。不发布 collection、source_namespace、嵌入权重与 eligibility。
- `request_data_task`（按申请清单）与 `get_task_context`（按已批授权）返回 `knowledge_readiness`：每个涉及的知识产品能否 search/fetch/aggregate、
  缺哪些关系或列、哪些属性可过滤、暴露 profile 是否为 V5。只作信息，从不让请求失败；审批人仍可能删减。
- `request_data_task`、`get_task_status`、`wait_for_approval` 返回 `waiting_for` 与 `next_action`：`human_submit_in_oa`（须申请人本人在 OA 提交，
  agent 不能代交；目录启用 `delegated_authorization.agent_submits` 时不会出现，任务创建即待审批）、`await_approver_decision`、`use_task`、`none_terminal`；配置 `PUBLIC_OA_BASE_URL` 时，未批准任务的状态带草稿链接 `oa_url`。
- `get_sql_capabilities` 增 `where_predicates`、`where_rejected`、`catalog_control_defaults`；`describe_data_product` 增 `sql_effective`
  （生效算子：声明的或策略缺省集；该产品可用的 WHERE 谓词；聚合，未声明即不能聚合）。

拒绝细节：错误体在 `code`、`message` 之外带 `reason` 与以下键（`mcp.ToolError.Details`）。

| reason | code | 何时 | 细节与补救 |
|---|---|---|---|
| `UNKNOWN_KNOWLEDGE_PRODUCT` | `INVALID_REQUEST` | knowledge_product 不是目录中的知识产品名（KB1-LOOP-137 前为 `POLICY_DENIED`"任务授权外的知识产品"） | `knowledge_products` 列出全部知识产品；输入是某知识产品的关系名时给 `suggested_knowledge_product` 与 `relation_role` |
| `RELATIONS_NOT_GRANTED` | `POLICY_DENIED` | 任务缺工具所读的关系（search/fetch：chunks 与 documents；aggregate：values；download_document：chunks 与 documents） | `missing_products`；授权只能收窄，须按 `task_requirements` 新建任务 |
| `COLUMNS_NOT_GRANTED` | `POLICY_DENIED` | 检索或 aggregate 计划读取的列未获授权 | `missing_columns`（关系 → 列）；同上新建任务 |
| `OPERATOR_NOT_EXECUTABLE` | `RETRIEVAL_FILTER_UNSUPPORTED` | 语言接受、计划尚无渲染的过滤算子（text_set 的 `has`） | `attribute`、`attribute_type`、`operator` |
| `RETRIEVAL_NOT_PREPARABLE` | `POLICY_DENIED` | 其余检索准备失败（兜底） | 具体原因只记在网关日志（trace_id 对应的 cause） |
| `QUERY_IN_PROGRESS` | `CONFLICT` | 同一任务已有在途查询（每个任务一个名额，委托子任务各有名额） | `retryable`、`same_request_id_ok`：在途查询返回后可用同一 request_id 重试 |

`POLICY_DENIED` 的全部发射点（gateway 包，KB1-LOOP-137 后）：`QueryPlan 不在任务授权可精确计量的数据暴露片段内`（query.go，兜底，cause 记日志）、
`任务授权缺少目录要求的强制数据范围`（query.go）、`当前实例不支持跨数据源任务`（datasource.go）、`QueryPlan 请求了任务授权外的数据产品`、
`QueryPlan 无法在语义 View 的任务授权内编译`、`QueryPlan 使用了语义 View root 未授权的运算`、`语义 View 查询不在其治理闭包与任务授权内`（view_execution.go）、
`检索不在任务授权可精确计量的数据暴露片段内`（retrieval.go，兜底）、`任务未授权该知识产品的抽取值关系`（知识产品无 values 关系时）、
`委托任务无法继承父任务的授权预算`、`委托任务扩大了父 Grant 的产品、字段、范围或期限`（tasks.go），以及 retrieval_refusals.go 的
`RELATIONS_NOT_GRANTED` 与 `COLUMNS_NOT_GRANTED` 两类。被拒请求不进审计链，只在网关日志按 trace_id 记录：拒绝只取决于授权与请求，不取决于数据。

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
