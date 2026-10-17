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
| 集合级抽取值（可选，2026-10-17 作者定） | `kb.extracted_values` 上 DISTINCT → `reporting.<kp>_collection_values` | (field, canonical_value, kb_department, classification) | 同 values 去掉 doc_id/version/document_type | f_val 的集合级身份：同值同范围只计一次 |
| 内容事实 | `kb.extracted_values` → `reporting.<kp>_values` | 文档级：(doc_id, version, field)；集合级：(field, canonical_value) | doc_id, version, document_type, field, value_type, canonical_value（exposure 规范编码，身份用）, value_text / value_numeric / value_date（按 value_type 恰一列非空，聚合与类型化单元格用）, department, classification | f_val = ⟨ϖ,ν,s,d,v,a,τ,C_τ(x)⟩ 或集合级 ⟨ϖ,ν,s,a,τ,C_τ(x)⟩ | `NewBaseCellFactV2(ν_val, s, key, field, τ, x)`，τ 用现有 `CanonicalSQLValue` |
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

- 语料（K2.5，`evaluation/kbcorpus` + `evaluation/cmd/kb-corpus`）：文档正文 = 前言区（`---` 包围的 `field: value` 行，
  字段按文档类型固定）+ 若干 `## 标题 [密级]` 节；节级密级可为 leadership（领导层专属章节）。冻结件是提交的
  `evaluation/kbcorpus/manifest-v1.json`（seed、计数、documents.jsonl 的 sha256），JSONL 不入库、由同一 seed 重生成并与
  NAS 镜像核对。切分函数 `section-window-v1`：先按节切，节内按 ≤ 800 字符在句边界开窗，块密级 = 节密级；抽取源
  `frontmatter` 即解析前言区（`kbcorpus.ParseFrontMatter`）。
- `cmd/kb-publish`（K2.3，库 `internal/kbpublish`）：输入 publish spec（`taskgate-kb-publish-spec-v1`：知识产品、collection、
  source_id/catalog_source、ν、epoch、Catalog source 的 schema_digest、语料路径与 sha256、切分/嵌入/抽取规则），
  以管理员连接写 kb.* 并建三个 `reporting.kb_<kp>_<epoch>_<kind>` 物化视图（owner taskgate_snapshot_owner、gateway_reader
  SELECT；不含 embedding/body），再以 gateway_reader 连接调用现有 `snapshotbundle.ScanPostgresSnapshot` +
  `CompileOwnedToDirectory` 编译三个 V4 bundle 到 `<out>/bundles/<publication>/`（安装器与网关加载器的既有布局）。输出
  `<out>/kb-publication.json`（本清单）、`<out>/inputs/{documents,chunks,values}.json`（带 expected_digests 的编译器输入，
  直接交给 `cmd/snapshot-index` / `cmd/snapshot-sidecar-install`）、`<out>/catalog-fragment.yaml`（K2.4 的 Catalog 片段）。
  `kb-publish -verify -out <out>` 重扫重编译并比对全部摘要。同一 collection 不可重复发布（新构建 = 新 epoch）。
  已实测：同输入在两个库发布 `manifest_digest` 相同；篡改一块文本并刷新物化视图后 verify 失败。部署顺序：db/init →
  kb-publish → snapshot-index（kb 三个输入）→ snapshot-sidecar-install（与其他发布物同批，安装器首装即封印集合）。
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

