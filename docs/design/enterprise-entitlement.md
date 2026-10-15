# 设计说明：企业权限模型（部门、岗位、上下级）

状态：**仅设计，未实施**。2026-10-15 起草。扩展 [subject-bound-scope.md](subject-bound-scope.md)。
不改 p10-r2 上的代码、catalog 或冻结证据。

## 1. 问题

真实企业的权限比演示复杂：HR 能看全公司；销售只能看销售数据；生产只能看生产数据；
同一部门里，员工与领导能看的也不一样。FactGate 应如何承接这些规则？

## 2. 现状

读代码所得（2026-10-15）：

- **身份没有组织属性。** principal 只有 `ID / Subject / Role`（`internal/mcp/server.go:24-28`），
  用户在启动代码里硬编码为 alice 与 carol（`cmd/gateway/main.go:166-172`）。
- **没有常设权限上限。** 建任务时 `normalizeScopes`（`internal/gateway/tasks.go:346`）只核对 catalog
  的取值域，不接收 principal。alice 能申请的上限就是整个 catalog。
- **审批路由不看组织。** 路由只按产品敏感度或产品名，审批人全是 bob（`config/catalog.yaml:717-791`）。
- **吊销只能本人发起。** `revoke_task` 是"撤销自己的任务"（`internal/gateway/tools.go:76`），
  没有管理员或系统因调岗离职而吊销的入口。
- **已有、可复用的部分。** grant 被签名；执行时 grant 的 scope 由 `scopePredicates`
  （`internal/physicalquery/prepare.go:653`）注入 `WHERE`；预算与暴露记账按任务计。
- `legacy.employees` 有薪资、银行账号、电话列（`db/init/00-schema.sql:13-20`），
  但没有任何产品暴露它们（`config/catalog.yaml` 中无 `salary`）。

结论：今天的安全性完全落在单一审批人身上；FactGate 管住了"这一次给多少"，但没有"这个人最多能要多少"。

## 3. 模型：四层，逐层只能收窄

```
身份（IdP / HR 主数据）：此人是谁、部门、岗位、上级
  → 常设权限上限：此人对每个产品最多能申请的字段与范围
    ∩ catalog：数据能怎么切（产品、字段、scope 维度、SQL 白名单）
      ∩ 任务申请：agent 自写的这一次要什么
        → 审批：按组织路由，只能收窄
          → 签名 grant → 每条查询注入 WHERE + 预算记账（现有）
```

| 层 | 管什么 | 放在哪里 | 属主 | 变更频率 |
|---|---|---|---|---|
| 身份 | 部门、岗位、职级、上级、在职状态 | 企业 IdP（OIDC claims）或 HR 主数据同步 | HR / IT | 每天（入职、调岗、离职） |
| 常设权限 | 角色与属性 → 每个产品的字段上限与 scope 上限 | 独立、版本化的策略（文件或策略引擎，如 OPA、Cedar） | 安全 / 数据治理 | 每月 |
| catalog | 产品、字段、敏感度、scope 维度、SQL 白名单、预算档 | `config/catalog.yaml`（已有） | 数据团队 | 随数据发布 |
| 任务与审批 | 这一次的字段、范围、期限、预算 | FactGate（已有）+ 组织路由 | 申请人与审批人 | 每次 |

**为什么不全塞进 catalog**：catalog 是数据契约，随数据发布而变，且版本与摘要绑定冻结证据；
权限随组织和人变，属主也不同。合在一起，每次调岗都要改 catalog、换 `catalog_version`、重算摘要。
catalog 只需要做一件事：声明哪些维度**可以**按身份限定（见第 4 节 scope 表达式）。

## 4. 常设权限怎么写

规则按"角色 + 属性 → 产品 → 字段上限 + scope 上限"表达。scope 值允许三种写法：

- 字面值：`销售部`
- 相对值：`$self.department`、`$self.employee_no`、`$self.managed_departments`，由网关用已认证属性展开
- 全量：`*`

示例（示意格式，非最终语法）：

