# KB-1 实验设计（K4.1，2026-10-17 作者认可）

状态：v1，2026-10-16 起草；**2026-10-17 作者认可**（"我同意k4.1"，台账 KB1-LOOP-79），并定正式轮按方案 C 执行，见
docs/kb_extension_plan.md 阶段 4b；正式轮的执行规程待补为本文"正式轮"一节（K4.8），预登记的问题、预期与停止规则不改。
起草时的状态：草案 v1，2026-10-16。证据政策按 docs/kb_extension_plan.md §0：campaign_class `kb-pilot`、`publication_eligible: false`、
三部署重复、登记到新文件 `evaluation/final-v5-wsl2/kb-pilot-evidence-v1.json`，宏由 `paper/tkde/generate_kb_evidence.py` 产出
（封印的 pilot-evidence-v1.json 与 generate_evidence.py 不动）。停止规则与阈值用保守先验，写在执行之前，不以实测拟合。
前置：K3.3（执行接入，§9.7 已定方案 A，已落地）与 §9.8（Result 单元格取 text_sha256，2026-10-17 作者同意，已落地）；语料已冻结（manifest-v1.json，
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

**预登记修订（2026-10-16 23:1x，E-KB1 首次运行前写定，台账 KB1-LOOP-57）**：
1. 预算单位改为"单元格"：实现的通用代数把每个命中块记为 4 个 Release 单元格（doc_id / version / chunk_index / text，kb-design §5 实测），
   原 B_R = 200 的量级含义是"约 200 块 ≈ 部门可申请块的 6%"，故 B_R = 800 单元格、B_D = 2,400（保持 3 × B_R 的比例），B_O = 64、B_Q = 256
   不变。这是单位换算，不是按结果调参。
2. S2 的分区属性：`labels has` 在已实现的检索准备语言之外（text_set 无计划渲染，关闭式拒绝），分区改为 job_role（5 值）× valid_until
   月份桶（按部门语料的日期范围等分 6 桶，用 `>=` / `<` 两个日期原子），共 30 个分区，种子决定扫描顺序。
3. 合法任务用 k = 5（员工式提问），每题 ≤ 2 次 search + 1 次 aggregate，完成判据不变；问题取 E-KB4 冻结查询集中该部门的 verbatim 问题前 10 条，
   聚合题为该部门 contract.amount 的 sum，与直连 SQL 比对。
4. 部门随种子轮换（sales / finance / hr），主体 clearance = {public, internal}；可申请块 = 该部门 classification ∈ {public, internal} 的块数（从库中实测）。



- 问题：持有合法部门授权的 agent 用多次检索搬空部门库时，逐次 ACL 与 FactGate 各把累计外泄限制在什么量级；合法任务在
  同一预算下完成率多少。
- 臂：(A) 逐次 ACL 基线——直接对 kb.chunks 以同一 (kb_department, clearance) 过滤执行精确最近邻 top-k，无账本；
  (B) FactGate——经网关 search/fetch，预算 kb-pilot-v1。
- 策略（每步一次请求，最多 400 步或预算耗尽/枚举完成）：S1 词汇扫描（查询文本依次取部门主题词与模板词，k = 20）；
  S2 过滤分区（按 labels、valid_until 月份、job_role 组合分区后扫描）；S3 邻接取回（命中后 fetch 同文档其余块）。
  修订（2026-10-17，作者定 kb-design §9.6 (a) 之后）：`fetch` 只接受本根族已返回过的块，S3 在网关臂下的"取同文档其余块"
  按设计被 `CHUNK_NOT_RETURNED` 拒绝且不计费；S3 保留为策略并照常运行，网关臂的 S3 曲线因此度量的是"邻接取回被封闭后
  仅靠 search 的外泄"，拒绝次数计入轨迹。ACL 臂不受影响。
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
  **可行性**：10^6 档原以为取决于 kb-design §9.8（Result 单元格改 text_sha256）。§9.8 已于 2026-10-17 实施，实测发布峰值
  RSS 只降约 16%（96k 块 4.70 → 3.97 GiB，每增一块约 43 KiB），外推 10^6 块约 41 GiB，仍超本机 29.4 GiB。
  **作者 2026-10-17 定：10^6 档不做，论文写明未评估并报发布内存斜率（kb-design §9.8 选项 iii，台账 KB1-LOOP-77）。**
  本段对 10^6 档的预期因此未经检验，不进论文。
  （2026-10-17 更正：同日早先写在此处的"发布物体积不再是 10^6 档的障碍"是未经实测的错误判断，见台账 KB1-LOOP-75 与
  kb-design §9.8 实测段。）
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

**结果登记（2026-10-16，台账 KB1-LOOP-41 试跑 / KB1-LOOP-50 正式）**：v2 + bge-m3，k=20，identity_rate / mean_recall / short（200 条）：

