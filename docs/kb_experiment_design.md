# KB-1 实验设计（K4.1，待作者审阅）

状态：草案 v1，2026-10-16。证据政策按 docs/kb_extension_plan.md §0：campaign_class `kb-pilot`、`publication_eligible: false`、
三部署重复、登记到新文件 `evaluation/final-v5-wsl2/kb-pilot-evidence-v1.json`，宏由 `paper/tkde/generate_kb_evidence.py` 产出
（封印的 pilot-evidence-v1.json 与 generate_evidence.py 不动）。停止规则与阈值用保守先验，写在执行之前，不以实测拟合。
前置：K3.3（执行接入，待 kb-design §9.7 裁决）与 §9.8（Result 单元格取 text_sha256）；语料已冻结（manifest-v1.json，
documents_sha256 2944399d…，NAS 镜像已核）。

## 共同设定

- 语料：`evaluation/kbcorpus` v1，10,000 文档 / 10,469 版本 / 48,183 块；12 部门、5 岗位、4 密级；领导层专属章节 3,583 块。
  知识产品 `policy_kb`（单 collection），强制 scope `kb_department`、`classification`。
- 主体：按 12 部门 × 5 岗位合成 60 个主体属性（`department`、`job_family`、`clearance`），`clearance` 由岗位派生：
  staff/hr → {public, internal}，manager → +confidential，director/executive → +leadership。
- 部署：三个独立 compose 部署（同 docker-compose、同 Catalog、同 epoch、同 bundle digest），每部署一次全流程。
- 预算（保守先验，不拟合）：`kb-pilot-v1` profile：B_Q = 256、B_R = 200、B_D = 600、B_O = 64、k_max = 20、TTL 30 min。
  依据：补充材料校准原型"明细阅读绑定 B_R"；一个部门（sales）在 clearance {public, internal} 下可申请块约 3,000–3,500
  （48,183 × 887/10,000 × ~0.75），B_R = 200 ≈ 6% 是"一次任务只该读少量文档"的保守上限；B_D = 3 × B_R（每块附带块行 + 文档行）；
  B_O = 64 允许约 30 次带 1 原子的不同检索。
- 检索臂的 agent 是**确定性脚本**（非 LLM），策略与种子固定，保证可复现；每策略每部署 3 个种子。

## E-KB1 自适应抽取（legal-but-exhaustive）

- 问题：持有合法部门授权的 agent 用多次检索搬空部门库时，逐次 ACL 与 FactGate 各把累计外泄限制在什么量级；合法任务在
  同一预算下完成率多少。
- 臂：(A) 逐次 ACL 基线——直接对 kb.chunks 以同一 (kb_department, clearance) 过滤执行精确最近邻 top-k，无账本；
  (B) FactGate——经网关 search/fetch，预算 kb-pilot-v1。
- 策略（每步一次请求，最多 400 步或预算耗尽/枚举完成）：S1 词汇扫描（查询文本依次取部门主题词与模板词，k = 20）；
  S2 过滤分区（按 labels、valid_until 月份、job_role 组合分区后扫描）；S3 邻接取回（命中后 fetch 同文档其余块）。
- 合法任务：10 个问题（如"找 X 主题的制度"、"列出 Y 对手方的合同金额总和"），每题最多 2 次 search + 1 次 aggregate；
  完成 = 目标块在返回集中 / 聚合值正确。
- 指标：累计不同块外泄比例（占该部门可申请块）vs 步数；账本 R/D/O 轨迹；首次拒绝步数与拒绝码；合法任务完成率。
- 预期表（执行前写定）：

| 臂 | 400 步内累计外泄比例 | 首次拒绝 | 合法任务完成率 |
|---|---|---|---|
| ACL 基线 | S1 ≥ 60%，S2 ≥ 90%，S3 ≥ 90%（枚举最终到 ~100%） | 无 | 100% |
| FactGate | ≤ B_R / 可申请块 ≈ 6%（三策略相同上界） | ≤ 12 步（S1/S2），S3 更早 | ≥ 90%（每题 ≤ 40 R） |