实现备注（K2.4 已落地，权威见 internal/catalog/knowledge.go 与 docs/catalog-guide.md "Knowledge Products"）：视图把 kb.* 的
`department` 列别名为 `kb_department`，以免与 demo Catalog 既有的中文 `department` Scope 冲突；`classification` 作第二个强制
Scope，领导层专属章节由 `clearance` 属性的包络在申请时挡住。`attributes[].scope` 必须与属性同名。片段由
`catalog.MergeYAMLFragment` 追加进既有 Catalog（只追加顶层序列，不改写既有键）。
校验（K2.4）：三个 Product 存在、均为 V4 publication、source_namespace 前缀一致、`values` 产品字段含
`field/value_type/canonical_value/value_text/value_numeric/value_date`；`attributes[].scope` 必须是本 knowledge product 的 mandatory scope 且为 enum；
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
- 拒绝码（新增，发射端与校验端同时登记）：`RETRIEVAL_FILTER_UNSUPPORTED`（OR/通配/正则/未知属性/未知算子/类型不符）、
  `RETRIEVAL_QUERY_UNSUPPORTED`（脚本、more-like-this、重排器、文本与向量同时给出、维度不符）、`RETRIEVAL_K_EXCEEDED`、
  `RETRIEVAL_SCOPE_EXCEEDED`（调用方对受强制范围约束的属性给出的值与任务范围交集为空）、`CHUNK_NOT_RETURNED`、
  `AGGREGATE_UNSUPPORTED`（字段非抽取字段、类型不匹配或聚合不在白名单）。等价输入（原子顺序、同一属性 `in` 值顺序与重复、
  空白与大小写差异、单值 `in` 与 `=`）必须得到同一 h_N（K3.1 验收）。注意 `in [a,b]` 是同一属性上的析取，与两个 `=` 原子的
  合取不等价，不得归一为同一形式（2026-10-16 更正本文早先的说法）。

## 5. 三维效果定义（降级器输出 → 现有结算）

记 y 为返回块集合，D(y) 为其文档集合，A(q) 为调用方原子集合（不含注入的强制范围原子——与现有 V5 "caller-controlled"
口径一致【读代码】fact.go PredicateAtomFactV5 注释）。

| 工具 | F_R | F_D | F_O |
|---|---|---|---|
| search | {doc_id, version, chunk_index, text 四个单元格 Fact : chunk ∈ y} | F_R ∪ {chunk 行 Fact : chunk ∈ y} ∪ {f_doc, doc.doc_id 单元格, doc.version 单元格 : d ∈ D(y)} ∪ {chunk 的 a 单元格 : chunk ∈ y, a ∈ 调用方过滤属性} | A(q) ∪ {c_q} |
| fetch | 同上（对请求的块） | 同上（无调用方过滤属性项） | {c_q}（无原子：块 id 不是调用方字面量而是先前结果） |
| aggregate | {派生 Fact：见证 W = 匹配的 values 单元格} | 匹配的全部 values 单元格与其文档行 Fact（现有聚合依赖口径） | A(q) ∪ {c_q} |
| 空结果 | ∅ | ∅ | A(q) ∪ {c_q} |

- **口径来源（2026-10-16 实测后改，台账 KB1-LOOP-47）**：检索按 §10a 以 chunks ⋈ documents 的关系计划准备，F_R / F_D 就是现有
  V5 关系代数对该计划的输出，不另造：可见四列即 Release；溯源见证 = 两源的行 Fact + 连接键单元格（documents.doc_id / version）
  + 外层过滤读到的 chunk 单元格（每个调用方过滤属性一个）；注入的强制范围由策略渲染器施加，不留见证。字段身份带稳定关系角色
  （`policy_kb_chunks.text` 而非裸列名 `text`，因为是关系计划而非单源计划）。实测（kbe2e5，k=5，过滤 classification）：R=20、
  D=45=5×(4+1+1)+5×(2+1)，与 kboracle 按定义重算的计数一致；effect.go 与 oracle 已改为此口径并 200 次随机命中集逐成员一致。
  原"仅 text 单元格为 Release"的写法作废（§9.9）。
