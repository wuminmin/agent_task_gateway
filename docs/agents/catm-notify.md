# CATM 通知命令（按需读取）

作者只看手机通知，**只有 `catm-notify say` 会到手机**；`sync` 只刷新会话状态。CATM 不是 MCP，
是本地命令（`catm-notify` skill 承载）；仓库里另有 `./scripts/catm-notify.sh`（子命令 `sync|notify|done`，与全局命令不同名），
仅作后备。

```bash
catm-notify start "<任务名>"      # 开/复用本工作区会话
catm-notify sync  "<当前阶段>"    # 只刷新状态，不到手机（可加 --status working|waiting_author|…）
catm-notify say   "<心跳三行>"    # 到手机的唯一通道
catm-notify done - <<'CATM'       # 收尾，与终端最终答复一字不差
<终端最终答复原文>
CATM
```

- 全局命令用 **`say`**，没有 `notify`（误用只打 usage 不发送）。
- **正文一律用文件或 stdin 传递**（`say "$(cat file)"`），不内嵌反引号、嵌套双引号、撇号。
- 从非仓库 cwd（如看守脚本）发送要加 `--workspace <仓库路径>`，否则落到另一个会话。
- CATM 偶发 `unreachable`，发送要带重试（看守脚本已内置 4 次）。

## 心跳格式（作者 2026-08-16 定）

里程碑即时发（派工、复核完、路线级事实、停手、需作者动手、阶段结论、更正自己的错误）；此外连续工作期间
**每 30 分钟至少一条**，三行：
1. 在做什么 + 已耗时；
2. 本窗口实测事实（带数字与提交号；没有就写"无新事实"）；
3. 是否需要作者动作（没有就写"无需作者动作"）。

时间戳一律 UTC+8（`TZ=Asia/Shanghai date`），**在同一条命令内插值，不得手写**。坏消息照发。
"不刷屏"只约束同一窗口内重复发同一件事；等作者期间无新事实不重复发。
