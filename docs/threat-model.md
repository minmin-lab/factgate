# 威胁模型与生产化差距

## 范围与资产

要保护的资产包括：

- `legacy.*` 业务数据及 Reporting View 返回的明细和汇总结果。
- Alice/Carol MCP Token、OA 服务 Token、回调/会话密钥和两个 PostgreSQL 的密码。
- TaskGrant、审批凭证、task-scoped View binding/dependency/status、资源预算、根任务三维 exposure head、immutable FactID/ordinal dictionary、bitmap containers、查询凭证与审计 Hash Chain。
- AES-256-GCM 数据密钥、TaskGate 私有对象存储中的加密 Parquet 规范原件，以及 Control PostgreSQL 中的 artifact 元数据。

Agent 提交的结构化申请、QueryPlan、SQL、浏览器请求、网络回调和业务数据都视为不可信。Catalog、Gateway 二进制、Docker 宿主机以及可读取 `.env`/Volume 的管理员属于可信运维域；该域失陷后，本 Demo 无法保证机密性或不可否认性。

## 主要威胁与现有控制

| 威胁 | 当前控制 | 残余风险 |
|---|---|---|
| Agent 越权读取其他主体任务/结果 | Bearer Token 映射固定 Principal；任务和结果同时校验 principal、task、query 与 actor；越权按不存在返回；Principal 禁用后 Gateway 不再列出或执行工具 | 长期静态 Token 没有设备绑定、短期会话、组织级权限或外部会话吊销发布 |
| Carol 读取敏感原始结果 | Carol 只注册两个审计工具，凭证不含结果行；Control PG 不保存 Parquet bytes | 同时拥有对象存储、数据密钥和元数据权限的管理员可以解密；审计元数据可能包含目标和 OA 信息 |
| 客户端伪造授权字段 | 身份字段由 Gateway 生成；产品、字段和 Scope 必须显式且非空；OA 展示并绑定 RFC 8785 `AuthorizationManifestV1` 摘要 | 不验证自然语言目标与查询语义；审批人仍须核对结构化 Manifest |
| 无资格主体把越权申请送进审批队列并依赖审批人误批 | Catalog `eligibility` 在建草稿前把申请人的证明属性（含带窗口的临时授权）编译成可申请包络，包络外直接 `NOT_ELIGIBLE`，不建任务、不设例外路由；委托按委托人属性评估；裁决连同属性快照摘要落入任务记录 | 属性快照是身份/HR 系统的输入，其正确性与时效属可信运维；包络只覆盖 `require` 与 enum Scope，不覆盖字段级或日期范围；误批仍受预算、TTL 与回执约束而非被阻止 |
| 恶意 QueryPlan | 本地编译器验证产品、字段、聚合、过滤、排序、literal、Limit 与 Offset，随后进入完整 SQL AST 策略 | 编译器或策略实现缺陷仍需模糊测试和攻击语料覆盖 |
| 通过拆分查询、重叠分页、新 request ID 或不同阈值重复提取数据 | release/influence/outcome 按 decoded FactID 集合结算；exact bitmap ANDNOT 只计 novel facts；OutcomeFact 绑定规范化命题与结果；终态 request replay 不重执行 | outcome 是每个成功命题一个单位，不估算信息量；不限制输出顺序、背景知识或跨 root 推断 |
| 通过预算拒绝探测数据相关阈值 | 超预算结果始终在释放前丢弃，成功结算保持账本上限 | 接受/拒绝位本身不计 exposure，可能泄漏候选 Effect 是否超过剩余额度；当前机制不是 simulatable auditor，不能把拒绝称为无泄漏 |
| 聚合把百万来源伪装成一个返回行 | release 与 dependency 分账；聚合输出只占少量 release，但所有正向贡献来源进入 exact bitmap influence | legacy 全量 FactSet 已证明不可交互；V4 bitmap 的性能门槛尚待固定环境验证，安全路径不使用 sketch/近似 |
| 子 Agent 通过委托重置预算 | 签名 root/parent lineage、逐维 Grant narrowing、每次执行验证祖先 ACTIVE；整个任务族共享 root ledger | 默认 Demo 只有一个 query Principal；更大委托 DAG 只有有限模型与实现测试，尚无大规模实验 |
| 同一主体申请多个根任务重置账本 | 每个 root 都需要一次独立的人类审批并在其后代内守恒 | 系统没有 principal/tenant 级全局 exposure ledger；若审批策略允许多个 root，同一主体会获得多个独立预算，root-family 定理不阻止这种放大 |
| 并发子任务同时花费最后额度 | 全家族共享一个三维 root head；epoch CAS 一次发布 R/I/O，冲突后重算；上限、结果和 receipt 同事务 | 单一 root head 仍是高并发热点；当前没有分布式 settlement 协议 |
| 可见结果与 provenance 观察不同数据版本 | 两条策略 SQL 在一个只读 `REPEATABLE READ` 事务执行；Product 绑定 manifest-proven snapshot publication；证据缺失或截断时结果不释放 | 冻结/发布流程仍属于可信运维；错误源快照或构建前可变数据会破坏前提；系统不支持 mutable OLTP/CDC serving |
| 活动任务在每日切换后误读新 publication | 签名 Grant 传递式绑定包含 Product→publication 与 artifact digests 的 Catalog SHA；查询再次校验 task/Grant/Service Catalog，root head 固定 dictionary set，错版本在连接器调用前关闭式拒绝 | 连续执行旧任务需要保留旧 Catalog/artifact/Gateway epoch 并按 task binding 路由；单个 Service 不支持安全 hot swap，路由器和 artifact 保留属于可信运维 |
| Nested View 的 root 或任一 child 在审批后被替换 | Catalog 分开固定 exact definition、transitive dependency、typed canonical plan 和 ordered interface 四摘要；只发现本任务 roots；规范 binding digest 进入签名 Manifest/Grant、immutable Control rows 和 query record；反向 dependency rows 保留影响证据 | 漂移在申请、OA 激活或新查询时惰性发现，不是数据库 DDL event listener；Catalog/DBA 与 terminal publication 发布仍是可信运维；受支持范围仅是 bounded restricted fragment |
| View 检查后、业务 SQL 前发生 `CREATE OR REPLACE VIEW` | reservation 前重算 binding；Connector 在 visible/provenance SQL 的同一个只读 `REPEATABLE READ` 事务中再次发现 closure 并核预期 revision digest，检查和执行观察同一 PostgreSQL snapshot | PostgreSQL/connector 缺陷或 DBA 绕过事务快照仍超出应用保证；semantic replay hit 不执行业务 SQL，只依赖其执行前的 task binding 校验和 immutable committed evidence |
| Semantic root 藉展开绕过 public Grant 或 Scope | 展开后 public fields 只能经已绑定 `Artifact.Output.FieldID` 进入 `CompileRelational`；签名 Grant 仍只包含 root；Gateway 每查询派生最小 terminal internal grants，并要求 root scope 映射覆盖每个 terminal mandatory scope；V4 只绑定 terminal publication ordinal/sidecar | Catalog 对 root output、terminal scope 或 publication 的错误声明仍属可信发布风险；当前不支持 query-time semantic-root-plus-product Join 或 root 上 order/page，已聚合 root 之上只纯投影 |
| 无关 View 变化造成全局拒绝服务 | Semantic roots 排除在 legacy source-level `schema_digest` 外，task binding 只含获批 products 的 reachable closure；无关 root replacement 不改变该 task binding 或 readiness | 没有 `view_contract` 的 legacy/terminal products 仍共享 source-level attestation；该兼容路径上的漂移会按设计关闭该 source 的 readiness |
| 通过直接 SQL 绕开 exposure compiler | `query_sql` 在**任何** Grant 下（含 resource-only）都必须按 `taskgate-reporting-sql-v1` 无损 lowering 为 canonical QueryPlan；Gateway 丢弃原始 SQL 作为执行来源，只执行重新生成并再次过策略的 visible SQL 与 provenance companion；不能 lowering 时在执行和结算前结构化拒绝 | 在线 SQL profile 仅支持声明的单产品片段和 2–16 源内任意形状的 connected INNER equi-join graph；每条 edge 只含一个或多个 column-to-column equality，self/outer/cross/non-equality 和断开 graph 仍拒绝。16-source 上限是 operational complexity/DoS ceiling，1 MiB 请求体、AST/资源通用边界仍适用；AVG 只支持精确数值，不支持窗口、Union SQL 或任意 SQL provenance，lowering 入口仍需持续 fuzz |
| SQL 注入、注释绕过或写操作 | PostgreSQL AST 单语句白名单、逻辑产品/字段/函数/运算符限制、Scope CTE、外层行限制 | Parser/策略缺陷和依赖漏洞仍可能存在 |
| 策略失误后修改业务库 | 非 owner、非 superuser、无 `BYPASSRLS` 的独立 `gateway_reader`；角色/连接/事务只读；仅 Datasource Attestation、Reporting Views 和 ordinal sidecar `SELECT`；legacy Schema Attestation 与 semantic View binding | DBA 误授权、Catalog/contract 本身批准了错误语义或数据库漏洞仍可绕过应用意图；摘要只能证明版本一致，不能证明业务语义正确 |
| OA 回调伪造、重放或乱序 | HMAC-SHA256 认证原始 Body；Event ID/状态/context/actor 校验；独立 OA Ed25519 回执绑定 Manifest 与最终 Grant；Gateway 可配置多把 OA 验签公钥及有效/退役窗口；事务幂等 | Demo OA Outbox 不持久；KMS、密钥分发和吊销发布仍需外部运维 |
| 并发/重试查询超资源预算 | 控制 PG 对任务和资源预算加行锁；`(task_id, request_id)` 唯一；预留与结算在事务中；同一任务一个在途查询 | 单 Gateway 没有跨实例执行租约，不能安全横向扩容 |
| Gateway 崩溃遗留预算/回调/artifact | 启动恢复将 RESERVED 查询按资源预留保守计费并标记 `INDETERMINATE`，释放未结算 exposure reservation，写入查询回执，并禁止同 request ID 重执行；结算已提交的 `PENDING` artifact 只在 staging/committed 证据一致时由确定性 key 幂等 promotion，启动和后台 sweep 继续恢复 | 资源计费可能高于实际消耗；staging 丢失或 canonical 证据冲突时 readiness fail closed，必须恢复正确对象证据或执行受审修复；当前没有自动放弃/退款；staging 和控制元数据必须共同纳入恢复、对账和告警 |
| 查询回执伪造或历史密钥混淆 | V6 成功回执绑定 dictionary set、三维 effect/charge、root epoch、result digest、`signed_at`、Gateway Key ID 和终态审计位置；旧 verifier 保留；Gateway 发布带有效/退役窗口的公钥 Bundle | Keyring 发布仍依赖 Gateway 配置和部署重启；没有集中 KMS/HSM、透明日志或外部撤销服务 |
| 并发审计链分叉或序列空洞 | 单行 `audit_chain_head` 通过 `SELECT ... FOR UPDATE` 串行化；连续序号、事件和链头在同一事务提交；可配置 `taskgate-audit-checkpoint-anchor/v1` 签名 checkpoint POST 到外部日志/WORM 服务 | 外部 Anchor 服务本身的保留、不可篡改性和时间戳可信度依赖部署；高写入量时链头是串行瓶颈 |
| Grant/审计记录被应用修改 | PostgreSQL Trigger 禁止 UPDATE/DELETE，逐事件 SHA-256 链可验证 | 超级用户可禁用 Trigger 并重建整条链 |
| 对象结果泄露或篡改 | Gateway 在对象存储客户端侧使用分块 AES-256-GCM，AAD 绑定 task/result/frame ordinal，并校验明文 Parquet 与密文对象双 SHA-256；private bucket、bucket-scoped 凭据和内部网络阻止 Agent 直接列举；每个 artifact 绑定 `key_id`；TTL/purge 删除 canonical bytes 后 tombstone 元数据，active legal hold 阻止 retention 清理 | bucket 必须私有且禁用 versioning；否则删除可能只写 delete marker 而保留旧 bytes。任务、Scope、Grant、对象键、预算与审计元数据未加密；Demo AES key 仍位于环境和进程内存；已开始的并发读取可能在 key ID 擦除后结束；真实 key material 销毁、轮换和 KMS/HSM custody 仍需外部平台 |
| 交付 capability 泄露 | `deliver_result` 产生绑定 result/task/expiry 的 HMAC 短 TTL URL，Gateway 流式解密而不暴露 S3 对象键；非 loopback 基址必须为 HTTPS | URL 在过期前是 bearer capability；反向代理、APM、浏览器/客户端历史和聊天附件层可能记录它，必须对 `token` 查询参数脱敏；离开 TaskGate 边界的副本无法再撤回 |
| 本机网络窃听 | Gateway、OA 和 Control PG 绑定 `127.0.0.1`；Business PG 和结果对象存储默认仅内部网络 | 本地仍使用明文 HTTP/PG/S3；同机恶意进程可尝试连接或截获凭据 |
| 浏览器会话伪造与 CSRF | 签名 Cookie、bcrypt、`HttpOnly`、`SameSite=Lax`、CSRF Token、CSP | 本地 HTTP Cookie 无 `Secure`；无 MFA、锁定、SSO 与登录审计 |
| DoS 与成本滥用 | HTTP Body/Timeout、连接池、数据库 Timeout、任务预算、结果行上限和 preview byte cap | 目录和任务申请缺少租户配额、速率限制和异常检测；Connector/可见结果到 Parquet 的部分路径仍会完整内存物化，而 preview 默认对大于 64 MiB 的 artifact 关闭，尚非生产级有界百万行流式实现 |
| 容器逃逸或供应链攻击 | 非 root、只读根文件系统、`no-new-privileges` | 未固定所有镜像 Digest，缺少定制 seccomp/AppArmor、签名和 SBOM 门禁 |

