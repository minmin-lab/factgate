# 知识产品设计记录（KB-1 / K2.1）

状态：草案 v1，2026-10-15，**待作者审阅**（kb_extension_plan K2.1 BLOCKED）。分支 kb-dev。
依据：paper/tkde/supplement.tex §"Knowledge Products: Carrying the Contract to Document Access"（LOOP-53 写入）、
docs/kb_extension_plan.md、现有实现（internal/exposure/fact.go 的 FactID 编码、internal/snapshotbundle 与
internal/ordinal 的发布物与字典、internal/catalog 的 snapshot_publications、阶段 1 的 eligibility）。
原则：**凡现有机制能原样承载的，一律复用，不另起一套**；本文只记录新增的声明、绑定、身份编码、检索语言与效果定义。
每条判断标注【读论文】/【读代码】/【设计选择】；需要作者裁决的点集中在 §9。

## 0. 一句话

把一个文档集合冻结成三张关系（documents / chunks / extracted_values），用**现有** V4 snapshot publication 编译器
发布成三个 Product；检索工具 `search` / `fetch` / `aggregate` 是三个"降级器"，把请求规范化成封闭形式后在受信任只读
引擎（PostgreSQL + pgvector）内执行，再把返回的块、文档与抽取值映射成**现有** V2/V5 Fact 身份交给**现有**三维结算。
契约 T=(P,S,B,C)、账本、重放、回执、委托一行不改。

## 1. 治理单位与 Fact 身份

【读论文】补充材料定义四层：文档版本、分块、内容事实、派生内容；正文覆盖第 2–3 层。
【设计选择】三层各对应一张冻结关系与一个 Product（命名空间 ν 下三个 source_namespace）：

| 层 | 关系（kb.* 表 → reporting 视图） | entity_key | 字段 | 论文身份 | 实现身份（现有构造器） |
|---|---|---|---|---|---|
| 文档 | `kb.documents` → `reporting.<kp>_documents` | (doc_id, version) | doc_id, version, title, document_type, department, classification, labels, valid_from, valid_until, content_sha256, chunk_count | f_doc = ⟨ϖ,ν,s,d,v⟩ | `NewBaseRowFactV2(ν_doc, s, C(d,v))` |
| 分块 | `kb.chunks` → `reporting.<kp>_chunks` | (doc_id, version, chunk_index) | doc_id, version, chunk_index, text_sha256, text, department, classification, labels, valid_until, embedding（不发布） | f_chunk = ⟨ϖ,ν,s,d,v,c,C(text_c)⟩ | 行 Fact `NewBaseRowFactV2(ν_chunk, s, C(d,v,c))` + 单元格 Fact `NewBaseCellFactV2(ν_chunk, s, C(d,v,c), "text_sha256", text, sha256(text))` |
| 内容事实 | `kb.extracted_values` → `reporting.<kp>_values` | 文档级：(doc_id, version, field)；集合级：(field, canonical_value) | doc_id, version, document_type, field, value_type, canonical_value, department, classification | f_val = ⟨ϖ,ν,s,d,v,a,τ,C_τ(x)⟩ 或集合级 ⟨ϖ,ν,s,a,τ,C_τ(x)⟩ | `NewBaseCellFactV2(ν_val, s, key, field, τ, x)`，τ 用现有 `CanonicalSQLValue` |
| 派生 | 不落表 | — | — | f_gen = ⟨ϖ,B,H(answer),CW(W)⟩ | `NewDerivedFactV2(bundle, outputRowKey, expr, τ, value, witnessCommitment)`（aggregate 由现有聚合路径产生；生成回答 KB-1 **不实现**） |

- ϖ 沿用 `taskgate-exposure-v5`，**不开新的 exposure profile 与哈希域**【设计选择】：知识 Fact 与报表 Fact 由 ν 区分，
  可共享一个账本（论文"durable key = SHA256(d_R ‖ Canon(f))"的要求由现有 `FactID.Hash()` 满足）。
- 分块的 Result 身份取**文本单元格 Fact**（含 C(text_c)），与论文 f_chunk 一致；分块行 Fact 与文档行 Fact 作为依赖。
  由此 Dependency 计数比论文纸面例子多出"分块行"一项（见 §9 决策 2）。
- 集合级去重（同一金额在三份文档中出现只算一个 f_val）是 Catalog 的 `extraction_rules[].scope: collection` 声明，
  不是运行时选择【读论文】。实现上集合级字段单独落一张关系（entity_key=(field, canonical_value)）。

## 2. 索引发布期 s 与绑定内容

【读论文】发布期绑定：集合、切分函数摘要、嵌入模型/分析器版本、每块 id 与内容摘要、每块标签、抽取值、序数字典；签名
Catalog SHA 承诺全部。重建即新发布期，不改旧发布期。

