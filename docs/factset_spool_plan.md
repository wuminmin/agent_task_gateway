# FactSet 内存优化计划

> **2026-08-16 作废**：本文原目标是「恢复 RQ4 空间门槛（Gateway cgroup peak ≤ 512 MiB）」。
> 该门槛经查**既非作者决策、也不在论文中**（全文检索 `main.tex`/`supplement.tex` 零命中），
> 已由台账 `AUDIT-decisions` 行判定不再作为任何门禁。**落盘工作本身已随 v1.8 冻结、结论有效，
> 但其既定理由不成立**；本文不得再被引用为「512 MiB 门禁」的依据。
> 当前真实内存事实见 `docs/dbtest_and_9of9_plan.md` 第 9.8 节。
>
> **核心原则**：这不是性能优化，是用延迟换余量。允许写盘/读回/加解密开销，
> 换来同样的宿主机能吃下更大的 workload。

---

## 红线约束（本计划必须遵守）

1. **红线 6：只允许替换，禁止并存**
   - 不得出现"小的走内存、大的走磁盘"两条派生实现。
   - 要么统一走一个自适应结构（内存态只是阈值以下形态），要么不做。
   - 第二份实现会漂移，而漂移会被当成测量结果读。

2. **红线 8：改运行时决策先于冻结**
   - 必须在 v1.5 冻结前完成。
   - 冻结后再落地等于作废整轮 campaign。

3. **红线 6 附加：关键推导只允许一份实现**
   - FactSet 承载的是关键推导（28 个 indexed digest、123 格 binding、所有留存证据）。
   - 任何改动必须证出 digest 恒等，否则下桌。

---

## 总体顺序

```
阶段 0: 建立差分恒等判据（照抄 593ce83）
   │
   ▼
阶段 1: key 改 [32]byte —— 小改动、同一套证明、先验证路子
   │
   ▼
阶段 2: value 落盘 —— 照抄 encrypted_spool 模式
```

**阶段 0+1 先做，阶段 2 等验证后再启动。**

---

## 阶段 0：建立差分恒等判据

### ID
`P5-mem-0`

### 前置
- 无

### 目标
建立 FactSet 改动的差分测试基础设施，照抄 593ce83 的 oracle 模式。
保留旧实现作为 oracle，逐字节比对，确保 digest 恒等。

### 步骤

1. **读 593ce83 的差分测试模式**
   - 文件：`internal/exposure/outcome_digest_streaming_test.go`
   - 核心思想：保留旧实现 `bufferedReleaseOutcomeDigest` 作为 oracle，
     新实现 `ReleaseOutcomeDigest` 必须在所有输入上与其逐字节一致。

2. **为 FactSet 建立类似的 oracle 测试**
   - 创建 `internal/exposure/factset_differential_test.go`
   - 实现旧版 FactSet 的 oracle（当前 `map[string]FactID` 实现）
   - 覆盖以下操作：
     - `NewFactSet`
     - `Add`（去重、冲突检测）
     - `Values`（排序）
     - `Clone`
     - `MergeChecked`

3. **差分测试用例**
   - 空集合
   - 单元素
   - 已排序集合
   - 未排序切片
   - 重复事实坍缩
   - 大集合（压力测试）
   - 随机化测试（300 轮随机大小/顺序/重复）

4. **负控制测试**
   - 证明 oracle 能检测差异
   - 不同输入必须产生不同 digest

### 验收

```bash
# 1. 差分测试必须通过
go test -v -run 'FactSet.*Differential' ./internal/exposure

# 2. 随机化测试必须通过
go test -v -run 'FactSet.*Randomized' ./internal/exposure

# 3. 负控制必须通过
go test -v -run 'FactSet.*NegativeControl' ./internal/exposure

# 4. 全量门禁
gofmt -l $(git ls-files '*.go')
go build ./...
go vet ./...
go test -count=1 ./internal/exposure/...
```

**所有测试必须实际 PASS，不得 SKIP。**

### 产出
- `internal/exposure/factset_differential_test.go`
- 本阶段提交信息末行注明 `P5-mem-0`

---

## 阶段 1：FactSet key 改 [32]byte

### ID
`P5-mem-1`

### 前置
- `P5-mem-0` 完成（差分判据建立）

### 目标
将 FactSet 的 key 从 `string`（64 字符 hex）改为 `[32]byte`。
去掉 `hex.EncodeToString` 开销，减少每 Fact 约 80 B 内存。

**这是小改动，但用同一套差分证明机器验证路子。**

### 改动范围预估

**核心定义**（1 处）：
- `internal/exposure/fact.go:1528` - `type FactSet map[string]FactID` → `map[[32]byte]FactID`

**调用方**（预估 98 处非测试引用，19 个文件）：
- `internal/exposure/*.go` - 约 50 处
- `internal/gateway/*.go` - 约 20 处
- `evaluation/finalv5oracle/*.go` - 约 15 处
- `evaluation/internal/experiment/*.go` - 约 13 处

### 步骤

1. **扩展 Hash() API**
   - 保留 `Hash() (string, error)` 现有签名（向后兼容）
   - 新增 `HashBytes() ([32]byte, error)` 返回原始字节
   - 内部共用同一实现

2. **修改 FactSet 定义**
   ```go
   // Before
   type FactSet map[string]FactID

   // After
   type FactSet map[[32]byte]FactID
   ```

3. **更新 Add() 方法**
   ```go
   // Before
   hash, err := fact.Hash()
   // ...
   s[hash] = fact

   // After
   hash, err := fact.HashBytes()
   // ...
   s[hash] = fact
   ```

