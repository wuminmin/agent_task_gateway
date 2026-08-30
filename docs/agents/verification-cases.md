# 「查证的具体做法」的案例出处（按需读取）

AGENTS.md 的八条做法每条都对应一次已发生的代价；同类根因第二次出现即固化为做法。台账
（`docs/codex_publication_execution_plan.md` §10）有全文。

1. **列全产生站点再推理**：P73 误诊与 P74 插错位置——四处只读了两处，把"恰好成立的观察"当成普遍前提。
   同类：v1.11 A–D 只改发射端，五处校验端仍钉 178 格（`649eadb` 补齐）；E0 缺陷 (a)（生成器读的 workloads 文件漏改）。
2. **先 grep 现成机制**：P74 另造了一套白名单不认识的诊断记录，既多余又让每轮 runner 非零退出。
3. **发射端与校验端同时改**：只改一半不会在编译期暴露，只会在最贵的那一轮炸（launcher 收尾 jq 门若不改会在 5 小时后才炸）。
4. **判据查活体配置**：台账写 `mem_limit: 512m`，实查早已是 `12g`，差点据此误报正式轮会 OOM。
5. **远端进程死活只用直接证据**：拿 tailscale 流量计数器归零反推"可能重启"是错的；
   2026-08-28 tailscale 显示 offline 时 ssh 仍通过一次——"offline"不等于主机死亡。
6. **pgrep/pkill 排除自身**：三次自匹配（杀掉自己的 shell、误报 adapter 重启）；NAS 无 pgrep，bracket-grep 仍会匹配
   Claude 的 wrapper shell，用 pidfile 或按 argv 字段过滤。
7. **通知与提交信息用文件传递**：三次被 shell 截断，一次丢掉提交理由后半段。
8. **停止规则用保守先验**：P75 重抽上限 31 由冻结预登记的 `p≥0.25` 推出，不由实测漏采率反推。

## 2026-08-27/28 新增案例

- `-root` + 绝对路径：route-matrix 用 resolvePath 兼容，cache-isolation-live 用 filepath.Join 不兼容——改一处必须
  grep 同脚本全部 `-root` 调用并核对各工具实现（同类第二例）。
- 失败路径先 `docker logs` 落盘再 `down`：trap 里的 down 清掉了 installer 日志，多跑一次复现才拿到直接证据。
- db-test harness `up --wait` 在一次性 installer 刚 running 就返回（P48b 时已观察、手工绕过），`755dccd` 根治。
- 估时前先核该估算所用运行的 class/密度：P93 试跑是 pilot 密度（每格 1 样本），不能推正式轮时长。

## 2026-08-31 新增案例

- `go test -cpuprofile` 会把测试二进制（`gateway.test`，40 MB ELF）写进当前目录；repohygiene 测试随后把 DB 套件判红。剖析跑完即删，
  或用 `-o` 指到 `generated/bin/`。
- 非交互 ssh 会话的 umask 是 0002（登录 shell 是 0022）：`t.TempDir()` 得到 775，十余个「directory is group- or world-writable」
  失败全是环境项，不是代码回归。测试/发射脚本内显式 `umask 022`；主工作树 `git pull` 落下的文件也是 664，跑门禁前 `chmod g-w,o-w`。
- 私有 Dataset Binding 钉住 `config/catalog.yaml` 的 SHA-256（`finalv5binding/binding.go:214`）：改该文件的任何一字节都让
  Scale/Artifact/ProvSQL 格拒绝发射，报错文字（「currently valid private Dataset Binding」）不点名根因。改 Catalog 前先想清是否
  值得一次作者重签；能放进 profile Catalog 或测试 Catalog 的改动不要进主 Catalog。
- campaign 运行中不得动它的工作树：pilot-09 跑到部署完成后被「running Gateway image names revision e6a5f0d but the verified source is
  bfb6a5b」判死——我为排队下一个 campaign 把 `~/wt-pilot` 快进了。要换提交就等它结束，或用另一个工作树。预检失败留下的
  `raw/<campaign-id>` 目录会让同 id 重发报「refusing to overwrite」，先看内容再删。
- 带 build tag 的文件默认构建不编译：`provsql_live_dsn_test.go` 移到 `taskgate_integration` 后丢了 `bytes` 导入，主机 `go vet ./...`、
  RQ1 容器、DB 套件全绿，只有链的 compose 验收（带该 tag）报 `[build failed]`，白跑 40 分钟。发链前 `go vet -tags taskgate_integration ./...`。