【设计选择】复用 `snapshot_publications`：三张关系各一个 V4 publication（HOT/COLD/sidecar、`manifest_digest`、
`dictionary_digest`、`sidecar_digest`），由现有 `internal/snapshotbundle` 编译。新增一个**索引发布清单**把它们捆成一个
发布期：

```json
{
  "version": "taskgate-kb-publication-v1",
  "knowledge_product": "policy_kb",
  "epoch": "policy-kb-2026-10-v1",
  "collection_digest": "<sha256: 全部 (doc_id, version, content_sha256) 排序后>",
  "segmentation": {"function": "fixed-window-v1", "window_chars": 800, "overlap_chars": 0, "digest": "<sha256 of canonical JSON>"},
  "embedding": {"model": "<name>", "version": "<ver>", "dimension": 1024, "quantization": "int16-fixed-point-v1", "digest": "<sha256 of canonical JSON>"} ,
  "extraction_rules_digest": "<sha256 of canonical JSON of rules>",
  "label_vocabulary_digest": "<sha256>",
  "publications": {"documents": "<manifest_digest>", "chunks": "<manifest_digest>", "values": "<manifest_digest>", "collection_values": "<manifest_digest>|null"},
  "counts": {"documents": 10000, "chunks": 0, "values": 0},
  "manifest_digest": "<sha256 of canonical JSON of all above fields>"
}
```

- `cmd/kb-publish`（K2.3）生成：同输入两次运行 `manifest_digest` 相同；篡改任一块的文本或标签会改变对应 publication 的
  digest 进而改变 `manifest_digest`（K2.3 验收）。
- 嵌入向量**不进入** Fact 身份也不进入 HOT 字典（身份由内容摘要定）；嵌入模型版本进入发布期摘要，换模型即新发布期。
- 别名（如 `kb-latest`）在审批时解析为具体 epoch，与报表别名同构【读论文】；KB-1 只实现显式 epoch。

## 3. Catalog 声明

【设计选择】新增顶层 `knowledge_products`，引用三个普通 Product（它们各自是 V4 publication Product，复用全部既有校验）：

```yaml
knowledge_products:
  - name: policy_kb
    description: 部门制度、合同、审批单与报告
    sensitivity: high
    source_namespace: kb.policy                 # ν；三个 Product 的 source_namespace 须以它为前缀
    products:
      documents: policy_kb_documents
      chunks: policy_kb_chunks
      values: policy_kb_values                  # 可省略 = 不做第 3 层记账
      collection_values: policy_kb_cvalues      # 可省略
    index_publication: policy-kb-2026-10-v1     # 对应 kb_publications[].epoch
    document_types: [contract, approval, policy, report]
    attributes:                                 # 可过滤属性词表 = 过滤原子的 f
      - {name: department, type: enum, scope: department}      # 绑定到 Catalog Scope → 强制范围注入
      - {name: classification, type: enum, allowed_values: [public, internal, confidential, leadership]}
      - {name: job_role, type: enum, allowed_values: [staff, manager, director, executive, hr]}
      - {name: labels, type: text_set}
      - {name: valid_until, type: date}
    segmentation: {function: fixed-window-v1, window_chars: 800, overlap_chars: 0}
    embedding: {model: <name>, version: <ver>, dimension: 1024, quantization: int16-fixed-point-v1}   # 可省略 = 仅词法检索
    extraction_rules:
      - {document_type: contract, field: amount, type: numeric, source: frontmatter, scope: document}
      - {document_type: contract, field: counterparty, type: text, source: frontmatter, scope: collection}
      - {document_type: approval, field: approved_amount, type: numeric, source: frontmatter, scope: document}
    retrieval:
      k_max: 20
      exact: true                               # 精确最近邻为默认；hnsw 作可选臂（K3.4）
      aggregates: [count, sum, min, max]        # 只对 values/collection_values 产品
      generation_admitted: false
    scopes: [department]                        # 强制范围（沿用 Scope 机制）
    eligibility: {...}                          # 与 Product 相同（阶段 1）
kb_publications:
  - epoch: policy-kb-2026-10-v1
    knowledge_product: policy_kb
    manifest_digest: <sha256>
    segmentation_digest: <sha256>
    embedding_digest: <sha256>
    extraction_rules_digest: <sha256>
```

校验（K2.4）：三个 Product 存在、均为 V4 publication、source_namespace 前缀一致、`values` 产品字段含
`field/value_type/canonical_value`；`attributes[].scope` 必须是本 knowledge product 的 mandatory scope 且为 enum；
`extraction_rules` 的 document_type ∈ document_types、field 唯一、type ∈ 现有 CanonicalSQLTypeV2 可接受集合；
`retrieval.aggregates ⊆ {count,sum,min,max}`；`kb_publications` 的 digest 与 kb-publish 输出一致才能激活，
激活失败即拒启动（与 snapshot registry 同一失败关闭语义）。

