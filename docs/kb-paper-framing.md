# 统一框架下的论文主稿改写草案（供作者审，K5.1 执行；2026-10-16 作者定统一框架，台账 KB1-LOOP-40）

状态：草案 v1。阶段 4 完成前不改主稿正文；本稿只供作者审阅与批注。数字一律用宏（generate_evidence.py / generate_kb_evidence.py），
下文凡写 ⟨…⟩ 处为待宏填充，不手写数字。

## 1. 一句话主张

FactGate 是面向 Agent 的企业数据产品暴露治理层：无论数据产品是报表关系还是文档集合，Agent 通过它读到的每一个事实都在同一账本上
按释放、依赖、结果三维记账、预算、重放与回执；治理单位不同（单元格与行、分块与文档），机制不变。

## 2. 摘要草稿（英文，供改写参考）

Enterprise agents read two kinds of governed data: curated reporting relations and document collections. Today each kind has its own
access-control story, and neither tells an operator how much an agent has actually been exposed to across many small, individually
legitimate requests. FactGate is a gateway that treats both as *data products* with one exposure ledger. A request is compiled into a
prepared operation whose released cells or chunks, dependency rows and outcome predicates are settled against a per-task budget; repeats
settle to zero, overlapping requests pay only for novelty, and every settlement is signed into a receipt that an independent finalizer can
reproduce. We instantiate the design for SQL over published relations and for exact retrieval over published knowledge chunks, and show
that the two share the compiler, the sealed preparation boundary, the settlement and the replay machinery; retrieval adds only a query
commitment, a hit-set wrapper and the chunk's cells as facts. On a reporting corpus ⟨V5 numbers⟩ and on a ⟨N⟩-chunk knowledge corpus ⟨E-KB
numbers⟩, adaptive extraction stays within the pre-registered budget bound on both paths, legitimate tasks complete, and gateway retrieval
returns exactly the hit set of direct exact retrieval with a measured settlement overhead of ⟨E-KB3⟩.

## 3. 引言改写要点

- 开头场景从"报表查询的累计暴露"扩为"报表与文档两类数据产品的累计暴露"；文档场景给出一个具体例子（持有本部门制度库检索权限的
  Agent 用几十次合法检索搬空部门库）。
- 贡献列表改为四条：(1) 数据产品层的统一发布与封印；(2) 事实层的统一身份与账本；(3) 操作层的"准备操作"抽象——SQL 与检索是两种准备；
  (4) 两条路径的实现与评估（报表：V5 campaign；文档：E-KB1–E-KB4）。
- 非主张保留并补一条：检索质量不是本文贡献，只报告"网关不改变命中集"（一致率）与 pilot 的 recall@k 以说明可用。

## 4. 模型节改写（四层同构，对应 kb-design §9b）

| 层 | 报表实例 | 文档实例 | 共同机制 |
|---|---|---|---|
| 数据产品层 | 关系（reporting.*） | 文档集合（documents / chunks / values） | 封印发布物、激活失败关闭 |
| 事实层 | 单元格、行 | 分块、文档、抽取值（四个可见单元格 + 行） | 同一身份编码与账本 |
| 操作层 | query_sql / execute_plan | search / fetch / aggregate | 带规范形的准备操作，同一封印边界 |
| 效果层 | R / D / O | R / D / O | 同一三维结算、重放、回执、委托 |

效果定义按 kb-design §5 的实测口径改写（Release = 四个可见单元格；Dependency 另含块行、文档键单元格、文档行、每个调用方过滤属性
一个块单元格）；补充材料现有"plain search 的 F_D = F_R 加文档 Fact"一句作废。

## 5. 实验节结构

- RQ 不变的部分：V5 campaign 的报表路径结果原样（封存证据）。
- 新增文档路径 RQ：(a) 自适应抽取在预算下的外泄上界被实现并实测（E-KB1）；(b) 资格门与误批止损（E-KB2）；(c) 结算开销（E-KB3，
  10^5 档；10^6 档视 §9.8）；(d) 网关与直连命中集一致率与 recall@k（E-KB4）。近似检索对照臂（K3.4）进补充材料（已入）。
- 证据类别：文档路径现为 kb-pilot（publication_eligible false）。进入主稿前须作者裁决翻为正式证据；否则文档路径结果只进补充材料，
  主稿按路线一措辞。

## 6. 对现有正文的改动清单（K5.1 执行时逐项核对）

1. 摘要、引言、贡献列表（§1）。
2. 模型节：Fact 定义处加文档实例；准备操作抽象一段。
3. 架构节：知识产品发布与激活、嵌入服务位置（受信边界内）、pgvector 装 pg_catalog 的原因一句。
4. 实验节：新增文档路径小节，引用 generated/kb.tex 宏。
5. 威胁模型：知识产品的两个边界（已写入 docs/threat-model.md，正文摘一句）。
6. 补充材料"知识产品"节：由"定义"改"实现并评估"、效果定义段改口径、Worked example 按实测代数重算——已完成（2026-10-17，作者定 §9.2 A）。

## 7. 需要作者决定的点

- 文档路径证据是否翻为正式（campaign_class / publication_eligible）；若翻，三部署的定义采用"独立控制库 + 独立 Service 实例 + 同一业务库"
  还是三个 compose 栈（后者需重做 E-KB1–E-KB4）。
- §9.8（text_sha256）与 E-KB3 的 10^6 档。
- 主稿页数（K5.1）。