## 累计查询与多 Agent 攻击

### 威胁：Query accumulation attack

攻击者不必提交一条明显越权的 SQL；它可以连续提交许多单独合法的筛选、
分页或聚合查询，并依据前一个答案自适应选择下一个谓词，尝试重建工资等敏感
数据。TaskGate 的防御不是限制查询条数，而是让同一根任务族的每次 Agent Task
Execution 都针对 `L_release`、`L_dependency` 和 `L_outcome` 计算正集合差。
已计量事实不重复收费，新事实必须原子加入累计 Exposure Ledger；任一维超过签名
预算时，候选结果在交付前被拒绝。

这个控制只覆盖已声明 FactID 与 controlled analytical SQL profile 中的显式暴露和
正向依赖。拒绝位、排序、背景知识、跨根任务串联以及 profile 外的推断渠道仍是
残余风险；因此“被预算阻断”不能表述为“攻击者没有获得任何信息”。

### 威胁：Retry amplification

网络重试或恶意客户端可能反复提交同一个调用。`request_id` 在任务内唯一：相同
`(task_id, request_id)` 与相同请求摘要只观察首次持久化终态，相同 ID 搭配不同请求
则关闭式冲突，不会产生第二次业务执行或第二次预算消费。攻击者换用新的 ID 重放
同一 canonical QueryPlan 时，semantic replay 仍重新授权并计普通资源，但已在根账本
中的 release/dependency/outcome facts 不产生新的 exposure delta；真正不同的命题会
形成新的 outcome，并可能耗尽预算。

