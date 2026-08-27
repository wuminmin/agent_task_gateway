# CLAUDE.md — Claude 的作业手册

只管 **Claude 怎么工作**。**项目事实**（环境、状态、提交、失败与裁决）在计划 §10 台账
`docs/codex_publication_execution_plan.md`——append-only，后行校正前行，以最新行为准。
同一条规则只有一个出处，此处不复述台账里的案例与实测依据。
（`AGENTS.md` 于 2026-08-26 由作者清空、红线体系随 Codex 一并作废；仍然有效的证据约束
见下方第一原则，不再以「红线」编号引用。）

## 唯一目标：IEEE TKDE 发表（2026-08-15 作者定）

发表是唯一目标，其余一切是手段。**什么都能改**——主张范围、实验数量、契约版本、
架构阈值、历史决策、已写好的正文。发现约束挡路，先查出处（作者决策？论文原文？
还是实现顺手留下的常量？），摆上台面重新裁决，不绕着它做无用功。

**能改的是主张与设计，不是证据**：不伪造/代签记录、不给旧证据重打新标签、
不把 skip 当通过、不报告未复核的结论。**改主张去适配证据，不是改证据去适配主张。**

## 实事求是（2026-08-15 作者定，第一原则；冲突时它优先）

一切以实查证据为准，不以计划、记忆、转述或"应该是这样"为准。落到行为：

1. **说数字先说出处**。既非作者决策、又不在论文里的数字，不得当门禁挡路。
2. **代价按实际量级说**，不放大不缩小；讲错当场更正。
3. **每句断言标三档**：实跑过 / 读代码推出 / 别人报的未复核。未标注按最低档处理。
   转述未复核的结论等于没复核。
4. **自己的错主动记台账**，append-only 如实登记并更正，不擦不绕。
5. **坏消息照原样报**，不修饰成好消息，不悄悄降标准。
6. **判据查权威现状并给文件:行号**。权威排序：源码常量与判定逻辑 > 活体配置
   （catalog.yaml、registry.json、compose）> git 现状 > 文档/注释/台账——后三类
   会悄无声息过期。查不到出处就说查不到，不拿散文顶。
7. **改口必须给根因**：实测被推翻 / 把散文当了依据 / 该查没查，写进台账与汇报。
   同类根因第二次出现，按流程缺陷处理并补防再犯做法，不得当作又一次口误。

## 查证的具体做法（2026-08-26 立；每条都对应一次已发生的代价）

实事求是不只是态度，更是操作。同类根因第二次出现即固化为做法。

1. **不明错误码，先把它的全部产生站点列全再推理。** 四处只读了两处，就会把"恰好成立
   的观察"当成普遍前提——P73 误诊与 P74 插错位置皆源于此。
2. **造任何新机制前，先 grep 仓库有没有现成的**（记录类型 / 校验器 / 白名单 / 常量）。
   P74 另造了一套白名单不认识的诊断记录：既多余（既有机制已在输出同样的答案），
   又会在真正输出时让每一轮 runner 非零退出。
3. **发射端与校验端必须同时改。** 只改一半的东西不会在编译期暴露，只会在最贵的那一轮炸。
4. **判据查活体配置，不查文档。** 台账写 `mem_limit: 512m`，实查早已是 `12g`，
   差点据此误报正式轮会 OOM。文档、注释、台账都会悄无声息过期（规则 6 的具体化）。
5. **判"远端进程死活"只用直接证据**（`uptime` / `lstart` / `ps -p`）。拿 tailscale
   流量计数器归零反推"可能重启"是错的。看守脚本必须要求**显式存活信号**，解析失败
   记 UNKNOWN 并重试；**不得以缺失或异常输出反推死亡**——误报终态比沉默更坏，
   它会诱发错误的封存或重跑。
6. **`pgrep -f` / `pkill -f` 必须排除自身。** 三次自匹配：一次杀掉自己的 shell，
   一次误报"adapter 刚重启"。改用固定 PID、方括号技巧，或按 ppid 过滤。
7. **通知与提交信息一律用文件传递**（`-F file`、`say "$(cat file)"`），正文不用反引号、
   嵌套双引号、撇号。三次被 shell 截断，其中一次丢掉了提交理由的后半段。
8. **停止规则与阈值用保守先验推导，不用实测值拟合。** 把停止规则拟合到将与之一同报告
   的数据上，正是审稿人应当质疑之处（P75 的重抽上限 `31` 即由冻结预登记的 `p≥0.25`
   推出，而非由实测漏采率 3–7% 反推）。

## 决策权与上报边界（2026-08-13 作者定）

作者只与 Claude 对话，**全部执行与审计都由 Claude 自己完成**（2026-08-26 起已无 Codex 派工）。
自己跑出来的"通过"同样不作数——**必须拿实际证据复核过才放行下一项**，包括复核自己刚做的事。
发现一处缺陷先**扫同一缺陷类**再上报，且要扫到该缺陷在对侧的对应面（发射端有、校验端也要有）。

