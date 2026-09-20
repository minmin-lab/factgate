# 评测执行环境（按需读取；会过期，判据以活体配置与实测为准）

## 执行机与仓库路径（2026-08-27 立）

TKDE 评测（campaign / route-matrix live / SQL 门等 harness 活）在 **WSL2** 上跑，NAS 不能跑 harness
（无 go、无 pgrep、无 docker harness 依赖）。**WSL 与 NAS 两处仓库都要与 origin 同步。**

- **WSL2 仓库**：`/home/wmm/worktrees/agent_task_gateway`
- **NAS 仓库**：`/volume1/homes/wuminmin/github/minmin-lab/factgate`（= `/var/services/homes/...` 同一文件系统；2026-09-07 由
  `github/wuminmin/agent_task_gateway` 改名而来，旧路径留符号链接指向新路径，供仍在跑的旧 Claude 会话与历史脚本使用；
  `~/stage-e-nas/watch-{e1,campaign}-nas.sh` 的 `WS=` 已改新路径）
- **origin（2026-09-07 起）**：两处仓库的 origin 都是 `https://github.com/minmin-lab/factgate.git`（原 wuminmin/agent_task_gateway，
  GitHub 对旧 URL 自动跳转）。NAS 目录名未改。NAS 上 git 在 `/opt/bin`、gh 在 `~/.local/bin`，非登录 ssh 会话要用 `bash -lc`；
  git 凭证走 `gh auth setup-git` 配的 helper，**不匿名访问 GitHub**（作者定，见全局 CLAUDE.md）。
- **NAS→WSL2 连接**：Tailscale 节点 `wmm-wsl`（`100.73.90.49`），用户 `wmm`，专用密钥 `~/.ssh/id_ed25519_taskgate`
  ```sh
  ssh -i ~/.ssh/id_ed25519_taskgate -o StrictHostKeyChecking=accept-new \
      -o UserKnownHostsFile=$HOME/.ssh/known_hosts_tailscale wmm@100.73.90.49
  ```
- 远端长命令输出一律落远端文件再读；ssh 中继会随时断。后台任务用 `nohup setsid … &`，
  同一 ssh 会话内不要让后台进程持有 stdout（会挂住 ssh）。
- 不在 git 里的驱动脚本：WSL `~/stage-e/`（E1/E2、qualification 链）、`~/formal-v111/`（正式 campaign 发射/叫停）、
  NAS `~/stage-e-nas/`（历史看守脚本；其中的 catm-notify 调用已随 CATM 废弃失效，重用前先删）。

## 网络（2026-08-27 实测）

WSL2 出网走 **Tailscale exit node = NAS（`ds423plus` `100.72.87.34`）**，宿主与容器流量都经此出口（实测出口 IP
均为 `131.226.101.166`）。**WSL↔NAS 未建直连、走 peer-relay**（`tailscale ping ds423plus` 报
`direct connection not established`），中继慢时实测下载仅 ~15 KB/s；大文件（apt 索引、go 模块）会超时。

**formal Gateway build 的绕法（纯传输韧性，`go.sum`/digest-pin 仍决定字节）：**
- **go 模块**：WSL2 起本机 GOPROXY 服务已缓存模块（局域网实测 41 MB/s），经 `TASKGATE_FORMAL_BUILD_GOPROXY` 透传：
  ```sh
  # eth0 IP 会变，用时现查：ip -4 addr show eth0
  python3 -m http.server 3000 --bind 0.0.0.0 --directory ~/go/pkg/mod/cache/download &
  export TASKGATE_FORMAL_BUILD_GOPROXY="http://<eth0-ip>:3000,https://proxy.golang.org,direct"
  ```
  buildkit RUN 步够不到 docker0 网关 `172.17.0.1`，**必须用 eth0 IP**。
- **apt**：`Dockerfile.formal` 的 apt-get 带 `Acquire::Retries=8` + 60s 超时。
- **构建超时**：`final-v5-gateway-build` 的 `buildTimeout` 90m。

### mirrored 网络模式（2026-08-27 作者定，已试；与 tailscale 冲突后弃用）

`.wslconfig` `[wsl2] networkingMode=mirrored` 曾作为根治 relay 的尝试。**任何 WSL 重启后**逐项重核：
1. DNS：`cat /etc/resolv.conf`（`generateResolvConf=false` 手工配置）、`getent hosts proxy.golang.org github.com`。
2. NAS→WSL2 SSH 能否连；不通查 `tailscale status` / `tailscale ip -4`。
3. `tailscale ping ds423plus` 是否 direct。
4. 出网速度 `curl -o /dev/null -w '%{speed_download}\n' https://proxy.golang.org/...`。
5. eth0 IP 是否变（本机 GOPROXY 地址、`~/stage-e/*.sh` / `~/formal-v111/run.sh` 里写死的 IP）。
6. 仓库 `git status` 干净、HEAD==origin 再继续。

## 磁盘与重启后核灾（2026-08-28 事故后立）

2026-08-28 07:33–07:43 作者把 WSL VHD 从 C 盘迁到 D 盘（同一台机器、同一 VHD 文件）。迁后实测顺序写 4.8 GB/s、读 6.2 GB/s、4k fdatasync ~880 IOPS，不慢于旧盘（P77 记 1.8 GB/s）。

