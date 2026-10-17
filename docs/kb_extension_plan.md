# 知识产品扩展计划（KB-1）：把 FactGate 从报表路径推广到文档与知识库

状态：草案 v1，2026-10-15，等作者审阅后用 `/loop` 启动。分支 `kb-dev`（自 p10-r2 468f87c 建）。
目标：TKDE 稿在"全部改完"后投：知识产品路径从"定义"升级为"实现并评估"（作者决策，台账 LOOP-54）。
框架（2026-10-16 作者定，台账 KB1-LOOP-40）：论文主张为"面向 Agent 的企业数据产品暴露治理"，报表与文档/知识是同一机制的两个实例，文档场景为主叙事（kb-design §9b）；K4 实验按可发表级执行，正文改写在阶段 5。

## 0. 边界与证据政策（需作者在审阅时确认）

- **封印证据不动。** V5 campaign（faf4d095）的数字、原始证据、generate_evidence.py 一律不改。
- **两个证据锚点。** 知识产品实验是第二套证据：绑定它自己的提交，由后封印生成器（generate_p10_evidence.py 同类，新文件 `paper/tkde/generate_kb_evidence.py`）出宏并做严格校验；V5 证据以 draft 模式（祖先校验）验证。论文可复现性声明改为"两个提交锚点"。
  `make paper-final-check` 在 kb-dev 上会因 measured paths 变动而失败，这是预期；改为新目标 `make paper-kb-check`（V5 draft + KB final）。
- **新实验类名** campaign_class 用 `kb-pilot`，publication_eligible 保持 false，直到作者裁决其为正式证据。 2026-10-16 作者采纳统一框架后，K4 证据按可发表级设计（三部署、冻结语料 v2 + bge-m3、预先登记预期），翻 true 仍待作者裁决。
- **主张不超证据。** 论文只说实现了什么、测了什么；未做的仍写"未评估"。
- **签署类动作永不代签**：catalog 变更凑齐后作者一次签署；Zenodo 发布只在作者明示后。

## 1. 阶段与任务清单（状态列由 loop 更新：TODO / DOING / DONE / BLOCKED）

### 阶段 1：资格先于审批（已开工，WIP 已提交）

| 编号 | 任务 | 验收 | 状态 |
|---|---|---|---|
| K1.1 | Catalog 增 `eligibility`（require 条件、scope_envelope）；严格解码与校验 | `go test ./internal/catalog/` 新增用例通过；无效声明被拒 | DONE |
| K1.2 | 新包 `internal/principalattr`：属性快照（标准属性 + 带有效期的临时授权），规范 JSON 摘要 | 单元测试：解析、摘要稳定、授权窗口 | DONE |
| K1.3 | 网关 `request_data_task` 在审批前编译包络：require 不满足或范围超出 → `NOT_ELIGIBLE`，不建草稿；委托按委托人属性检查 | gateway 测试：通过/拒绝/放宽/临时授权/过期/委托 六类 | DONE |
| K1.4 | 裁决落盘：pendingContext 增 `eligibility`（快照摘要、规则 id、包络）；manifest 不改版本 | 任务记录里可复核 | DONE |
| K1.5 | 配置与部署：`config/principal-attributes.yaml`、compose 挂载与环境变量、demo catalog 给 expense_detail 加资格；集成测试仍过 | `scripts/integration-test.sh` 通过；demo 脚本新增"非 HR 申请薪资被直接拒绝"例 | DONE |
| K1.6 | 文档：catalog-guide 新节、getting-started 一段、threat-model 更新 | 文档与代码一致 | DONE |

### 阶段 2：知识产品类型与索引发布编译器

| 编号 | 任务 | 验收 | 状态 |
|---|---|---|---|
| K2.1 | 设计记录 `docs/kb-design.md`：知识产品 Catalog 声明（集合、切分函数、抽取规则、属性词表）、索引发布期的绑定内容与摘要、Fact 身份编码（f_doc/f_chunk/f_val/f_gen）、封闭检索语言、三维效果定义 | 与论文补充材料一致；作者审阅 | BLOCKED |
| K2.2 | 业务库侧：pgvector 扩展、`kb.*` 表（documents、chunks、extracted_values、labels）、只读角色 | 迁移脚本；`db/init` 新文件 | DONE |
| K2.3 | `cmd/kb-publish`：冻结一个 collection 为发布物（块 id、内容哈希、标签、抽取值、嵌入模型版本、切分摘要），生成序数字典与清单，输出摘要写回 Catalog | 两次运行同输入同摘要；篡改一块即失败 | DONE |
| K2.4 | Catalog `knowledge_products` 块与校验；网关启动激活知识发布物 | 激活失败即拒启动 | DONE |
| K2.5 | 合成语料生成器 `evaluation/cmd/kb-corpus`：1 万份文档、12 部门、5 岗位、4 密级、自定义标签、领导层专属章节；文档类型含合同/审批单/制度/报告，带可抽取字段 | 语料冻结并 digest 绑定 | DONE |
| K2.6 | GPU 嵌入：WSL 装 CUDA 版 torch 与 nvidia-container-toolkit；嵌入提供者 `bge-m3`（权重 sha256 入发布期）接入 kb-publish；嵌入服务（受信边界内，供查询文本嵌入）；hash-projection-v1 留作测试提供者 | GPU 上 48k 块编码完成并记时；同输入两次嵌入余弦差 < 1e-4；发布期清单含模型摘要 | DONE |
| K2.7 | 真实感语料 v2：LLM 生成（元数据与可抽取字段受控），冻结 manifest + digest，JSONL 镜像 NAS（唯一备份） | 文档数、分布与 v1 口径相同或明写差异；digest 绑定；NAS sha256 核验 | DONE |
| K2.8 | 集合级抽取值（作者 2026-10-17 定）：Catalog 规则 scope=collection，第四张关系 `collection_values`（按值与强制范围去重）进发布物、片段、激活校验 | kbpublish / catalog 测试通过；v2 重发布带集合级 counterparty | DONE |