### 威胁：Multi-agent budget sharing

根任务把预算委托给 Agent A、Agent B 或更深后代时，委托可以把执行 Principal
改为获准的目标 Agent，但产品、字段、Scope、期限与容量等权限维度只能逐维收缩，
不能借主体变化创建更大的权限。所有后代解析到同一个
root-family Exposure Ledger；并发结算以一次三维 epoch CAS 发布，冲突方重读最新
head 后重新计算 delta，因此不能各自在旧余额上成功花费同一份剩余额度。

该性质不覆盖两个分别获批的根任务，也不等同于主体级或租户级全局预算。当前
单实例原型也没有跨多个 Enforcement Layer 实例的执行租约；在补齐该协议前，同一
publication epoch 不应横向扩容。

## 数据最小化与边界

Gateway 不向外部模型或翻译服务发送目标、Catalog、SQL 或结果。所有任务申请、SQL AST lowering 和 QueryPlan 校验均在本地确定性代码中完成。

普通 `query_sql`/`execute_plan`/`get_query_result` 响应只给获批 Alice 的 MCP 客户端返回摘要和 `result_id`，不返回全量行或对象键。`preview_result` 只释放有界行；`deliver_result` 在明确交付时通过 Gateway 给出短期下载能力。但 exposure 记账边界更早：规范加密 Parquet 对象创建成功时已视为完整结果被消费，不依赖 Agent 是否下载。交付后的 Agent Host 临时副本、用户设备、终端历史、截图或后续复制都在 TaskGate 删除控制之外。

