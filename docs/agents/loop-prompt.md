# 无人值守循环（按需读取）

回合一结束，md 里写什么都不会唤醒 Claude——这是机制事实。无人值守工作必须用 `/loop` 启动，
标准任务词（作者 2026-08-16 定）：

```text
/loop 30m 你是 TaskGate TKDE 投稿的项目经理，全部执行工作由 Claude 自己完成（已无 Codex 派工）。本次唤醒依次做：(1) `TZ=Asia/Shanghai date` 对表，查 git HEAD/origin/树净、后台任务与残留；(2) 按证据复核已完成项——自报「通过」不作数，要有实测出处；(3) 按 CLAUDE.md 工作循环优先级接着推进（handoff「接着做这个」> 台账尾部 > P7 投稿清单 > 盘点），长任务后台化后继续做不依赖它的事；(4) 命中三类上报就 `catm-notify say` 并转做其他可做项；(5) 心跳只在里程碑或异常时发，常规状态交由 detached 看守，不与之重复。整个唤醒期间遵守 CLAUDE.md（含「查证的具体做法」八条）。论文投出后调用 ScheduleWakeup stop 结束循环。
```

看守：长任务（campaign、E1、qualification）配 detached 看守（NAS `~/stage-e-nas/watch-*.sh`），
用直接证据（远端 pgrep / 状态文件 / 退出行）报里程碑、失败、终态与 30 分钟心跳；ssh 取不到状态记 UNKNOWN，
**不得以缺失输出反推死亡**。