### 阶段 3：检索工具与结算

| 编号 | 任务 | 验收 | 状态 |
|---|---|---|---|
| K3.1 | 检索降级器：`search`（AND 过滤原子 + 查询承诺 + k）、`fetch`、`aggregate`；OR/脚本/通配/未知字段关闭式拒绝；强制范围注入 | 规范形对等价输入一致（别名、原子顺序）；拒绝码齐全 | DONE |
| K3.2 | 效果提取：F_R/F_D/F_O 按 kb-design 定义；aggregate 的派生 Fact 带见证集 | 独立 oracle 包逐成员核对 | DONE |
| K3.3 | 接入现有三维结算、重放、回执、委托；MCP 暴露三个工具 | 现有 TLA+/属性测试对知识 Fact 原样通过；集成测试：重放 0/0/0、拆分不多扣、子任务共账、超限扣留 | DONE |
| K3.4 | 近似检索对策：精确检索为默认；HNSW 作为可选臂记录命中集差异率 | 差异率进补充材料 | DONE |
| K3.5 | aggregate 按字段的规则 scope 路由到 values 或 collection_values | 端到端：同一聚合在两种声明下依赖集合不同 | DONE |

### 阶段 4：实验

| 编号 | 任务 | 验收 | 状态 |
|---|---|---|---|
| K4.1 | 实验设计先写台账：问题、臂（逐次 ACL 基线 / FactGate）、语料、oracle、预期表、停止规则（保守先验，不拟合实测） | 台账行 | BLOCKED |
| K4.2 | E-KB1 自适应抽取：合法部门权限的 agent 用多次检索搬空部门库；指标：累计外泄分块比例 vs 步数、合法任务完成率；三部署重复 | 曲线 + 表 | DONE |
| K4.3 | E-KB2 资格门：非资格申请被拒率 100%、误批止损（预算与 TTL 下的最大外泄） | 表 | DONE |
| K4.4 | E-KB3 开销：search/fetch/aggregate 的结算时延与直连向量库对比；10 万与 100 万分块两档 | 表 | DONE |
| K4.6 | E-KB4 检索质量：合法任务 recall@k（真实感语料 v2 + bge-m3）、直连 vs 网关命中集一致率（精确检索下应为 100%）；三部署 | 表 | DONE |
| K4.7 | E-KB5 集合级口径：同一"列出本部门供应商 / 合同对方"聚合在文档级与集合级两份发布物上的 R/D/O 与预算消耗对比 | 表 + 补充材料一句 | DONE |
| K4.5 | 证据注册与宏：`generate_kb_evidence.py`，注册到 pilot-evidence 同类登记件；论文新节由"定义"改"实现并评估"，补充材料加实验小节 | 构建通过；页数再平衡 | DONE |

### 阶段 5：投稿前

| 编号 | 任务 | 状态 |
|---|---|---|
| K5.1 | 主稿页数定稿（12 或 13，作者定）、review/ 刷新、投稿包与投稿信更新、arXiv v3 | TODO |
| K5.2 | 作者签署 catalog 与证据绑定；Zenodo 存证（作者明示后） | TODO |

## 2. 估时（我的估计，未实测）

阶段 1 一到两周；阶段 2 三到四周；阶段 3 三到四周；阶段 4 四到六周；阶段 5 一周。合计约三到四个月。

## 3. 风险

- 近似检索不确定性让 Outcome 重放失效：默认精确检索，HNSW 作消融。
- 抽取规则覆盖面决定第 3 层记账：只对可结构化文档类型声明抽取，论文如实写边界。
- WSL 内存：索引构建与嵌入在 30 GB 上限内分批；不再并发跑 10^7 行研究发布物。
- 评审认为两个场景都没做透：主稿先给 SQL 全证据，知识产品节给实现 + 三组实验，不与 SQL 争篇幅。

## 4. 硬边界（loop 任务词引用）

封印件永不改；p10-r2 分支保持 `make paper-final-check` 可过；kb-dev 上改 internal/ 等路径属预期，但 generate_evidence.py 与 evaluation/final-v5-wsl2 的封印登记件不得改；新证据类名 kb-pilot；不 relax 门禁迁就数据；签署与批准永不代签；push 带 pull --rebase 重试。
