# 专利素材一页：演示栈第 147–152 轮的决策与机制清单（2026-10-28 整理）

读者：作者与专利代理人。用途：圈定论文之外、已实现并实跑验证过的授权机制，供起草交底书。
本页只列"做了什么、在哪、怎么验的"，**不写论文没有写的保证性主张**（安全性、最优性、不可绕过等一概不写）；
每条机制标明是否已在论文（paper/tkde/main.tex）中出现。出处行号以 kb-dev 743368ae 为准，台账为 docs/codex_publication_execution_plan.md。

## A. 论文已写的基线（交底书可引用，本页不重复主张）

| 机制 | 论文位置 |
|---|---|
| 任务级授权：一次人工审批编译成签名、限额、可审计的会话（安全视图、行范围、禁字段、操作限制、回执） | main.tex:246-250 |
| 资格包络先于审批；审批人只能缩小，不能放大 | main.tex:1084-1087 |
| 签名、单调（只收窄）的委托；全体后代共用一个根账本 | main.tex:978-995 |
| 每个子授权都经人工审批；原型只有人工审批一种模式 | main.tex:978-980、991-992 |
| 跨根拆分目标的防线是每个根的审批，不是记账 | main.tex:1496-1498 |

## B. 论文未写、演示与产品已实现的机制（候选专利点）

全部挂在目录顶层块 `delegated_authorization` 之下（internal/catalog/delegated.go）；该块进目录 SHA-256，正式轮目录没有它，行为逐字节不变。

| # | 决策（作者定，日期） | 机制 | 代码出处 | 验证（台账行） |
|---|---|---|---|---|
| 1 | 审批链按密级分层升级（10-26） | 链的 `escalations` 列表：每层 role/label/scope/values/level；同一人只出现一次 | internal/catalog/approval_chain.go:37、:87 | KB1-LOOP-147 阶段 1（L2774） |
| 2 | 子任务由申请人本人确认（10-26） | `child_self_confirm`：有父任务时审批人 = 申请人；校验端对无标记的自批失败关闭 | internal/gateway/service.go:217；tasks.go | 147 阶段 2（L2788） |
| 3 | 管理员代开单（10-26；10-27 改为按数据属主部门） | `issue_data_authorization`：任务挂员工名下，发单人为链首审批人；审计事件 TASK_ISSUED_FOR | internal/gateway/issue.go；tasks.go:546 | 147 阶段 3（L2796）；148（L2865） |
| 4 | 粗粒度长期授权 = 项目（10-26；10-28 定四类） | 申请时可选 `budget_profile`；`list_my_tasks` 分项目 / 日常任务 / 子任务 / 授权单 | internal/gateway/list_tasks.go | 150（L2908） |
| 5 | 三条通用审批规则（10-27） | 个人记录自助 `personal_scope`；职权层级（属性 authority_departments / authority_center + 层 level，自权删步、跨部门换上一层）；数据管家 `steward_department`；链删空即本人确认 `self_service` | internal/catalog/delegated.go:39、:52；types.go:125；approval_chain.go:252；tasks.go:590 | 148（L2865、L2889） |
| 6 | 项目成员各自确认（10-27） | `member_confirm`：受托成员为审批人并按其资格判定，人类主体仍是负责人，回执审批人为成员 | internal/gateway/task_flow.go:35 | 148（L2865） |
| 7 | 任何场景由 agent 提交、人只审批（10-28） | `agent_submits`：建任务即待审批；OA 未按待审批建草稿则 APPROVAL_UNAVAILABLE、不建任务；审计事件 TASK_SUBMITTED_BY_AGENT | internal/catalog/delegated.go:46；tasks.go:558 | 149（L2896） |
| 8 | 一个 agent、一把令牌，令牌代表谁由人在 OA 切换，agent 自己不能切（10-28） | 网关内令牌摘要→主体绑定表 + 切换历史；OA 以签名请求切换；`whoami` 工具；每个结构化结果附 `acting_as` | internal/gateway/agent_identity_auth.go:24；agent_identity.go:109；control/migrations/023 | 151、151b（L2921–2943） |
| 9 | 目录可声明"有序"的范围维度，申请时向下补全，不改放行判定（10-28） | `Scope.order`（必须是 allowed_values 的排列）；`DownClosure`；申请响应 `scope_closure`；工具说明由目录生成 | internal/catalog/types.go:186；scope_order.go:32；gateway/scope_order.go:25 | 152（L2944） |
| 10 | 撤销根任务级联归档子任务（10-26） | `cascadeRevocation`：记账而非安全（安全仍是根账本） | internal/gateway/tasks.go:1184 | 147 阶段 5（L2805） |

OA 可用性改动（一次登录切换账号、待办/审批记录、勾选式缩小、两卡页眉）是界面工作，不列为机制。

## C. 已知限制（不要写进权利要求）

- 机制 7 去掉了申请人对 agent 请求的背书：他人审批的场景里，审批人批的就是 agent 写的范围（作者知情后照定）。
- 机制 8 的切换历史不进审计哈希链；OA 切换接口未按 event_id 去重；共用令牌下一人切换影响同时在测的其他人。
- 机制 9 的原始申请值只在响应里，不持久化；OA 草稿不标注自动补进的值。
- 子任务只收窄、放行判定、资格包络这些既有判定本轮均未改；新机制的正确性证据是单元测试、变异检查与演示栈实跑（各台账行），没有形式化证明。

## D. 本页没有的

论文主张不在此重复；对 B 节机制没有任何论文之外的保证性断言。演示栈的数字（主体数、链长、通过步数）见各台账行与 /mnt/ctg-pto-dev/factgate-evidence/kb-demo-14x、15x。