- 实现备注（K3.2 已落地：internal/retrieval/effect.go，独立 oracle evaluation/kboracle）：实体键按 snapshot 编译器的构造
  `ComposeCanonicalKeyV2("base-entity", ν.kind, 逐键字段 name/sqlType/规范值)`；分块 Result 身份 = 裸列名 `text` 的单元格
  Fact（单源计划的 field id 是裸列名）；原子算子映射到 V5 词表（`=`/`in`/`has` → EQ 逐字面量、`<>` → NE、`<`/`<=`/`>`/`>=` →
  LT/LE/GT/GE），与 SQL 路径把 IN 逐字面量记为 EQ 的做法一致；原子上下文摘要域 `TASKGATE-RETRIEVAL-CONTEXT-V1`
  （Catalog sha、知识产品、epoch、chunks/documents publication manifest、注入范围摘要）。search/fetch 响应只回
  (doc_id, version, chunk_index, text) 与名次，不回相似度分数与属性列。
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
- 嵌入服务（K2.6，2026-10-16）：受信边界内的宿主进程（~/kb-embed/service.py，FastAPI，127.0.0.1:8090，GPU），`/model`
  报告模型名/维度/权重 sha256，`/embed` 返回 L2 归一化向量。kb-publish 在发布期调用它为块编码并把三元组核对后写进
  发布期清单；网关（K3.3）在 `search` 收到文本查询时调用它得到查询向量（容器经 host-gateway 访问宿主）。查询文本的承诺
  h_N 仍取规范化文本的 sha256，不取向量；向量只用于引擎排序。嵌入服务与业务库同属受信任执行器一侧：它看到查询文本与
  块文本，但不接触账本与授权。
- pgvector 装在 `pg_catalog`（2026-10-16 实测后定，台账 KB1-LOOP-46）：连接器对每条受治理语句钉 `search_path = pg_catalog`，
  策略语言不放行带模式限定的算子，检索包裹语句里的 `embedding <=> q` 只有在 pg_catalog 里才可解析；扩展可迁移
  （`ALTER EXTENSION vector SET SCHEMA pg_catalog`）。放弃的替代：改连接器钉（被评估的语句分类器按字节绑定）、改策略分析器。
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
   **已定（2026-10-16 作者采纳统一框架，台账 KB1-LOOP-40）：复用 V4 publication。** 文档集合与关系表同为"数据产品层"的发布物。
2. **Dependency 是否计入分块行 Fact**：A（推荐）按通用代数原样发出（纸面例子 4 块 2 文档 → D=10 而非 6，补充材料例子随
   实现重算）；B 在降级器里抑制分块行 Fact 以贴合论文例子（特殊分支，违背"改主张去适配证据"）。
   **已定（2026-10-17 作者选 A）**：按通用代数原样发出；实测口径下该算例为 R=16、D=26（§5），补充材料 Worked example 已按此重算，
   预算以单元格计（48/160/10）。
3. **f_val 集合级去重**是否进入 KB-1（需第四张关系与第四个 Product）；不进入则论文"collection-scoped"一句改为未实现。
   **已定（2026-10-17 作者选进 KB-1，"做实验"）**：抽取规则 scope 取 document | collection，由定义 Catalog 的人按字段的业务含义选择；
   集合级值以第四张关系 `collection_values` 发布（按 field、canonical_value、kb_department、classification 去重，身份带两个强制范围属性、
   不带文档号，使范围过滤仍可施加），aggregate 遇到集合级字段改在该关系上执行；度量见 E-KB5。
4. **嵌入模型**（2026-10-16 作者定）：用真实本地模型 BAAI/bge-m3（中英多语、1024 维），权重文件 sha256 与模型版本进入
   发布期清单；hash-projection-v1 保留为 CI/单元测试用的确定性提供者。向量不进 Fact 身份，故跨硬件数值差异不影响账本与
   重放。文本查询的嵌入在受信边界内由嵌入服务计算（新组件，部署位置一节补写）。同时作者定：语料改为真实感文档（K2.7）、
   增加检索质量指标（K4.6：合法任务 recall@k、直连 vs 网关命中集一致率）。