| 过滤 | iterative_scan | ef_search=40 | ef_search=200 |
|---|---|---|---|
| 无 | off | 0.964 / 0.997 / 0 | 0.990 / 0.999 / 0 |
| department | off | 0.577 / 0.788 / 83 | 0.866 / 0.968 / 19 |
| department | relaxed_order | 0.938 / 0.995 / 0 | 0.954 / 0.997 / 0 |
| department+classification | off | 0.157 / 0.481 / 168 | 0.594 / 0.827 / 70 |
| department+classification | relaxed_order | 0.848 / 0.988 / 0 | 0.888 / 0.993 / 0 |

精确臂 p50 时延 56 / 18 / 12 ms（无 / 部门 / 部门+密级），HNSW 臂 1.3–3.2 ms。对照预登记：方向一致；"department 过滤下明显下降"在 v2 上出现
（v1 未出现），严过滤下更重。报告：~/logs/kb1/k34-hnsw/report-v2.json（NAS kb-k34-hnsw/）。

**修订 2 结果登记（2026-10-17，台账 KB1-LOOP-75）**：同一命令在修订 2 发布物（向量全部取自按内容摘要键控的缓存）上重跑，
k=20，identity_rate / mean_recall / short（200 条）：

| 过滤 | iterative_scan | ef_search=40 | ef_search=200 |
|---|---|---|---|
| 无 | off | 0.964 / 0.997 / 0 | 0.990 / 0.999 / 0 |
| department | off | 0.572 / 0.787 / 83 | 0.871 / 0.969 / 19 |
| department | relaxed_order | 0.933 / 0.994 / 0 | 0.959 / 0.997 / 0 |
| department+classification | off | 0.157 / 0.482 / 166 | 0.599 / 0.827 / 69 |
| department+classification | relaxed_order | 0.838 / 0.987 / 0 | 0.883 / 0.992 / 0 |

与修订 1 相比 identity 最多差 1.0 个百分点、recall 最多差 0.003；精确臂 p50 59.8 / 18.5 ms（无 / 部门）。报告
evaluation/kb-pilot/k34-hnsw/report-v2r2.json（注册表 k34-hnsw-v2r2，NAS kb-k34-hnsw/）。

## 正式轮（方案 C）规程（K4.8，2026-10-17 起草，开跑前冻结）

本节规定方案 C 的正式轮怎么跑。上文预登记的问题、预期与停止规则不变；本节只规定部署、驱动、审批、证据与签署。开跑前的任何
修改都记台账；开跑后不改。

### 1. 标准与总体做法

- 与论文里 V5 正式证据同一标准：每次部署用冻结的提交全新拉起整套服务；请求经网关 HTTP 接口（MCP `/mcp`，bearer token）发出；
  任务经 `request_data_task` → OA 提交 → 审批人批准 → 回调激活；三次重复；每部署留逐文件摘要的部署记录；封存后镜像 NAS
  并登记；作者签署证据绑定。
- 不并入 V5 publication 类：它写死 V5 的 172 格与 11 个 profile，并入会改协议、master catalog 与 registry 的冻结摘要。另建并行
  的 KB runner，复用 V5 的保证件：launcher 闸门与 WSL preflight；提交冻结的宿主构建（-trimpath，构建清单记提交、二进制摘要、
  全部受控文件摘要）；正式网关镜像 build + verify；每部署唯一的 compose 项目名与主机预检；零残留清理；凭据只在进程内读取；
  驱动 stderr 闸门；NAS 镜像。
- 试点结果不改标签；正式证据只来自按本规程跑的一轮。

### 2. 冻结与作者批准

- 提交冻结：全部代码与配置定稿后的一个提交，HEAD 等于 origin，工作树干净。运行期间不改任何受控文件；台账备注先写
  `~/logs/pending-registration.md`，封存后再登记。
- KB 数据绑定，开跑前由作者批准其 SHA-256：两份语料文件及其 manifest、三份 publish 规格（入库）、嵌入模型权重摘要、嵌入缓存
  内容清单摘要、嵌入服务代码（入库）与 Python 环境清单摘要、两个镜像摘要（pgvector 业务库、嵌入服务基础镜像）、查询集清单、
  四个 profile Catalog 与主体属性文件的摘要。该文件作为正式网关构建的 `-dataset-binding` 封进构建清单。
- 发射前通知作者，作者同意后才启动。

### 3. Profile 与部署