## 4. 封闭检索语言（降级器输入）

【读论文】三种形式；OR、脚本/表达式、正则与通配、more-like-this、不报告所读内容的重排器关闭式拒绝；强制范围以过滤原子注入。

```json
{"tool": "search",    "task_id": "...", "request_id": "...", "knowledge_product": "policy_kb",
 "filters": [{"attribute": "department", "op": "=", "value": "销售部"}, {"attribute": "valid_until", "op": ">=", "value": "2026-10-01"}],
 "query": {"text": "Q3 pricing policy"}  | {"vector": [..]},  "k": 8}
{"tool": "fetch",     "task_id": "...", "request_id": "...", "knowledge_product": "policy_kb",
 "chunks": [{"doc_id": "...", "version": 3, "chunk_index": 2}, ...]}
{"tool": "aggregate", "task_id": "...", "request_id": "...", "knowledge_product": "policy_kb",
 "filters": [...], "field": "amount", "aggregate": "sum"}
```

规范形（h_N 的原像）：
- 过滤原子：`(attribute, op, canonical_literal)`，op ∈ {=, <>, <, <=, >, >=, in}（`in` 展开为同字段多原子，与现有
  predicate-footprint 的 IN 处理一致）；字面量按属性类型用现有 `CanonicalSQLValue` 规范化；原子按 (attribute, op, literal)
  字节序排序去重；强制范围与资格包络注入后再排序，调用方原子只能与之相交。
- 查询承诺：文本 → Unicode NFC → 大小写折叠 → 空白折叠为单空格并 trim → sha256；向量 → 维度核对 → int16 定点量化
  （scale 入 embedding.quantization）→ 小端字节串 → sha256。文本与向量二选一。
- k：1 ≤ k ≤ k_max 的整数。
- fetch：块 id 列表排序去重；每个 id 必须曾作为本根家族某次 `search`/`fetch` 的返回出现（Control 库查 root 家族已释放
  的 chunk 身份），否则 `CHUNK_NOT_RETURNED` 拒绝。
- 拒绝码（新增，发射端与校验端同时登记）：`RETRIEVAL_FILTER_UNSUPPORTED`（OR/通配/正则/未知属性/未知算子）、
  `RETRIEVAL_QUERY_UNSUPPORTED`（脚本、more-like-this、重排器、文本与向量同时给出、维度不符）、`RETRIEVAL_K_EXCEEDED`、
  `CHUNK_NOT_RETURNED`、`AGGREGATE_UNSUPPORTED`（字段非抽取字段或聚合不在白名单）。等价输入（别名、原子顺序、空白与
  大小写差异、`in` 与多个 `=`）必须得到同一 h_N（K3.1 验收）。

## 5. 三维效果定义（降级器输出 → 现有结算）

记 y 为返回块集合，D(y) 为其文档集合，A(q) 为调用方原子集合（不含注入的强制范围原子——与现有 V5 "caller-controlled"
口径一致【读代码】fact.go PredicateAtomFactV5 注释）。

| 工具 | F_R | F_D | F_O |
|---|---|---|---|
| search | {text 单元格 Fact : chunk ∈ y} | F_R ∪ {chunk 行 Fact : chunk ∈ y} ∪ {f_doc : d ∈ D(y)} | A(q) ∪ {c_q} |
| fetch | 同上（对请求的块） | 同上 | {c_q}（无原子：块 id 不是调用方字面量而是先前结果） |
| aggregate | {派生 Fact：见证 W = 匹配的 values 单元格} | 匹配的全部 values 单元格与其文档行 Fact（现有聚合依赖口径） | A(q) ∪ {c_q} |
| 空结果 | ∅ | ∅ | A(q) ∪ {c_q} |

- 复合 c_q 用现有 `CompositeOutcomeFactV5`：`QueryNormalFormVersion = "taskgate-retrieval-nf-v1"`、
  `QueryNormalFormSHA256 = h_N`、`ResultObservationSHA256 = h_y`（返回块身份哈希排序后的 sha256，复用
  `ReleaseOutcomeDigest` 形态）、`VisibleRows = |y|`、谓词集合摘要用现有 `PredicateSetHashV1`。
- 原子用现有 `PredicateAtomFactV5`：`SemanticProductID = <knowledge product>`、`StableRole = chunks|values`、
  `PublicFieldID = attribute`、`SQLType = 属性类型`、`Operator/CanonicalLiteral` 同 SQL 路径。
- 结算：三维新颖度、预算检查、重放（同 h_N 且同 h_y → 0/0/0）、回执、委托共账全部走现有 `query_sql` 之后的结算路径
  【读代码】（gateway 的 reserve → derive → settle）。aggregate 直接就是对 `<kp>_values` Product 的现有 `query_sql`
  （`SELECT sum(canonical_value::numeric) ... WHERE field='amount' AND <filters>`）经 MCP 工具包装，不新写结算。