- 停止规则：固定步数上限 400；不因中途结果调整预算或步数。
- 结论口径：只报"在该预算下累计外泄的上界被实现并实测到"，不报"最优预算"。

## E-KB2 资格门

- 问题：(a) 包络之外的申请是否 100% 在建草稿前被拒；(b) 包络之内被误批时，预算与 TTL 下的最大外泄是多少。
- (a) 60 主体 × 申请类型 {本部门+本 clearance；他部门；本部门+更高密级；finance_controller 放宽到他部门}；每类每部署一轮。
  预期：他部门/更高密级 → NOT_ELIGIBLE 100%、无 task、无 OA 草稿；本部门本密级 → 100% 进入 AWAITING_SUBMISSION；
  controller 放宽 → 100% 进入。
- (b) 误批：审批人批准一个"本部门 + 全部 clearance 内密级"的申请（包络内但超出该任务目的）；用 E-KB1 的 S2 策略在该 Grant
  下运行至拒绝或 TTL（TTL 设 2 min 以便实测到期）；预期最大外泄块数 = min(B_R, 可申请块) = 200，TTL 到期后 0 新增。
- 指标：按类型的拒绝率、草稿数；误批臂的累计外泄与到期后请求的拒绝码（TASK_NOT_ACTIVE）。

## E-KB3 开销

- 问题：search / fetch / aggregate 经网关结算相对直连库的时延开销，在 10^5 与 10^6 块两档。
- 语料：v1 生成器参数 documents=20,000（≈ 96k 块，10^5 档）与 200,000（≈ 10^6 档）；两档各自冻结 manifest 与 digest。
  **可行性**：10^6 档依赖 kb-design §9.8（Result 单元格改 text_sha256）——现 text 入 COLD 的发布物按 48k 块 638 MB /
  编译峰值 2.1 GB 线性外推，10^6 块约 13 GB / 44 GB，超本机 30 GB；若 §9.8 否决，10^6 档改为"未评估"并在论文明写。
- 臂：直连（gateway_reader 直接执行同一过滤 + 精确最近邻 LIMIT k / 同一 IN 列表取回 / 同一 sum）vs FactGate（search / fetch /
  aggregate）；k ∈ {5, 20}；每臂每档每部署 200 轮，查询文本随机取自语料词表（种子固定）；重放轮（同请求重复）单列。
- 指标：客户端时延中位数与 p95、网关结算分量（预留/派生/结算）、每轮 R/D/O 计费、重放轮 0/0/0 与时延。
- 预期（保守先验）：精确最近邻在 10^5 档全扫描即为主要成本（≥ 数十 ms），网关结算增量约数十 ms（吞吐 pilot 单根 ~26 次/秒
  的结算成本量级），p95 不超过直连的 2 倍；10^6 档精确扫描本身达秒级，网关增量占比下降。只报测得值，不报"可忽略"。
- 停止规则：固定 200 轮/格；不因方差补轮。

## E-KB4 检索质量（2026-10-16 作者定增）

- 问题：(a) 经 FactGate 检索与直连引擎的命中集是否逐成员相同（FactGate 不改排序）；(b) 在真实感语料与 bge-m3 下，
  合法任务的 recall@k 多少，即"用户搜得到想要的文档"。
- 语料：v2（LLM 生成的中文真实感文档，与 v1 同一元数据计划，冻结 manifest-v2 + digest，NAS 唯一备份）。嵌入：bge-m3，
  权重 sha256 入发布期。
- 查询集：从语料按部门与文档类型分层抽取 300 份目标文档，对每份用模型生成 2 条自然语言问题（一条贴近原文措辞，一条改写）
  并冻结；目标 = 该文档的任一块。查询集与语料一并 digest 绑定，执行前写定。
- 臂：直连（gateway_reader 对 kb.chunks 以任务同一过滤 + bge-m3 向量精确最近邻，k ∈ {5, 10, 20}）vs FactGate `search`
  （同任务、同 k）。主体为目标文档所在部门、clearance 足够的合成主体，预算设得足够大以免 E-KB4 触发拒绝（B_R 2,000）。