5. **生成回答 f_gen** 明确不在 KB-1 范围（论文已写"第四层为未来工作"），确认。
6. **新拒绝码六个**（含 `RETRIEVAL_SCOPE_EXCEEDED`）与 `NOT_ELIGIBLE` 一样进入 apierr 与文档；确认命名。
   **已定（2026-10-17 作者："1 认可；2 我选 a"）**：六个码按原命名进入 apierr 与文档；`CHUNK_NOT_RETURNED` 按 (a) 实现——
   `fetch` 只能取本根任务族此前 `search`/`fetch` 已返回过的块：网关在准备之前把每个块引用解析为分块发布物 HOT 索引里的
   实体，取其 Result 单元格 Fact 的序数，到 Control 库读该根族 V5 根头的 Release 集合判成员（`control.Store.RootReleaseContains`，
   读事务、不预留、不结算）；任一块不在集合内（含范围外、发布物外、从未返回）即关闭式拒绝。放弃的 (b) 是"范围内任意块皆可取"
   ——它让 fetch 成为不经检索的枚举通道。后果：E-KB1 的 S3（命中后取同文档其余块）在网关臂下按设计被拒，见实验设计修订。
7. **检索执行如何接入既有封印准备边界**（2026-10-16 增）：现有 query_sql/execute_plan 全部经 `physicalquery.Prepare` 产生
   带编译器身份与输入绑定的 `PreparedOperation`，执行、预留、派生、结算、回执、重放都以它为锚（"preparePlan 是唯一边界"，
   finalizer 以同一函数复算）。方案 A（推荐）：在 physicalquery 增加同纪律的 `PrepareRetrieval`（检索规范形 + 命中集
   SQL + 序数程序 + 谓词足迹 → 同一 PreparedOperation 密封），K3.3 的"现有结算/重放/回执/委托原样通过"由此自动成立，
   论文只需说"检索是第二种准备操作"；方案 B：检索走独立结算小管线直接调 control 的预留/结算，代码少但回执与重放要另造，
   论文要描述两条管线。方案 A 代价是先读透 physicalquery 的封印（预计一个唤醒）。K3.1（规范形）两方案通用，先做。
   **已定（2026-10-16 作者采纳统一框架，台账 KB1-LOOP-40）：方案 A。** 检索是操作层的第二种准备操作，与 query_sql 同一封印边界。

8. **分块 Result 单元格用 `text` 还是 `text_sha256`**（2026-10-16 增，有实测依据）：现实现把 `text` 作为发布字段与 Result
   单元格，1 万文档 / 48,183 块的 chunks bundle 为 638 MB（COLD 621 MB），发布编译峰值 RSS 2.1 GB；按线性外推 10^6 块
   需约 13 GB bundle、约 44 GB 编译内存，超出本机 30 GB，E-KB3 的 10^6 档不可行。若把发布关系里的 `text` 换成 `text_sha256`
   （Result 单元格 = 内容摘要单元格，网关交付正文时直接读 kb.chunks），论文 f_chunk = ⟨…, C(text_c)⟩ 中的 C 取"内容摘要"
   即可，身份仍确定且（抗碰撞意义下）单射，bundle 缩到每块约数百字节。推荐 B：发布 `text_sha256`，`text` 不进发布物；
   代价是 K2.3/K3.2 的字段表与 oracle 各改一行并重发布 epoch。

## 9b. 统一框架（2026-10-16 作者定，台账 KB1-LOOP-40）

论文主张改为"面向 Agent 的企业数据产品暴露治理"：报表路径与文档/知识路径是同一机制的两个实例，文档场景为主叙事。统一点是四层同构，而不是"知识 Fact 只是命名空间不同的同一种 Fact"：

| 层 | 报表实例 | 文档实例 | 共同机制 |
|---|---|---|---|
| 数据产品层 | 关系表（reporting.*） | 文档集合（documents / chunks / values 三关系） | 同一封印发布物、同一激活与失败关闭 |
| 事实层 | 单元格、行 | 分块、文档、抽取值 | 同一身份编码（ComposeCanonicalKeyV2）、同一账本 |
| 操作层 | query_sql / execute_plan | search / fetch / aggregate | 都是带规范形的准备操作（physicalquery.Prepare / PrepareRetrieval） |
| 效果层 | R / D / O | R / D / O | 同一三维结算、重放、回执、委托 |