WSL 根 VHD（`/dev/sdc`, ext4, `errors=remount-ro`）曾在正式 campaign 中进入 `emergency_ro`。重启后必查：
`grep " / " /proc/mounts` 无 `emergency_ro`、`dmesg | grep -iE "ext4|I/O error"` 为 0、写探针成功、
worktree `git fsck --no-dangling`、私有材料摘要（P45 signed binding `3bb2771f…` 110584B、`.env`）、
`docker info`、formal 镜像、go 模块缓存、残留 compose 项目清零、模块代理重启。
私有材料不得由 Claude 复制出 WSL（权限策略），由作者从 Windows `\\wsl$` 自备份。

## WSL ssh 会话的 umask 是 0002（2026-08-29 实测）

非登录 ssh 会话下新建文件 664 / 目录 775；`finalv5publication` 的安全谓词（`approval.go:339/348`，`Perm()&0o022 != 0`）
会把组可写的仓库文件/目录判为不安全，`git worktree add` 出来的第二工作树因此整包测试失败，`t.TempDir()` 下
`MkdirAll(0o777)` 也会被拒。**所有跑测试/门禁的包装脚本一律先 `umask 022`**；新工作树检出后 `chmod -R g-w`。

## 正式 campaign 期间的硬约束

- **不 push**：`final-v5-attestation-footprint`（`provenance.go`）与 `formalbuild.MaterializeHead`（`source.go`）都要求 HEAD==origin，
  verify-build 每个部署都跑；push 会让下一部署失败。NAS 可本地提交，campaign 结束后再推。
- **不动 WSL 工作树、不在 WSL 跑重活**（正式轮在测时延）。
- publication launcher 拒绝覆盖 campaign root、**不能续跑**；中断即整轮重跑新 ID。
- 估时只按同 class/同密度的实测给：pilot 试跑是每格 1 样本，正式轮是冻结密度（如 baseline 每格 30）。

## 2026-09-19 执行机重建后的现状（覆盖上文路径；判据以实测为准）

- WSL2 由作者当日重建（`/mnt/d/WSL/provision.sh`；VHD `/mnt/d/WSL/Ubuntu-22.04/ext4.vhdx`，`/dev/sdc` 1007G）。仓库在 **`/home/wmm/worktrees/factgate`**；`~/stage-e`、`~/formal-v111`、GOPROXY 服务、旧 worktree 均不存在。
- tailscale 节点名仍 `wmm-wsl`，IP 改为 **100.68.97.89**；NAS 走 `ssh nas`（192.168.8.233，密钥 `~/.ssh/id_ed25519`，非登录会话 git 在 `/opt/bin`）。
- Go 1.25.1 在 `~/.local/go`，`~/.local/bin/go`（Claude 装）。无 gh、无 GitHub 凭证：**push 路由 = `git push nas <branch>` → `ssh nas 'bash -lc "cd …/factgate && git push origin <branch>"'`**（remote `nas` 已配 receivepack/uploadpack=/opt/bin）。
- **pilot 原始目录缺失**：pilot-evidence-v1.json 的 17 个 pilot 与 p10-b6-throughput-01 的 raw 不在本机/NAS/D: 任一处（详见台账 P10-R2-LOOP-1）；论文容器构建在 `final_v5_pilot_evidence.py` 校验处失败，待作者恢复。
- 日志目录 `~/logs/`（Claude 建）。
- **MinIO 镜像已从 Docker Hub 下架（2026-09-19 实测 `pull access denied for minio/minio`、`minio/mc`）**，compose.yaml 钉的是 tag（`minio/minio:RELEASE.2025-04-22T22-12-26Z`、`minio/mc:RELEASE.2025-04-16T18-13-26Z`，无摘要）。绕法（不改冻结文件）：`docker pull quay.io/minio/<name>:<same tag>` 后 `docker tag` 成 compose 引用的名字；compose 发现本地已有该 tag 就不再拉取。重建 WSL 后必做一次。
- 本地开发栈：`.env` 已按 docs/getting-started.md 用本地生成的密钥写好（mode 600，不入库）；`docker compose up --build -d --wait` 日志在 `~/logs/devstack-up-*.log`。

## WSL VHD 压缩（2026-09-20 作者实测，步骤以此为准）

WSL 内删除的空间不会自动回到 D 盘：`ext4.vhdx` 只增不减。回收步骤（作者在 Windows 管理员 PowerShell 做，Claude 无法代做）：

1. **先在 WSL 里 `sudo fstrim -av`**——这一步是关键，缺了它 compact 基本回收不到东西（实测 fstrim 报 892 GiB 后 compact 才真缩）。
2. `wsl --shutdown`，等约 10 秒。
3. `Optimize-VHD -Path "D:\WSL\Ubuntu-22.04\ext4.vhdx" -Mode Full`（Win11 Home 亦可用，vmms/vmcompute 在跑即可；无 Hyper-V 才退回 diskpart compact vdisk）。
4. **不要开 `wsl --manage <distro> --set-sparse true`**：WSL 2.7.11 已因数据损坏风险禁用稀疏 VHD（E_INVALIDARG，需 --allow-unsafe 强开），活数据无第二份时不值得。代价是每次都要手工压。

实测一次：回收约 405 GiB、耗时 2 分 39 秒（521G → 117G）。压缩后复核 ext4 挂载干净、随机数据写回 sha256 一致、df 与压缩前一致、worktree git fsck 无错。