```yaml
entitlements:
  - who: { role: hr }
    product: employee_profile          # 假想的新产品，含薪资
    columns: "*"
    scopes: { department: "*" }
  - who: { department: 销售部, level: manager }
    product: expense_detail
    columns: [receipt_no, employee_name, department, expense_date, expense_type, amount, status]
    scopes: { department: $self.department }
  - who: { department: 销售部, level: staff }
    product: expense_detail
    columns: [receipt_no, expense_date, expense_type, amount, status]
    scopes: { department: $self.department, employee_no: $self.employee_no }
  - who: { department: 生产部 }
    product: production_output         # 假想的生产类产品
    columns: "*"
    scopes: { plant: $self.plant }
  - who: { any: true }
    product: expense_summary
    columns: "*"
    scopes: { department: $self.department }
```

字段级规则（薪资、身份证、银行账号）直接写在 `columns` 上限里：HR 有，部门经理没有。

## 5. 运行时规则

1. **建任务时求上限。** 网关以已认证身份向策略层请求"此人对所申请产品的上限"，
   申请的字段或范围超出上限 → **拒绝并说明是上限层拒绝**，不静默截断。
2. **相对值由网关展开。** `$self.*` 取自身份层，不取自申请参数；展开后的具体值写入签名 manifest。
3. **审批按组织路由。** 默认审批人为申请人直属上级；涉及他部门数据时加数据属主部门负责人；
   高敏感产品加二级审批。审批人只能收窄，不能超出申请人上限。
4. **agent 代人办事不放大权限。** agent 的上限 = 委托人上限 ∩ agent 自身白名单；
   委托子任务继承父 grant，只能收窄（现有委托机制已保证后半句）。
5. **时效与吊销。** manifest 记录策略版本与身份属性快照。身份层发出调岗或离职事件时，
   系统吊销此人进行中的任务；在吊销通道建成前，依赖短 TTL 兜底。需新增管理员或系统吊销入口
   （现有 `revoke_task` 只能本人撤销）。
6. **拒绝可归因。** 审计记录每次拒绝发生在哪一层：上限、catalog、grant、预算或暴露预算。

## 6. 用例走查

| 用例 | 上限层 | catalog | 审批 | 结果 |
|---|---|---|---|---|
| HR 专员查全公司各部门薪资汇总 | `employee_profile` 全字段、全部门 → 通过 | 需先发布含薪资的产品 | 上级 + HR 负责人（高敏感二级） | 通过 |
| 销售员工查研发部报销 | `department` 上限为 `$self.department` = 销售部 → **拒绝** | — | 不进入审批 | 上限层拒绝 |
| 销售员工查本人报销 | `employee_no = $self.employee_no` → 通过 | `expense_detail` 需增 `employee_no` scope | 直属上级 | 只得本人行 |
| 销售经理查本部门报销明细 | 销售部全员，不含薪资 → 通过 | 已有 | 直属上级 + 数据属主（财务） | 通过 |
| 王强调岗到销售部 | 身份事件触发重算 | — | — | 研发部相关进行中任务被吊销 |

## 7. 与已发现问题的关系

- [subject-bound-scope.md](subject-bound-scope.md) 是本模型中 `$self.employee_no` 的特例。
- 台账 LOOP-49：`expense_summary` 每组恰 1 笔，研发部与财务部各 1 人，低敏感汇总可还原个人。
  权限上限管"谁能要"，管不了"要到的东西是否已足以识别个人"；汇总产品仍需最小分组规模。两者都要做。

## 8. 实施顺序建议（若进入实现，另开分支）

1. 身份属性：principal 增属性，演示期从配置读取，产品化接 OIDC。
2. scope 相对值：先只实现 `$self.department` 与 `$self.employee_no`。
3. 常设权限：先用 YAML 规则加网关内的简单求值，接口按"输入身份 + 申请，输出上限"设计，日后可换策略引擎。
4. 组织审批路由：替换单一 bob。
5. 管理员或系统吊销入口，以及身份事件订阅。

第 1–3 步改动授权核心与 catalog 契约，属核心架构变更，需作者裁决；不得在 p10-r2 上进行。