- 四个 profile，各是一份只含 KB 产品的 Catalog，确定性生成、入库，摘要登记在 KB profile registry：
  1. kb-v2-pilot：v2 发布物；预算 kb-pilot（查询 256，Release 800，Dependency 2,400，Outcome 64，TTL 30 min）；
     跑 E-KB1、E-KB2(a)、E-KB2(b)、E-KB5 文档级。
  2. kb-v2-large：v2 发布物；预算 kb-large（查询 100,000，Release 10^8，Dependency 10^9，Outcome 10^7，行 10^7，单次超时 60 s，
     TTL 12 h）；跑 E-KB3 的 48k 档与 E-KB4。
  3. kb-20k-large：20k 发布物；预算 kb-large；跑 E-KB3 的 96k 档。
  4. kb-v2c-pilot：v2c 发布物（counterparty 为集合级）；预算 kb-pilot；跑 E-KB5 集合级。
- 每个 profile 重复 3 次，共 12 次全新部署。每次部署：compose.yaml 加 KB 叠加层，含控制库；业务库（pgvector 镜像按摘要钉住，
  `KB_REQUIRE_PGVECTOR=1`）；MinIO；OA；嵌入服务容器（基础镜像按摘要钉住，只读挂载冻结的 Python 环境、模型与服务代码，
  `/model` 报告的权重摘要须等于绑定值）；KB 装载作业（用冻结提交构建的 kb-publish 向新业务库发布，语料与缓存只读挂载并先核
  摘要，发布清单摘要须等于 Catalog 绑定值）；KB sidecar 安装；正式网关镜像（`GATEWAY_SNAPSHOT_ARTIFACT_DIR` 与
  `GATEWAY_KB_ARTIFACT_DIR` 分开挂载）。
- 业务库 schema 摘要在冻结前由一次参考部署测得并写入 profile Catalog；此后每次部署的实时 attestation 须等于它。
  参考部署即 pilot 类部署加 `KB_FORMAL_RECORD_ATTESTATION=1`：测得后经 `kb-formal-profile -attest -record` 写入
  profiles/schema-attestations.json，复测须相同，随即拆除，记录 outcome 为 reference；之后重新生成 profile 并入库。
- 实现说明（2026-10-17，开跑前定，台账 KB1-LOOP-85/86）：KB 装载作业用 `bin/` 里冻结提交构建的 kb-publish 主机二进制执行，
  不另起容器；装载前核语料摘要等于 publish 规格，装载前后各取一次嵌入缓存内容摘要（全部向量文件按路径排序求 sha256）且须
  不变，即缓存事实上只读；复现出的发布物文件须与冻结发布物逐字节相同，清单摘要须等于 registry 值。单次部署由
  `evaluation/kb-formal/scripts/run-deployment.sh` 执行，整轮由 `run-round.sh` 按重复次序（每次重复先走遍四个 profile）执行。

### 4. 主体与审批

- E-KB1、E-KB2(b)、E-KB3、E-KB4、E-KB5 的任务由 alice 发起。alice 的属性为 job_family finance_controller，clearance 覆盖实验
  所用的密级，因此可以申请任一单个部门；每个任务的授权范围仍按实验限定在单个部门，与预登记一致。alice 在 OA 提交，bob 批准；
  E-KB2(b) 由 bob 把 TTL 缩小到 120 s 后批准。
- E-KB2(a) 的 60 个主体由网关启动时从主体文件引导。主体文件只含 token 的 SHA-256；明文 token 每部署新生成，只在进程内与
  0600 私有文件里。60 个主体的属性写在 profile 的主体属性文件里。E-KB2(a) 只走到草稿（AWAITING_SUBMISSION），不需要 OA 账户。
- 网关的主体文件功能（`PRINCIPALS_PATH`）是本轮前置的代码改动，单独测试、入库。

### 5. 驱动

- 外部驱动程序用冻结提交构建（-trimpath，带构建清单），经 MCP over HTTP 调用网关。E-KB1–E-KB5 的实验逻辑与进程内 runner
  共用一个包，只换客户端。直连臂用业务库只读账号，凭据在进程内读取；嵌入用部署内的嵌入服务。
  **实现偏离（2026-10-17，开跑前定，台账 KB1-LOOP-85）**：实验逻辑逐步移植进 `evaluation/kbformal`，已登记的进程内 runner
  不改；等价性改由对拍证明：正式轮的输入（发布物、种子、查询集、部署序号 1–3）与试点完全相同，§6 对拍覆盖三个部署序号，
  即覆盖正式轮的全部输入。E-KB3 的 client_ms 保持试点含义（客户端拿到全部行：直连臂为取尽结果，网关臂为带内联行的工具
  响应），另记 delivered_ms（再加回执验签、制品交付与逐行核对）。
- 驱动对每次工具调用、OA 动作与制品下载写一行逐请求记录（`-request-log`，JSONL）：主体标签、工具、任务与请求标识、参数
  摘要、结果与错误码、计费与回执摘要、耗时；不写 token、口令、正文、向量与签名下载地址。