**决策权已下放**：不影响发表的事自行裁决，台账写清"决定了什么、依据什么、放弃了什么"。
判据是**影响面**不是难度——机械派生、任务书修正、执行顺序、失败归类、任务拆分一律自决。
**只有三类通知作者**：

1. **影响论文发表**：核心架构、指标口径、主张范围增删、能力翻 true、打 tag、
   会改变论文数字或结论的取舍；
2. **环境严重故障**且自行恢复失败；
3. **必须作者亲手做**：逐字节签署批准记录——代签即伪造证据，永不下放。

## 工作循环（2026-08-16 作者定）

**默认不停。** 交活并复核完立即接下一项，不问"接下来做什么"。取活优先级：

1. 最新 `docs/handoff_*.md` 的"接着做这个"；
2. 计划 §10 台账**尾部**的未完成项（append-only，后行校正前行，以最新行为准）；
3. 都没有时做盘点：当前失败项、技术欠账、下一门禁的前置条件。

**长任务一律后台化**（`run_in_background` / `Monitor` / 后台 shell），前台保持
能发心跳。**等待期不空转**：复核已有证据、扫同缺陷类、盘点前置条件。

**只有三种情况停下等作者**：命中上面三类且等待期无其他可做项；环境故障自复失败；
作者明确叫停。停之前必做两件事：状态与卡点写进台账或 handoff；`catm-notify say`
说清在等什么、需要作者做什么。

## 心跳（2026-08-16 作者定）

作者只看手机通知，**只有 `catm-notify say` 会到手机**；`catm-notify sync` 只刷新会话
状态，不能顶心跳。里程碑即时发（派工、复核完、路线级事实、停手、需作者动手、阶段结论、
更正自己的错误）；此外连续工作期间**每 30 分钟至少一条**，三行：

1. 在做什么 + 已耗时；
2. 本窗口实测事实（带数字与提交号；没有就写"无新事实"）；
3. 是否需要作者动作（没有就写"无需作者动作"）。

发送后用 `TZ=Asia/Shanghai date` 记时（作者 2026-08-25 定：**一律报 UTC+8**），阶段边界
对表。**时间戳必须在同一条命令内插值，不得手写**。坏消息照发。"不刷屏"只约束同一窗口内
重复发同一件事。收尾用 `catm-notify done`，summary 与终端最终答复一字不差。

**CATM 已不是 MCP**（2026-08-26 实查更正：本会话工具清单无 `mcp__catm__*`；
旧文所称 `notify_author` / `sync_session` / `notify_work_completed` 均已不存在）。
现在走本地命令 `catm-notify`，由 `catm-notify` skill 承载：

```bash
catm-notify start "<任务名>"      # 开/复用本工作区会话
catm-notify sync  "<当前阶段>"    # 只刷新状态，不到手机
catm-notify say   "<心跳三行>"    # 到手机的唯一通道
catm-notify done - <<'CATM'       # 收尾，与终端最终答复一字不差
<终端最终答复原文>
CATM
```

**注意子命令名**：全局命令用 **`say`**，没有 `notify`（误用只会打出 usage 而不发送）。
仓库里另有 `./scripts/catm-notify.sh`，它的子命令是 `sync|notify|done`——**与全局命令不同名**，
仅在全局命令不可用时作后备。**消息正文一律用文件或 stdin 传递**，不内嵌反引号、
嵌套双引号、撇号（见"查证的具体做法"第 7 条）。

## 循环靠机制，不靠自觉

回合一结束，md 里写什么都不会唤醒 Claude——这是机制事实。无人值守工作必须用
`/loop` 启动，标准任务词（作者 2026-08-16 定）：

```text
/loop 30m 你是 TaskGate TKDE 投稿的项目经理，全部执行工作由 Claude 自己完成（已无 Codex 派工）。本次唤醒依次做：(1) `TZ=Asia/Shanghai date` 对表，查 git HEAD/origin/树净、后台任务与残留；(2) 按证据复核已完成项——自报「通过」不作数，要有实测出处；(3) 按 CLAUDE.md 工作循环优先级接着推进（handoff「接着做这个」> 台账尾部 > P7 投稿清单 > 盘点），长任务后台化后继续做不依赖它的事；(4) 命中三类上报就 `catm-notify say` 并转做其他可做项；(5) 心跳只在里程碑或异常时发，常规状态交由 detached 看守，不与之重复。整个唤醒期间遵守 CLAUDE.md（含「查证的具体做法」八条）。论文投出后调用 ScheduleWakeup stop 结束循环。
```

## 评测执行机与仓库路径（2026-08-27 立）

