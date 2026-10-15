# 设计说明：按调用者本人限定数据范围（subject-bound scope）

状态：**仅设计，未实施**。2026-10-15 起草。不改 p10-r2 上的代码、catalog 或冻结证据。

## 1. 问题

演示中作者问：能否让"王强只能看自己的报销数据"？这条规则应定义在 catalog 吗？

## 2. 现状：现有机制做不到

以下均为读代码所得（2026-10-15，HEAD 66fc115）。

- **身份只有 token → 固定 principal。** `cmd/gateway/main.go:166-172` 从环境变量读两个 token，
  硬编码 `principal_alice`（Subject `alice`，Role `query`）与 `principal_carol`（`auditor`）。
- **principal 没有属性。** `internal/mcp/server.go:24-28` 只有 `ID / Subject / Role`；
  `internal/control/types.go:32-39` 另加 `TokenHash / CreatedAt / DisabledAt`。没有工号、部门等。
- **Subject 不参与行过滤。** 它只用于任务归属、OA 提交人与审批人核对、审计
  （`internal/gateway/tasks.go:74-75`，`internal/gateway/callback.go:371-381`）。
- **scope 不读调用者。** 类型只有 `enum` 与 `date_range`（`internal/catalog/types.go:129-141`，
  `internal/catalog/validate.go:355-377`）。`normalizeScopes`（`internal/gateway/tasks.go:346`）
  的签名只接收产品与申请里的 scope，没有 principal 参数；取值来自申请，再由审批人收窄。
- **执行注入可复用。** 已批 grant 的 scope 由 `scopePredicates`（`internal/physicalquery/prepare.go:653`）
  转成谓词，经 `internal/sqlpolicy/render.go:33-50` 渲染为 `WHERE`；scope 摘要已被签名覆盖
  （`prepare.go:1042-1053`，`internal/gateway/query.go:2454-2462`）。
- **RLS fixture 不是按人。** `db/init/09-final-v5-rls-fixture.sql` 的策略是常量谓词
  `department = '销售部'`，是论文对照基线。
- **数据里的员工不是系统用户。** 王强（E003）只是种子数据里的一行；系统用户只有 alice。
- `expense_detail` 的强制 scope 只有 `department`（`config/catalog.yaml:169`），连员工维度都无法限定。

## 3. 规则放在哪里

"王强只能看自己的"由三部分拼成，各有归属：

| 要素 | 放哪里 | 理由 |
|---|---|---|
| 该产品可按员工维度限定，且该维度必须等于调用者本人 | **catalog**：新增 scope 类型 `subject_bound`，声明绑定哪个身份属性 | catalog 管"数据能怎么切"，是数据契约 |
| 调用者本人是 E003 | **身份层**：principal 属性。演示期放 principal 配置；产品化接 OIDC 或 HR 主数据（`docs/threat-model.md:114,147` 已列为加固项） | 人是谁不是数据属性，不能由 agent 或申请自报 |
| 谁可以越过本人限制（部门经理看本部门、审计看全部） | **授权策略**：按 Role 决定该 scope 是否绑定本人，或改绑哪个属性 | 例外必须显式、可审计 |

所以答案是：**catalog 只声明"这个维度要绑本人"，"本人是谁"必须来自身份层。**
只写 catalog 不够——没有可信的身份属性，绑定无从取值。

## 4. 应保证的性质

1. 绑定值由网关在建任务时从**已认证的 principal** 取得，不取自申请参数。
2. 申请里给出与本人不同的值 → 拒绝（新错误码），不静默覆盖，以便审计看到越权意图。
3. principal 缺该属性 → 拒绝建任务，不降级为"不限定"。
4. 审批人只能批准或驳回，不能把该维度改成别人；OA 页面标注"本人绑定，不可修改"。
5. 委托子任务继承父 grant 的绑定值，只能收窄。
6. 绑定值进入签名 manifest 与 grant 摘要；执行沿用现有 `scopePredicates`，不另开通道。

## 5. 实现草案（若做原型：另开分支）

1. principal 增属性：`control.Principal` 加 `Attributes map[string]string`（迁移加列），
   `mcp.Principal` 同步携带；`main.go` 的 bootstrap 从配置读取。
2. catalog：`Scope` 增 `Attribute` 字段，新常量 `ScopeTypeSubjectBound`；`validate.go` 放行并校验属性名。
3. `normalizeScopes` 增加 principal 参数；遇 `subject_bound` 时按第 4 节规则取值或拒绝。
4. 核对 manifest、grant 摘要与 `canonicalEffectiveScopeSHA256` 覆盖新类型。
5. catalog 给 `expense_detail` 增 `employee_no` scope（`subject_bound`，attribute `employee_no`）。
6. 发射端与校验端同改：OA 展示与回调校验同时识别新类型。
7. 演示种子：每名员工一个 principal 与 token。

## 6. 影响面

- 改动 catalog 契约与授权核心，属核心架构变更，需作者裁决后才进入实现。
- 会改变 `catalog_version`、schema 与 publication 摘要，**不得在 p10-r2 上做**；论文冻结证据不受本设计影响。
- 与台账 LOOP-49 的关系：`expense_summary` 每组恰 1 笔、研发部与财务部各 1 人，低敏感汇总可还原个人。
  按人绑定只约束明细产品；汇总产品另需最小分组规模（例如每组不少于 5 笔），两者都要做。

## 7. 验证方案（原型时）

- 单测（`normalizeScopes`）：未给值 → 自动填本人；给他人工号 → 拒绝；principal 无属性 → 拒绝。
- 端到端：王强的 token 申请 `expense_detail` → OA 显示 `employee_no = E003` 不可改 → 批准后
  `SELECT SUM(amount)` 得 2990（种子手算：1450 + 1260 + 280，含 1 笔 rejected）；
  alice 的 token 申请 E003 → 拒绝。
- 回归：`go test ./...`；p10-r2 冻结 catalog 的 sha256 不变。
