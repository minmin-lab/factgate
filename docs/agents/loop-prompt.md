# 无人值守循环（按需读取）

回合一结束，md 里写什么都不会唤醒 Claude——这是机制事实。无人值守工作必须用 `/loop` 启动。
P9 阶段标准任务词（2026-09-02 更新；作者 2026-08-16 立机制）：

```text
/loop 30m 你是 TaskGate TKDE 投稿的项目经理，全部执行与审计由你自己完成，默认不停。本次唤醒依次做：(1) 对表与体检：TZ=Asia/Shanghai date；NAS 与 WSL 的 git HEAD/树净/分支一致；后台任务与看守是否还在（判死活只用直接证据：显式 EXIT 标记行或 ps 按 argv，缺失输出只记 UNKNOWN 不得反推死亡，launcher 会把 PID 交棒给子进程）；WSL df -h（<60G 先修剪：docker builder prune -af、已注册 pilot 的 snapshot-index-artifacts-full/profile-artifacts 副本——先核 deployment-record 无引用再删）；goproxy :3000 不通就跑 ~/stage-e/rebind-proxy.sh；每条 ssh 命令带 umask 022，Go 在 /home/wmm/.local/bin。(2) 复核上个唤醒的产出——自报通过不作数，要实测出处（exit code、digest、逐格比对）。(3) 按优先级接着推进：docs/p9_new_evidence_program.md 的 P9 阶段（当前主线：P9.C profile 生成→激活定点→pilot→注册→论文；随后 P9.D 片段扩展、P9.E 规模、P9.F 对比）> 台账尾部未完成项 > 盘点。长任务（campaign/定点/链）一律 nohup 后台化并挂 marker 看守，等待期做不依赖它的事（下一项的语料/设计/NAS 侧论文），但不在正在测时延的 WSL 上跑重活。(4) 硬边界：封存件永不改；扩展实验类名保持 pilot 不挪用 publication_eligible；不 relax 门禁去迁就已产数据（改的是自己的杠杆）；catalog 改动全部凑齐后由作者一次签署（P9-SIGN-HOLD），签署与批准记录永不代签；git push 一律带 pull --rebase 重试并回显确认。(5) 命中三类上报（影响发表/环境自复失败/须作者亲手）就写进台账与 handoff 并转做其他可做项；失败先查 df 与真实日志再定性，改口给根因记台账。(6) 台账 append-only 记本唤醒的决定与证据。全程遵守 CLAUDE.md 与查证十条。P9 全部自执行项做完且余项均须作者时，写 handoff 说清在等什么，再 ScheduleWakeup stop。
```

要点（任务词的出处，帮下个会话理解为什么这么写）：

- **看守判终态只认显式标记**（如 PILOT_EXIT=/CHAIN_DONE 行）：launcher 进程会交棒，按 PID 判死已两次误报。
- **不 relax 门禁迁就数据**：pilot-counter-rigor-01 教训——samples=3 flag 对抗刻意的 mechanism-smoke 契约，
  正确杠杆是 repetitions=3；错了就回退自己的改动重跑。
- **盘水位**：定点跑需 15-18G，campaign 每部署约 6G；rigor 类 12 部署约 54G；builder cache 是最大漏。
- **一次签署**：会改 catalog 的实验（P9.D/E/F）先全部落 pilot 定点，最终 catalog 凑齐后作者只签一次。
- **激活定点引导序**：新 profile catalog 先跑 attest（首跑在激活处崩溃属预期）→ 用 attested digest 重生成 catalog → 二跑绿。

看守：长任务配 detached 看守（marker 轮询，5 分钟一轮），用直接证据报里程碑、失败、终态；
ssh 取不到状态记 UNKNOWN，**不得以缺失输出反推死亡**。