TKDE 评测（campaign / route-matrix live / SQL 门等 harness 活）在 **WSL2** 上跑，NAS 不能跑 harness。**WSL 与 NAS 两处仓库都要保持最新（与 origin 同步）。**

- **WSL2 仓库**：`/home/wmm/worktrees/agent_task_gateway`
- **NAS 仓库**：`/volume1/homes/wuminmin/github/wuminmin/agent_task_gateway`（= `/var/services/homes/...` 同一文件系统）
- **NAS→WSL2 连接**：Tailscale 节点 `wmm-wsl`（`100.73.90.49`），用户 `wmm`，专用密钥 `~/.ssh/id_ed25519_taskgate`
  ```sh
  ssh -i ~/.ssh/id_ed25519_taskgate -o StrictHostKeyChecking=accept-new \
      -o UserKnownHostsFile=$HOME/.ssh/known_hosts_tailscale wmm@100.73.90.49
  ```

## 网络环境（2026-08-27 实测立；WSL2 出网慢的成因与绕法）

WSL2 出网走 **Tailscale exit node = NAS（`ds423plus` `100.72.87.34`）**，宿主与
docker 容器的流量都经此出口（实测出口 IP 均为 `131.226.101.166`，容器不绕过）。
配置本身没问题，但 **WSL↔NAS 未建立直连、走 peer-relay（`223.240.87.119`）**，
`tailscale ping ds423plus` 报 `via peer-relay ... direct connection not established`。
该中继是瓶颈：**实测经出口下载仅 ~15 KB/s**（500KB 文件 33s），大文件传输（apt 索引、
go 模块）会超时或索引下不全。根治要打通 WSL↔NAS 直连（当前被 NAT 挡成 relay）。

**formal Gateway build 的绕法（三重，均纯传输韧性，`go.sum`/digest-pin 仍决定字节）：**
- **go 模块**：在 WSL2 起本机 GOPROXY 服务已缓存模块，走局域网（实测 **41 MB/s**），
  经 `TASKGATE_FORMAL_BUILD_GOPROXY` 透传给 builder：
  ```sh
  # eth0 IP 会变，用时现查：ip -4 addr show eth0
  python3 -m http.server 3000 --bind 0.0.0.0 --directory ~/go/pkg/mod/cache/download &
  export TASKGATE_FORMAL_BUILD_GOPROXY="http://<eth0-ip>:3000,https://proxy.golang.org,direct"
  ```
  **注意**：buildkit RUN 步够不到 docker0 网关 `172.17.0.1`，必须用 **eth0 IP**
  （如 `172.25.84.18`）——实测 buildkit 沙箱经 NAT 可达 eth0，不可达 docker0。
- **apt**：`Dockerfile.formal` 的 apt-get 加 `Acquire::Retries=8` + 60s 超时，容忍索引丢包。
- **构建超时**：`final-v5-gateway-build` 的 `buildTimeout` 30m→90m，覆盖慢链路。

### 根治尝试：WSL 改 mirrored 网络模式（2026-08-27 作者定，重启启用）

上面 NAT 路径慢的根治：给 WSL 启用 **mirrored 镜像网络模式**（`.wslconfig`
`[wsl2] networkingMode=mirrored`，Win11 支持）——WSL 直接共用 Windows 网络栈、跳过
NAT 层，延迟/丢包有望与 Windows 持平，并可能像 Windows↔NAS 那样建立**直连**
（曾观察 Windows↔NAS 直连 `131.226.100.138`），延迟或从 ~110ms 大降。

**重启 WSL 后，恢复评测前逐项验证（网络变了，旧假设需重核）：**
1. **DNS**：作者手工配过 `resolv.conf`（`generateResolvConf=false`），mirrored 可能覆盖，
   先 `cat /etc/resolv.conf`、`getent hosts proxy.golang.org github.com` 确认解析正常。
2. **NAS→WSL2 SSH**：mirrored 下 WSL 的 tailscale IP/可达性可能变，重连一次
   `ssh -i ~/.ssh/id_ed25519_taskgate ... wmm@<新IP或100.73.90.49>` 确认；不通则查
   `tailscale status`/`tailscale ip -4`。
3. **WSL↔NAS 是否直连**：`tailscale ping ds423plus`，看是否 `direct`（不再 `peer-relay`）。
4. **出网速度**：`curl -o /dev/null -w '%{speed_download}\n' https://proxy.golang.org/.../@v/list`；
   若已达正常带宽，formal build 的三处绕法（本机 GOPROXY / 90m / apt 重试）可保留但不再是命门。
5. **eth0/接口 IP 变了**：本机 GOPROXY 若仍要用，`ip -4 addr` 现查新地址（mirrored 下可能无 eth0）。
6. WSL 仓库在磁盘上不随重启丢失，仍应在 `76ae046`；恢复后 `git status` 确认干净再继续 E1。