文档实例独有的三处：自由文本查询承诺（h_N 取规范化文本摘要）、近似检索下 Outcome 的不确定性（K3.4 量化，默认精确检索）、分块 Result 单元格取内容摘要（§9.8）。正文改写在阶段 5 进行；统一框架草稿见 docs/kb-paper-framing.md。

## 9a. 已知架构债（2026-10-16 记，作者讨论后）

- **复用 V4 发布物是为论文进度做的选择，不是文档治理产品的终局设计。** 它在 1 万文档 / 5 万块的实验规模上成立；大型企业
  文档库还需要：增量发布（每日新增与改版不应整库重发布期）、千万级分块（现 COLD 含正文的体积与编译内存不可行，§9.8 只是
  第一步）、第 4 层生成内容的记账、按密级加权的新颖度、多租户账本。这些大概率要换成专用发布物格式与专用降级器。
- **"知识 Fact 是同一种 Fact"只在账本/身份层成立。** 论文表述应为"同一账本、不同的治理单位与效果"：账本安全、重放、委托对
  文档路径原样成立；单位定义、发布编译、检索降级、威胁边界（改写、同义、跨块推断、嵌入反演）是文档路径新增且未被报表
  路径证据覆盖的部分。局限一节须明写这四条。
- 作者 2026-10-16 表达的方向：企业文档与知识治理比报表数字场景更有价值，论文与产品可能以此为重点。论文重心是否迁移
  （维持"报表为证据核心、文档为第二实现实例"还是改为文档优先）是待作者决策项，见台账 KB1-LOOP-28。

## 10a. K3.3 实施设计（2026-10-16，依 §9.7 方案 A；读代码后定）

**读到的边界（读代码）**：`physicalquery.PreparedOperation` 只能经 `newPreparedOperation(identities, compiler, draft)` 产生，绑定成员集固定（新增成员会使当前契约版本的全部激活失效）；已有第二种准备 `PrepareSemanticView` 的先例：自带输入类型与 `identities()`，复用同一密封。网关 `executeSQL` 的链条是：守卫（任务/Grant/目录/委托/预算）→ 预留（`ReserveRequest.Exposure` 的三维估计）→ 以预留观测到的前置状态重推导 → Connector `QueryPairStream`（可见语句 + 溯源语句，溯源行流进 `ordinalDerivationSink`，按 `OrdinalProgram` 与 sidecar `SnapshotIndex` 得到 Release/Influence 位图与 V5 动态 Fact）→ `control.OrdinalExposureObservation` → `FinalizeOrdinalQuery*WithReceipt`（结算、回执、执行绑定同一事务）。结算层消费的是位图 + 动态 Fact，不关心语句从哪来。

