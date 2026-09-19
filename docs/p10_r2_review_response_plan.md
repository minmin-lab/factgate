# P10-R2：GPT 第二轮模拟评审整改清单（2026-09-19 作者定：全做）

评审核对基线 `3327996`。上一轮清单 `docs/p10_review_response_plan.md`。状态列由执行者更新（TODO / DOING / DONE / 待作者）。
每项完成标准：改 → 容器构建 → 主稿 12 页 0 溢出、无未定义引用 → 提交 → `make paper-final-check` rc=0 → push → NAS ff → 台账一行 → 本表状态 DONE。

## C 组：结论与证据的正确性（先做，全部离线或低成本）

| # | 项 | 出处 | 做法 | 状态 |
|---|---|---|---|---|
| C1 | 预算效用扫描改为精确集合重放，撤回上下界 | `evaluation/cmd/budget-utility-sweep/main.go:75-110`；`results.json` 0.25× 处 6/23 > 5/23；`supplement.tex:2238-2242` | 每条语句保存 R/D/O 三维完整候选事实集合（D 已有 `f.Dependency` hash 列表；R/O 需从 finalv5benign 的 footprint 或执行记录取出全集），每个倍率下 Δ = F ∖ K，通过才并入；两条曲线合并为一条"精确重放（离线，非执行）"曲线；若 R/O 拿不到全集，只对 D 做集合、R/O 明说是 per-statement 求和并写清；`completion_pct` 改名 `admitted_pct`；重生成 results.json、图、`\Sweep*` 宏与 supplement 段；main.tex 若引用上下界一并改；结果登记 pilot-evidence | DOING（代码/文本/结果已改，容器构建被环境阻塞，见台账 P10-R2-LOOP-1） |
| C2 | 时序通道降为条件模型估算 | `main.tex:374-376` "caps it at … bits"；`supplement.tex:2074-2082` | 主文改为"在线性高斯模型、均匀先验与经验参数下估算约 X bit/次；本实验未证明最坏情况上界"；supplement 增一句限制（经验斜率非最大斜率、观测方差非攻击者不可降噪声、均匀先验非信道容量）；762 次拒绝时延保留为经验观察 | DOING（main.tex:374 与 supplement 时序段已改；待构建） |
| C3 | 代理实验 Outcome 12 vs 11 口径对齐；probe `correct` 定义 | `evaluation/final-v5-wsl2/p10-pilot-evidence-v1.json:22`；`evaluation/agent-pilot/results.json` ledger_outcome_median=11 | 去执行机读 p10-b2-agent-01 各 factgate/probe 运行的 agent.jsonl，逐步列 ΔO、拒绝前已用、被拒请求需求量；按实证改写为"已用 11、下一探测需 ≥2 不可容纳"或其它实情；证据文件改动记台账（登记件改动要给根因，不得悄改）；results.json 与 supplement 写明 `correct` = 区间包含真值 / 精确恢复 | DOING（raw 在 git 内已逐步核实 11/12；文本与登记件已改；待构建） |
| C4a | 吞吐：ledger 一致性与客户端完成率分述 | `evaluation/throughput-pilot/results.json` 1×50 disjoint 261/300、nested 259/300、CONFLICT 80/600、cas_attempts 1664 | supplement 与主稿把 error_rounds=0 / ledger_matches 与 settled/requests、CONFLICT 数、CAS 重试次数分列成表；主稿一句不得写成"全部成功" | DOING（表加 requested/CONFLICT 列、文本分述；待构建） |
| C4b | 吞吐补：含客户端重试的有效完成吞吐、最终完成比例、端到端尾延迟 | 评审 §6.2 | 设计写台账（客户端对 CONFLICT 重提至多 k 次，k 用保守先验定，不拟合；三部署；同 benign-x4 剖面；K∈{1,4}×N=50×3 overlap）；campaign_class pilot；先查现有 throughput-pilot 数据能否推出下界再决定重跑范围；结果进 supplement 表 | READY（设计 docs/p10_r2_c4b_throughput_retry_design.md；adapter -throughput-client-retries、hook 透传、launch-throughput-retry-pilot.sh、throughput-retry/analyze.py 全部就绪；pilot 类接受占位绑定；待本机 CPU 空闲后发射） |

## D 组：已有结果放到正确位置（低成本文本项）

| # | 项 | 做法 | 状态 |
|---|---|---|---|
| D1 | 独立集 55%（22/40）比开发集 70% 更显眼 | 摘要/引言/评估节顺序：先独立集再开发集；开发集标"功能迭代所得"，独立集标"冻结后适用边界" | DOING（摘要与 §评估已改；待构建） |
| D2 | 真实代理实验明确为两目标受控机制研究 | 主稿与 supplement 写明 36 runs = 2 objectives × 2 arms × 3 deployments × 3 repeats、十行 fixture，不是任务完成率评估 | DOING（主稿一句已改；supplement 原已写明；待构建） |
| D3 | 吞吐同时报成功/冲突/重试（与 C4a 合并完成） | — | TODO |
| D4 | 复现包入口 / artifact URL（原 A3） | 评审已从 github.com/minmin-lab/factgate 读到全部文件，说明公开仓库存在；核实该 URL 与 3327996 可达（gh api），主稿 artifact 段与 README 填入 URL + commit；若作者另有指定位置则待作者 | DOING（仓库 PUBLIC 已核；主稿填 GitHub URL，591MB 原始样本的 DOI 指向 README 单点，**DOI 需作者存缴后填**） |

## E 组：B5 公共路径消融与朴素精确集合基线（最贵，最可能改变录用判断）

| # | 项 | 做法 | 状态 |
|---|---|---|---|
| E0 | 设计写台账后再动手 | 四臂：(i) 同治理与发布路径、不记账；(ii) 加事实推导、不做历史预算限制；(iii) 同语义朴素精确集合账本（同事实身份、同预算语义、同持久化、同失败语义——不得用仅内存 set）；(iv) 当前完整实现。同工作负载（benign 23 语句 + footprint ladder 选段）、三部署、每臂同轮数；停止规则用保守先验；指标：每语句端到端延迟分解、settle 子阶段、账本存储；预期表先写 | DESIGNED（docs/p10_r2_b5_ablation_design.md） |
| E1 | 不改 V5_MEASURED_PATHS 的实现路线 | `evaluation/cmd/b5-ablation/` 独立二进制复用 internal 包，开关在该二进制内组装；朴素账本在 `evaluation/internal/naiveledger/` 实现（PG 集合表或持久化 Go map + WAL），走同一 settle 接口；若实证必须改 internal/ → 记台账「待作者裁决：解冻 measured paths 并重封」并转做其他项 | DOING（naiveledger + 良性轨迹重放已实测；-b5-ablation 驱动、run-b5-ablation.sh、三份+主 catalog 无记账孪生、analyze.py 就绪；未改 internal/） |
| E2 | 执行 pilot、登记、入 supplement 新小节 + 主稿一表 | campaign_class pilot、publication_eligible false；回答四问：公共路径成本 / 事实推导成本 / 朴素正确实现成本 / 优化增量收益；主稿 21.8–144.5× 的解读随之校准（改主张适配证据） | DOING（6 次部署结果在 evaluation/b5-ablation/results.json；supplement §ablation-supp 与主稿一段已写、宏已入生成器；待原始目录恢复后构建验证） |

## 执行顺序

C1 → C2 → C3 → C4a → D1 → D2 → D4 → E0 → E1 → E2 → C4b（C4b 与 E2 都是三部署重跑，互斥于时延测量，串行；E 组设计 E0 可在 C 组容器构建等待期写）。