4. **更新 Values() 方法**
   - 从 `map[string]FactID` 遍历改为 `map[[32]byte]FactID`
   - 排序逻辑需要适配 `[32]byte` 比较（`bytes.Compare`）

5. **逐文件修改调用方**
   - 按依赖顺序：exposure → gateway → evaluation
   - 每个文件编译通过后进入下一个

6. **确保序列化兼容性**
   - JSON 输出仍用 hex string（ledger/schema 层不变）
   - 只改内存表示

### 验收

```bash
# 1. 差分测试必须继续通过
go test -v -run 'FactSet.*Differential' ./internal/exposure

# 2. 全量门禁
gofmt -l $(git ls-files '*.go')
go build ./...
go vet ./...
go test -count=1 ./...

# 3. 确认收益
# 创建临时 benchmark
go test -bench=BenchmarkFactSetMemory -benchmem ./internal/exposure
```

**期望**：
- 差分测试证明 digest 恒等
- 每约 Fact 节省 ~80 B（从 64 字符 string → 32 字节）
- 全量测试无回归

### 产出
- FactSet 定义改为 `map[[32]byte]FactID`
- `HashBytes()` 新 API
- 本阶段提交信息末行注明 `P5-mem-1`

---

## 阶段 2：FactSet value 落盘（待启动）

### ID
`P5-mem-2`

### 前置
- `P5-mem-0` 完成
- `P5-mem-1` 完成
- 差分测试继续通过

### 目标
参照 `internal/gateway/encrypted_spool.go` 模式，实现 FactSet 的自适应落盘。

**阈值以下：纯内存**
**阈值以上：AES-GCM 分块落盘，独立认证目录，临时密钥不持久化**

### 设计约束（红线 6）

1. **必须是替换，不是并存**
   - 不允许"小内存、大磁盘"两条路径
   - 统一一个自适应结构，内存态只是阈值以下形态

2. **必须复用 encrypted_spool 模式**
   - 阈值以下永不落盘
   - 越过阈值才把明文搬进独立认证的 AES-GCM 分块
   - 放进 mode-0700 私有目录
   - 临时密钥永不持久化
   - 内存明文随即清空

3. **必须证出 digest 恒等**
   - 旧实现保留作为 oracle
   - 逐字节比对
   - 证不出恒等，这个改动就下桌

### 步骤（草案）

1. **读 encrypted_spool.go 完整实现**
   - 理解阈值逻辑
   - 理解 AEAD 分块格式
   - 理解 AAD 绑定设计

2. **设计 FactSet 的自适应结构**
   - 不能是简单的 map（需要携带落盘状态）
   - 可能需要改名为 `factSet`（小写）+ 结构体
   - 或者保留公开 `FactSet` 为接口

3. **实现落盘后备**
   - Write 时检查阈值
   - 越阈值后启动磁盘模式
   - 读回时解密

4. **更新所有调用方**
   - 98 处非测试引用
   - 确保兼容性

5. **差分测试验证**
   - 阶段 0 的 oracle 必须继续通过
   - 所有输入逐字节一致

### 验收（草案）

```bash
# 1. 差分测试必须通过
go test -v -run 'FactSet.*Differential' ./internal/exposure

# 2. 压力测试（验证落盘触发）
go test -v -run 'FactSet.*Spillover' ./internal/exposure

# 3. 全量门禁
gofmt -l $(git ls-files '*.go')
go build ./...
go vet ./...
go test -count=1 ./...

# 4. 内存测试
# 验证常驻集确实降到 1/5–1/7
```

**期望**：
- 差分测试证明 digest 恒等
- 大 FactSet 常驻内存显著降低
- 写盘/读回开销在可接受范围

### 产出
- 自适应 FactSet 实现
- 阶段 2 提交信息末行注明 `P5-mem-2`

---

## 风险与应对

| 风险 | 影响 | 应对 |
|---|---|---|
| 阶段 1 恒等证明失败 | 下桌 | 60 万 hex key 不是主因，放弃此路 |
| 阶段 2 恒等证明失败 | 下桌 | 在小改动（阶段 1）上发现，不是在 98 处改完后 |
| 调用方兼容性问题 | 阻塞 | 逐文件验证，编译通过才继续 |
| 落盘性能不可接受 | 下桌 | 在阶段 2 验收时暴露，直接放弃 |

---

## 后续

阶段 0+1 完成后：
1. 重生成岔口二的 A 案 evidence（`evaluation/exposure/` 直接用具体 `exposure.FactSet`）
2. v1.8 冻结（含 593ce83 + 阶段 0+1）
3. 重认证 + P5.1 六格重跑 → 第一组真实峰值
4. 阶段 2 另起一轮（需要再冻结吗？作者裁决）

---

## 附：593ce83 差分测试关键点

```go
// 旧实现保留作为 oracle
func bufferedReleaseOutcomeDigest(release []FactID, visibleRows int64) (string, error) {
    // ... 旧实现 ...
}

// 新实现必须逐字节一致
func TestReleaseOutcomeDigestMatchesTheBufferedImplementationByteForByte(t *testing.T) {
    for _, testCase := range cases {
        want, _ := bufferedReleaseOutcomeDigest(release, rows)
        got, _ := ReleaseOutcomeDigest(release, rows)
        if got != want {
            t.Fatalf("digest differs\n  buffered  %s\n  streaming %s", want, got)
        }
    }
}
```

**核心**：
1. Oracle 名为 `buffered*`，明确是旧实现
2. 新旧实现同一输入必须同一输出
3. 负控制证明 oracle 能检测差异
4. 随机化测试覆盖大量形状