Source-influence FactID 是 Gateway 内部的计量证据，响应和收据只交付计数与摘要，不把这些 FactID 当作 Agent 已知事实返回。因此 exposure ledger 表示“此前已计量的正向事实与成功命题集合”，不是主体知识状态，也不是总体隐私损失。空/零答案会进入 outcome 集合，但排序、背景知识、timing 以及预算接受/拒绝产生的推断不进入；一个 outcome 单位也不等于一个信息 bit。

V4 HOT dictionary 只保存 row-handle/ordinal 映射和 32-byte FactHash；COLD
dictionary 才保存可恢复 canonical payload。二者及 bitmap 仍是敏感访问元数据，
必须受 Control/artifact 权限和保留策略保护。当前原型尚未把 dictionary/bitmap
生命周期完整接入用户删除请求或跨 snapshot 受审压缩。

默认部署不向宿主机发布 Business PostgreSQL 或 MinIO；只有 Gateway 所在隔离内部网络可达。`compose.debug.yaml` 是明确标记的非论文调试覆盖，只将业务库和 MinIO 绑定到宿主机回环地址。控制库、业务库与结果对象存储使用独立的数据、凭据和 Volume，业务只读账号无法访问控制库或结果 bucket。

### 知识产品的两个边界（KB-1）

- 名字解析：数据连接器为每条受治理语句钉 `search_path = pg_catalog`，策略语言不接受带模式限定的算子或函数，因此 pgvector 必须装在
  `pg_catalog`（`db/init/kb-schema.psql`）。这保持了"未限定名只能解析到系统目录"的性质；代价是业务库管理员要把扩展放进系统模式。