- 每个 search / fetch 响应都核对：回执验签（query receipt keyring）、回执里的 Catalog 摘要等于 profile 摘要、交付正文的
  sha256 等于结果里的摘要。
- 驱动 stderr 必须为空；安全诊断写进报告。

### 6. 对拍（K4.9 验收）

- 正式轮之前，在一次非正式部署上用外部驱动跑全部实验，与进程内 runner 在同一发布物上的报告逐项比对：计数、命中集、
  Release / Dependency / Outcome、拒绝码与终止码必须一致；时延不比。
  实际做法（2026-10-17）：pilot 类彩排跑满 12 次部署，`evaluation/kb-formal/scripts/crosscheck.py` 对每次部署的每份报告与
  试点报告中同一部署序号的部分逐项比对，任一差异即停。

### 7. 证据、封存与登记

- 每次部署留：环境采集、正式网关运行时身份、嵌入服务身份、KB 数据身份（发布清单、schema 摘要）、网关诊断里的 Catalog 摘要、
  各实验报告与逐请求记录、清理记录，以及列出每个文件 sha256 与大小的 deployment-record.json。部署结束即镜像 NAS，并在 NAS
  上复算摘要。
  对应文件：environment.json（内核、Docker、镜像 ID、重启次数、覆盖文件摘要）；gateway-identity.txt（镜像内记录的二进制、
  源提交、构建上下文与源清单摘要，及运行中二进制的实算摘要，二者须相同）；embedding-model.json 与
  embedding-venv-identity.json；artifacts.sha256、schema-attestation.txt、embedding-cache.sha256；
  gateway-diagnostic-start.json 与 -end.json（admin profile-activation 诊断，Catalog 摘要须等于 profile，前后进程 nonce
  相同）；reports/<实验>.json 与 .requests.jsonl；cleanup.json；deployment-record.json。部署的签名私钥、口令与 bearer token
  只在 0600 的 env 与 token 文件里，拆除后即删除，从不镜像。
- 12 次部署全部通过后，KB finalizer 复核全部部署记录并按实验汇总，create-exclusive 写 campaign-evidence.json；登记到
  `evaluation/kb-formal/kb-publication-evidence-v1.json`（campaign_class `kb-publication`，publication_eligible true）；
  `generate_kb_evidence.py` 增加正式类加载器，只认封存后的证据。作者对登记签署批准记录。

### 8. 失败规则

- 任一部署出现以下情况即失败，整轮记为 incomplete：Catalog、发布清单、schema 或嵌入身份的摘要不符；回执验签失败；交付正文
  摘要不符；驱动 stderr 非空；清理有残留；预登记停止规则以外的异常终止。不续跑，换新 campaign ID 整轮重跑；失败与无效记录
  全部保留。
  每次运行是一次 attempt（deployments/<profile>/<NNN>/attempt-<n>），失败的 attempt 同样写记录并镜像 NAS、从不覆盖。
  只有 pilot 类允许同一部署位置的第二次 attempt；正式类的脚本拒绝第二次 attempt。

### 9. 工作量（读代码估计，未细算）

网关主体文件；实验逻辑抽成共用包并做进程内回归；MCP 客户端与 OA 审批流程；profile Catalog 与属性文件生成；KB compose 叠加层、
嵌入服务容器与装载作业；KB runner（闸门、构建、部署循环、记录、清理、镜像）；finalizer、登记与生成器正式类；非正式部署上的
对拍与修正；正式运行 12 次部署。按模块看是数天的工程量。

## 证据与宏

- 每部署目录：config.json（Catalog sha、epoch、kb-publication manifest、预算、种子）、raw/*.jsonl（逐步请求/响应/计费/拒绝码）、
  summary.json；三部署逐个镜像 NAS 并 sha256 核验。
- `evaluation/final-v5-wsl2/kb-pilot-evidence-v1.json` 登记 run id、family（kb_extraction / kb_eligibility / kb_overhead）、
  campaign_evidence_sha256、samples_sha256、submission_commit；`paper/tkde/generate_kb_evidence.py` 严格校验后出宏；
  V5 证据仍以 draft 模式（祖先校验）验证，`make paper-kb-check` 组合两者。

**实施偏差（2026-10-17 登记，台账 KB1-LOOP-75）**：上面的证据布局是运行前的计划，实际未按此实施，照实登记而不改写计划：
注册表是 `evaluation/kb-pilot/registry.json`（不在封印的 final-v5-wsl2 目录下），每个实验一份报告（各部署明细在报告的
`deployments_detail` 里），没有逐部署 raw/*.jsonl 目录，也没有 `make paper-kb-check`；宏由 `generate_kb_evidence.py` 只读注册表、
按 sha256 校验后生成，缺任一条目即失败。