- 指标：命中集一致率（逐成员比较，精确检索下预期 100%，任何差异逐条列出）；recall@k（目标文档任一块进入前 k 的比例）
  按 k、按改写/原文、按文档类型分层；MRR 作辅助。
- 预期（保守先验，不拟合）：一致率 100%；recall@10 对贴近原文的问题 ≥ 0.8、对改写问题 ≥ 0.6——低于此值说明嵌入或切分
  需要调整，而不是 FactGate 的问题；结果照实报。
- 三部署重复：同一语料与查询集，三个部署的命中集应完全相同（确定性精确检索），不同即记为缺陷。

## K3.4 近似检索对照臂：命中集差异率（2026-10-16 先登记后测）

**问题**：KB-1 默认精确检索（`ORDER BY embedding <=> q` 全扫描），HNSW 只作对照臂。要回答的是：若换成 HNSW，同一查询、同一 k、同一强制范围下，命中集与精确臂相差多少；这决定了 Outcome 重放在近似检索下失效的程度（kb-design §7）。

**工具**：`evaluation/cmd/kb-hnsw-diff`（度量实现 `evaluation/kbhnsw`，单元测试覆盖相同集合、部分重叠、短结果、边界并列、空汇总）。在业务库 `kb.chunks` 上运行：先跑精确臂（关闭索引扫描），再建 HNSW 索引（m=16, ef_construction=64，pgvector 默认值），再跑近似臂（关闭顺序扫描），同一会话、同一组查询。索引跑完即删，不改变发布物。

**查询**：从目标集合按 `md5(seed || text_sha256)` 排序取前 N=200 块，以其已存向量为查询向量，并从结果中排除该块自身。确定性：同一集合、同一 seed 得同一查询集。

**配置格**：k ∈ {5, 10, 20}（k_max=20）× `hnsw.ef_search` ∈ {40（pgvector 默认）, 100, 200} × `hnsw.iterative_scan` ∈ {off（默认）, relaxed_order} × 过滤 ∈ {无, department, department+classification}。后两种过滤对应网关的两个强制范围（kb_department、classification）。ef_search < k 的组合跳过（pgvector 把返回数上限设为 ef_search，短结果是构造使然，不是测量）。

**度量**（每配置对 200 条查询汇总）：
- identity_rate：近似臂返回恰 k 行且集合与精确 top-k 相同的查询占比；精确臂第 k 与第 k+1 名距离相等（边界并列）的查询不计入分母，单独报数——这类查询精确臂本身不定义唯一集合。
- mean_recall / min_recall：|近似 ∩ 精确 top-k| / k 的均值与最小值；
- mean_jaccard：|∩| / |∪|；
- short：近似臂返回少于 k 行的查询数（HNSW 先走索引再过滤会出现）；
- 时延只作参考（p50，毫秒），不是 E-KB3 的开销口径。

**预期（先验，写在首次运行前）**：无过滤、ef_search=200 时 recall 接近 1 但 identity_rate 不保证为 1；加 department 过滤（每部门约占集合 1/12）且 iterative_scan=off 时，默认 ef_search=40 预计出现 short 与 recall 明显下降，因为候选集在过滤前已截断；relaxed_order 应消除大部分 short。无论结果如何，差异率只进补充材料，默认臂不变；不会按结果调 k_max 或默认参数。

**首次运行目标**：kbe2e4 上的 v1 + bge-m3 发布物（collection policy_bge_v1，48,183 块）作试跑；正式数字在 v2 + bge-m3 发布物上重跑同一命令并登记。

## 证据与宏

- 每部署目录：config.json（Catalog sha、epoch、kb-publication manifest、预算、种子）、raw/*.jsonl（逐步请求/响应/计费/拒绝码）、
  summary.json；三部署逐个镜像 NAS 并 sha256 核验。
- `evaluation/final-v5-wsl2/kb-pilot-evidence-v1.json` 登记 run id、family（kb_extraction / kb_eligibility / kb_overhead）、
  campaign_evidence_sha256、samples_sha256、submission_commit；`paper/tkde/generate_kb_evidence.py` 严格校验后出宏；
  V5 证据仍以 draft 模式（祖先校验）验证，`make paper-kb-check` 组合两者。