- 近似检索（HNSW）下同一 h_N 可能得到不同 y：复合不同 → 记为新复合；R/D 不受影响【读论文】。KB-1 默认精确检索
  （`ORDER BY embedding <=> q` 全扫描 / `ts_rank` 精确），HNSW 只作 K3.4 的对照臂并记录命中集差异率。

## 6. 任务契约与资格

T 不变。P：请求命名知识产品、属性范围与目的，先经阶段 1 的 eligibility 编译包络（`knowledge_products[].eligibility`
与 Product 同一结构与代码路径）；S 绑定 `kb_publications[].epoch` 的 manifest_digest（进入 Grant 的 catalog 绑定，
与 snapshot publication 的 digest 绑定方式相同）；C 绑定 segmentation/embedding/extraction_rules 三个摘要、k_max、
聚合白名单、generation_admitted；B 为同一三元组与 B_Q。临时授权 = 任务 TTL + 属性有效期窗口（阶段 1 已实现）。

## 7. 部署位置与数据库

- 业务库：PostgreSQL 16 + pgvector（compose.yaml 的 business-postgres 镜像 `pgvector/pgvector:pg16`，2026-10-15 已可拉取）。
  表定义在 `db/init/kb-schema.psql`，由 `db/init/30-kb-schema.sh` 在 pgvector 可用时安装（`KB_REQUIRE_PGVECTOR=1` 时缺失
  即初始化失败；研究叠加层钉 plain postgres 时跳过）；`internal/kbschema` 的测试对独立 pgvector 容器应用同一文件。
  `kb.*` 表只由 `cmd/kb-publish`（管理员连接）写入；`gateway_reader` 对 `kb.*` 五表只读（search 在库内做精确最近邻需要读
  `kb.chunks.embedding`，故授在表上而非仅 reporting 视图）。分块级 department/classification/job_role/labels 允许
  "领导层专属章节"比所属文档更严格。
- 检索执行在业务库内完成（受信任只读执行器），网关只拿到块 id、文本与属性；向量不出库。
- 直连向量库的客户端绕过账本，与直连数据库相同【读论文】。

## 8. 与论文的一致性

- §1–§6 的定义与补充材料一致，差别只有两处需同步：(a) F_D 含"分块行 Fact"（§9 决策 2）；(b) 复合与原子直接复用
  V5 编码而非论文记法中独立的 k_a/k_c 常量——论文记法是抽象的，实现用 `Kind` 区分，不冲突。
- 三维效果、重放、委托的属性测试与 TLA+ 模型对知识 Fact 原样成立，因为知识 Fact 只是 ν 不同的 V2/V5 Fact（K3.3 验收
  的依据）。
- 可携带性表、加权新颖度、非声称均不受本设计影响；加权新颖度 KB-1 不实现。

## 9. 需要作者裁决

1. **复用 V4 publication 承载三张关系**（推荐）还是为知识产品另写发布物格式？复用的代价：documents/chunks/values 要以
   Product 形式出现在 Catalog（可由 kb-publish 生成片段），收益：编译器、字典、bitmap 结算、激活失败关闭全部免费。
2. **Dependency 是否计入分块行 Fact**：A（推荐）按通用代数原样发出（纸面例子 4 块 2 文档 → D=10 而非 6，补充材料例子随
   实现重算）；B 在降级器里抑制分块行 Fact 以贴合论文例子（特殊分支，违背"改主张去适配证据"）。
3. **f_val 集合级去重**是否进入 KB-1（需第四张关系与第四个 Product）；不进入则论文"collection-scoped"一句改为未实现。
4. **嵌入模型**：KB-1 实验用确定性本地模型（候选：固定随机投影 + 词袋哈希，可复现、无外部依赖）还是真实嵌入模型
   （需下载权重、GPU/CPU 时间、结果不可逐字节复现）？效果实验（E-KB1/2/3）衡量的是账本行为而非检索质量，推荐前者并在
   论文写明。
5. **生成回答 f_gen** 明确不在 KB-1 范围（论文已写"第四层为未来工作"），确认。
6. **新拒绝码五个**与 `NOT_ELIGIBLE` 一样进入 apierr 与文档；确认命名。

## 10. 实施顺序（不等审阅即可做的部分）

K2.2 表与角色（按 §1/§7，默认不含集合级关系）→ K2.5 语料生成器（独立于裁决）→ K2.3 kb-publish → K2.4 Catalog 块与激活
→ K3。若 §9 决策 1/2/3 改变，受影响的只有 kb-publish 的输出形态与 K3.2 的 oracle 计数，表结构不变。