- 嵌入服务：文本查询的向量由受信边界内的嵌入服务计算（`KB_EMBEDDING_ENDPOINT`）。它看到查询文本与块文本，不接触账本与授权；
  查询承诺 h_N 取规范化文本的摘要而非向量，向量只进入准备输入并被语句摘要覆盖。服务不可用时文本查询关闭式拒绝，向量查询不受影响。
  直连嵌入服务或向量库的客户端绕过账本，与直连数据库相同。
- 正文交付（kb-design §9.8）：账本里一个返回块的 Result 单元格是内容摘要 `text_sha256`，正文不在发布物里。`search`/`fetch`
  的结果由网关补上正文：封印语句执行之后、结算之前，按（集合，封印语句返回的身份三元组）读 `kb.chunks`，逐行核对 sha256(正文)
  等于该语句返回的摘要，作为第五列写进规范结果，结果制品与重放都携带它。不符或缺行即 `CONFLICT`，查询在结算前失败、不交付任何
  正文，也不释放任何 Fact。这条读取是网关内部读，不经调用方 SQL 策略；它只按封印结果里的身份取行，调用方无法影响其谓词。业务表
  正文相对发布期漂移因此表现为交付失败，不会把未发布的字节送出。

- 主体文件（`PRINCIPALS_PATH`）：部署可以增加 query 主体，文件只含 token 的 SHA-256。它与 `TASKBOUND_ALICE_TOKEN` 同属部署
  配置，能改它的人就能增减可登录的 Agent 身份；文件本身不含秘密，可以作为部署证据留存，明文 token 只应在发放方手里。

## 需要运营配合的边界

- Catalog 管理员必须保证字段的业务语义正确、`entity_key` 在 snapshot 内稳定唯一、数据更新时提升 snapshot，且 View 不包含未发布敏感列；legacy Attestation 和 semantic View 四摘要只能证明被检查的结构/定义/计划/interface 与已发布 contract 一致，不能证明这些语义本身正确。
- Semantic View 发布必须让普通 View closure 落入 restricted fragment，并把已治理 materialized View 作为 terminal；root 或 child 变更后应发布新 Catalog 并创建新 task/OA approval。不要尝试把旧 task 的 `REQUIRE_REBIND` 改回 `ACTIVE`。
- `.env` 应保持 Git 忽略并收紧文件权限；数据密钥、Control 元数据和对象存储备份必须按同一 artifact 恢复点安全管理。
- 每日 Catalog/publication 切换若要保留 ACTIVE 任务，必须保留旧 Catalog、只读 snapshot、artifact 和一个旧 epoch Gateway，并按 task 的 Catalog binding 严格路由；若部署做不到，应排空或重新申请任务，因为错版本会关闭式拒绝而不会自动切换。
- 应监控回调重试、`VIEW_SEMANTIC_CHANGED`/`REQUIRE_REBIND`、启动/后台 `PENDING` 恢复、孤儿 staging 对账、数据库和对象存储 Timeout、预算耗尽、结算重试、readiness 和审计链验证。当前反向 dependency index 是持久证据与定位辅助，不替代外部 DDL 监控。
- 两个 PG Volume 与结果对象存储必须分别备份并联合恢复演练，避免误把系统控制数据、业务数据和 artifact bytes 混作同一生命周期。