**方案（选定）**：检索 = "编译器产出的等价计划语句，外包一层命中集约束"。
1. `physicalquery.PrepareRetrieval(RetrievalPreparationInputsV1)`：输入 = Grant、Catalog、知识产品、检索规范形（K3.1 `retrieval.NormalForm`，含注入范围原子）、查询向量（float32 数组，与规范形里的 int16 承诺一并绑定）、chunks/documents 发布物绑定、k。内部：
   a. 由规范形构造等价 `queryplan.QueryPlan` P：`<kp>_chunks ⋈ <kp>_documents`（doc_id, version），Columns = doc_id, version, chunk_index, text（或 §9.8 的 text_sha256），Filters = 原子（注入范围原子按现有 V5 口径不进入调用方足迹）；经现有 `Prepare` 得到 S_v / S_c、`OrdinalProgram`、谓词足迹、字段表、sidecar grant——全部由现有编译器产出，不手写。
   b. 外包：`WITH hits AS (SELECT doc_id, version, chunk_index, row_number() OVER (ORDER BY embedding <=> $vec, doc_id, version, chunk_index) AS rank FROM kb.chunks WHERE collection = $c AND <原子与范围按 kb.chunks 列渲染> LIMIT k)`，可见语句 = `SELECT s.* FROM (S_v) s JOIN hits USING (doc_id, version, chunk_index) ORDER BY hits.rank`，溯源语句同样对 S_c 做半连接。排序键带 (doc_id, version, chunk_index) 作全序，使两条语句在同一可重复读事务里命中同一集合（K3.4 的边界并列问题由此消除）。向量字面量进入语句文本，故被 VisibleTarget/CompanionTarget 摘要覆盖；finalizer 以同一输入重算即得同一字节。
   c. 密封：`NormalFormSHA256` = 检索规范形摘要（域 `TASKGATE-RETRIEVAL-NF-V1`），`PlanSHA256` = P 的摘要，`PreparationInputsSHA256` = 检索输入摘要，其余成员与 SQL 路径同源（编译器身份、Catalog、快照绑定集、序数程序、字典集、谓词足迹、EstimatedBaseFacts = k）。配套 `RequireRetrievalInputs`。
2. 网关 `executeRetrieval`：把 `executeSQL` 的守卫—预留—执行—结算链抽成可被两种准备共用的函数（只搬不改语义；现有测试原样通过是验收），检索调用它：预留估计 Release = k × 可见字段数、Influence = k 块行 + ≤k 文档行、Outcome = 调用方原子数 + 1；执行走同一 `QueryPairStream`；位图由现有 sink 从溯源行得出（命中块不在发布物中 → sink 查不到实体 → 关闭式失败，对应 `CHUNK_NOT_RETURNED` 的发布期一致性含义）；复合 c_q 的 `ResultObservationSHA256` 由现有 V5 路径按可见行计算。
3. MCP 工具：`search`（文本或向量查询 + 过滤 + k）、`fetch`（按 (doc_id, version, chunk_index) 列表；规范形无原子）、`aggregate`（§5：对 `<kp>_values` 的现有 `query_sql` 包装，不新写结算）。`search` 的文本查询先经嵌入服务得向量；向量进入准备输入。
4. 独立 oracle（`evaluation/kboracle`）与 K3.2 的 `retrieval.BuildEffect` 继续作为对照：对同一命中集逐成员核对结算写入的 Release/Influence/Outcome 集合。

**新增待裁决 §9.9（有实现依据）**：可见语句必须投影 doc_id, version, chunk_index 才能与命中集连接并回给调用方，于是这三列的单元格也成为 Release Fact（R = 4|y| 而非 |y|）。推荐接受：这是通用代数原样发出（与 §9.2 同一理由），响应里本来就交付了这三个值；K3.2 的 effect.go 与 oracle 相应改为四列。 **已按推荐实施（2026-10-16，KB1-LOOP-47）；作者若否决需回到 §10a 的替代方案。**放弃的替代：把身份列藏进溯源侧的句柄列（要改编译器的可见/溯源投影规则，触碰封印）。

**阶段**：K3.3a `PrepareRetrieval` + 单元测试（finalizer 重算一致、篡改语句被 Validate 拒绝、命中集全序）；K3.3b 网关链条抽取与 `executeRetrieval` + 集成测试（重放 0/0/0、拆分不多扣、子任务共账、超限扣留）；K3.3c 三个 MCP 工具 + `scripts/integration-test.sh` 用例 + 文档。

## 10. 实施顺序（不等审阅即可做的部分）

K2.2 表与角色（按 §1/§7，默认不含集合级关系）→ K2.5 语料生成器（独立于裁决）→ K2.3 kb-publish → K2.4 Catalog 块与激活
→ K3。若 §9 决策 1/2/3 改变，受影响的只有 kb-publish 的输出形态与 K3.2 的 oracle 计数，表结构不变。