## 生产化差距

### 身份、网络与秘密

1. 用 OIDC/OAuth 2.0、短期 Token、Audience/Scope、租户绑定和集中撤销替代静态 Token。
2. Gateway、OA 与数据库全部启用 TLS/mTLS，并置于受控网络与反向代理后。
3. 使用 KMS/HSM 或 Secret Manager 做 Envelope Encryption，保存 Key ID，支持轮换，并把 key ID 擦除操作绑定到真实 key material 销毁与透明撤销日志。
4. 使用工作负载身份或编排平台 Secret，避免秘密长期存在于 `.env` 和进程环境。
5. 对象存储 bucket 保持私有、禁用 versioning，只给 Gateway bucket-scoped 最小权限；非 loopback 交付使用 HTTPS，并在负载均衡、日志与 APM 层脱敏 capability query token。

### 持久化与高可用

1. 持久化 OA 草稿、用户、Outbox 和审批历史，提供双向对账与死信队列。
2. 在横向扩容 Gateway 前实现跨实例、可续租且可恢复的每任务执行租约；只有数据库行锁不足以覆盖业务查询执行窗口。
3. 将结果保留清理的 TTL、管理员操作和 legal hold 接入组织级工单、审批和告警流程。
4. 对控制库、业务库和对象存储分别建立最小权限、备份、时间点恢复和跨系统一致性演练。

### 审计与策略

1. 将签名 Audit Head Anchor 接入企业级 WORM/透明日志、可信时间戳和告警流程，并监控 Anchor 失败。
2. 启动及周期性验证 Hash Chain，失败时停止敏感查询并告警。
3. 将 Catalog、Reporting View、角色 Grant 和数据库迁移作为同一个受审发布单元，并做 Schema/类型校验。
4. 对 SQL-to-QueryPlan lowering、QueryPlan 编译器、Nested View discovery/compiler、SQL Parser、Scope 注入和渲染器持续做 Fuzz、属性测试及 PostgreSQL 版本兼容测试；已有五类固定 seed、各 64-case `testing/quick` properties 覆盖 determinism、alias invariance、join-order invariance、nested/direct equivalence 和 semantic/interface/dependency drift，并另覆盖 cycle/limit/aggregate barrier 和并发 View replacement。属性测试是 bounded grammar 的实现证据，不应表述为任意 SQL 语义保持的形式化证明。
5. 增加管理员撤销/暂停、紧急 Kill Query、主体/产品级全局预算和速率限制。
6. 将已实现的 snapshot compiler/artifact 校验接入企业计划 ETL/同步、版本化 Reporting Snapshot 流水线和双人发布门禁，为仍有活动任务的旧 dictionary/bitmap/artifact 建立保留、压缩和受审删除策略；实时 CDC serving 不在当前范围内。
7. 持续对 2–16 源 JoinGraph 的 chain/star/cycle、10 表链、多 predicate edge、alias 改名、node/edge/predicate 置换和 deterministic binary fold 做属性测试与 Fuzz；self/outer/cross/non-equality Join、断开 graph、SQL Union、窗口和多引擎 provenance 未进入已声明 profile，继续关闭式拒绝。
8. 如需防止空结果、顺序、相关查询或外部知识产生的推断，应叠加差分隐私、查询审计或领域专用推断控制；当前 exposure ledger 不提供这些保证。

### 平台加固与可观测性

1. 增加结构化指标、分布式 Trace、告警、SLO、容量测试和故障演练。
2. 固定镜像 Digest，生成 SBOM、签名并接入漏洞扫描。
3. 显式 drop Linux Capabilities，配置 seccomp/AppArmor、资源配额和网络策略。
4. 为 MCP 与 OA 增加速率/并发限制、异常检测、Body/响应配额和防暴力登录。

## 上线门槛

接入真实企业数据前，至少应完成企业身份与 TLS、集中密钥管理、持久 OA/Outbox、多 Gateway 租约、外部审计锚、Catalog/View 联合发布、攻击测试、双库备份恢复演练、数据保留策略和独立安全评审。
