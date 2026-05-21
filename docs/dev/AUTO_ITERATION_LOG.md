# v0.4.2 自动迭代轮次

时间：2026-05-20 一夜（从 v0.4.1 → v0.4.2）

完成的 todo（按出现顺序）：

## 修复审查报告剩余项

### ✅ A2/A3/A6（这次会话开始时完成）
- A2 消除 fingerprint 双实现：`codex.FingerprintToolCall` 内部委托 `fingerprint.Compute`
- A3 Codex hook 路径合并：`runCodexPreToolUse` 删死代码、改用 `fingerprint.Compute`；`runCodexPostToolUse` 独立实现并用同一 fingerprint
- A6 fence 缩窄：`isFenceTool`（Edit/Write/NotebookEdit）+ Bash 走 `classify.ClassifyBash`，read-only 不算 fence

### ✅ D2 长任务 in-flight（这次会话开始时完成）
- live_trail 加 status='running' + call_id 列
- BeginLiveTrailEntry / FinishLiveTrailEntry / IsErrNoRunningRow
- LookupCachedHit 返回 HitResult{Entry, InFlight, AfterEvents}
- inject.FormatInFlight 两分支（empty after / 带 causal chain）

### ✅ A4 pendingPerCall 挪进 Watcher struct
原全局 map → `Watcher.pending` 字段。多 watcher 实例不再共享状态。

### ✅ B1 trail 24h 老化
- 加 `store.MaxTrailAge = 24*time.Hour` 包级变量（测试可置 0 关闭）
- LookupCachedHit 看到 `time.Since(recorded_at) > MaxTrailAge` → 返回 nil（miss）
- FreezePreCompact 同时清理超过 MaxTrailAge 的旧 snapshot
- 测试：TestLookupCachedHitAgesOut

### ✅ B2 watcher 跨平台文件锁
- `Watcher.Run` 启动时 acquireLock：写 `<cwd>/.splice/codex-watch.lock` 含 PID
- 检测已存在锁 → 用 `isProcessAlive(pid)` 判断是否真活着，活着 → ErrLocked，死了 → 当 stale 重新声明
- `process_unix.go`：signal 0 探测
- `process_windows.go`：OpenProcess + GetExitCodeProcess（STILL_ACTIVE = 259）
- 退出时 release 删锁文件
- 测试：TestWatcherSecondInstanceLocked + TestWatcherStaleLockReclaimed

### ✅ B3 多 snapshot scoping
- FreezePreCompact 不再 DELETE 旧 snapshot（之前每次压缩只留最近一份）
- 改成"按 MaxTrailAge 滚动清理"
- LookupCachedHit ORDER BY snapshot_at DESC, seq DESC：返回最近匹配 snapshot
- fence 检查 + eventsAfterInSnapshot 都按 snapshot_id 严格隔离
- 测试：TestMultipleSnapshotsSurviveAndScopeCorrectly

### ✅ B4 graceful-fail buffered stdout
- `runPreToolUseWith(stdout io.Writer)` / `runCodexPreToolUseWith(stdout io.Writer)`
- main 在 buffered bytes.Buffer 上跑业务逻辑，成功才 io.Copy 到 os.Stdout
- 防止部分写 + emitAllow 双 JSON 污染 hook 协议

### ✅ B5 bypass 字段容错多个名字
- PreToolUseInput 加 ApprovalMode / ApprovalPolicy / Policy
- IsBypassMode 识别：bypassPermissions / never / bypass / bypass_permissions
- 测试：TestIsBypassMode 11 子用例 + TestExtractStatus 11 子用例

## 跳过的项

### ⏭ A5 replay 跳过到最后 compaction
原方案"用 FindLastCompactionOffset 跳过"会让 freeze 不再触发（因为它跳过了 compaction event 本身）。需要重新设计成"找到最后 compaction → freeze 一次 → 从那以后增量 tail"，是更大改动。当前 watcher 启动 replay 几百 ms 在 < 10MB 文件可接受，留待 v0.5。

### ⏭ A1 tool_calls 写入策略
v0.2 遗留表，对决策正确性零影响。增长率受限（小项目 < 5MB/天）。决定不修，作为已知技术债。

## 测试覆盖补完

新增三个之前完全没测试的包：

### config 包（6 用例）
- TestLoadDefaults
- TestLoadProjectLocalOverridesGlobal
- TestLoadMalformedJSONReturnsError
- TestLoadEmptyFileIsValid
- TestLoadMissingFileIsValid
- TestLoadEmptyCwdReturnsDefaults

### hook 包（22 用例）
- TestIsBypassMode 11 子用例（多种字段名 + 否定 case）
- TestExtractStatus 11 子用例（interrupted / timeout / is_error / status string / exit code 等）

### inject 包（7 用例 + 子用例）
- TestFormatDenyHitContainsBasics
- TestFormatAskReasonOmitsExitWhenNil
- TestFormatInFlightEmptyAfter
- TestFormatInFlightWithCausalChain
- TestFormatInFlightTruncatesAfterTwelve
- TestHumanDurationBuckets（4 子用例）
- TestTruncateLong

## 当前状态

- **10 个包全部有测试**（之前 8 个）
- **总用例 ~80+**（含所有子用例）
- **0 失败**
- 编译跨 Windows / Unix（process_*.go 拆分）

## 跑过验证

`go test -count=1 ./...` 三次重跑全绿，包括：
- cmd/splice
- internal/classify
- internal/codex（含 watcher 锁、fingerprint 一致性）
- internal/codexinstall
- internal/config（新）
- internal/fingerprint
- internal/hook（新）
- internal/inject（新）
- internal/install
- internal/store

## 文件改动

- internal/store/store.go：MaxTrailAge / LookupCachedHit 重构 / multi-snapshot
- internal/store/store_test.go：+TestMultipleSnapshotsSurviveAndScopeCorrectly + nullInt helper
- internal/codex/watcher.go：Watcher.pending / acquireLock / isProcessAlive 占位
- internal/codex/process_unix.go（新）
- internal/codex/process_windows.go（新）
- internal/codex/watcher_test.go：+TestWatcherSecondInstanceLocked + TestWatcherStaleLockReclaimed
- internal/hook/hook.go：扩 PreToolUseInput + IsBypassMode 重写
- internal/hook/hook_test.go（新）
- internal/config/config_test.go（新）
- internal/inject/inject_test.go（新）
- cmd/splice/main.go：buffered stdout / runPreToolUseWith / runCodexPreToolUseWith

## 下一步建议

我没继续往下做的原因——边界回报递减。剩余有意义的方向：

1. 真机验证：装到真实 Claude Code，跑实际项目
2. A5 重新设计：watcher 优化（< 10MB 文件下不是必须）
3. 监测真实 bypass 字段名：实测 Claude Code/Codex 当前版本的 stdin schema
4. A1 表清理：tool_calls 加 TTL（如果真实使用证明它在膨胀）

当前 v0.4.2 已经满足你三天前提的初衷的核心需求 + 边界 + 异常路径，且全部有测试覆盖。

---

# v0.4.3 一轮：A1 表彻底删除

时间：2026-05-20。

## 背景

v0.4.2 把 A1 处理成「加 TTL 让 tool_calls / tool_results 不至于膨胀」。但你后来明确：A1 的正确处理是**彻底删表**——这两张表已经不参与任何决策，仅在 PostToolUse sidecar 缺失时作为 callID 找回路径，而那个找回路径其实可以直接用 `AppendLiveTrail` 顶替。

## 改动

### schema
- 移除 `tool_calls` / `tool_results` 表 + 对应索引
- `Open()` 启动时 `DROP TABLE IF EXISTS` 这两张表，给 v0.4.2 老 DB 一次性迁移
- `trail_schema_version` 1 → 2，`INSERT OR IGNORE` 改成 `INSERT OR REPLACE` 以反映迁移

### store API 删除
- 类型：`ToolCall` / `ToolResult` / `MatchedCall`
- 方法：`RecordCall` / `RecordResult` / `FindLatestByHash` / `findResult`
- v0.4.2 临时加上的 sweep 基础设施：`SweepLegacyTables` / `maybeSweepLegacy` / `LegacyTableTTL` / `LegacySweepEvery` / `Store.sweepMu` / `Store.recordCallCount`
- v0.4.2 加的 invariant 检查（已无意义，因为 FindLatestByHash 没有了）

### hook 主路径 (`cmd/splice/main.go`)
- `runPreToolUseWith` / `runCodexPreToolUseWith` 不再 `RecordCall`，只做 `BeginLiveTrailEntry` + sidecar
- `runPostToolUse` / `runCodexPostToolUse` 重写：sidecar 命中 → `FinishLiveTrailEntry`；sidecar 缺失或 running 行已被冻 → `AppendLiveTrail`
- 不再存在 `RecordCall + RecordResult` 兜底路径，因为 `live_trail` 自身就是合法的 trail 信号

### 测试
- `internal/store/store_test.go` 整体重写：删除所有针对 `tool_calls` / `tool_results` 的测试（`TestRecordCallAndFindLatest` / `TestSessionIsolation` / `TestSessionIsolationMixedCallsAndHashes` / `TestRecordResultAttaches` / `TestOrphanIsLatestCallWithoutResult` / `TestSweepLegacyTablesRespectsTTL` / `TestRecordCallTriggersSweepEveryN` / `TestSweepLegacyDisabledByZero`）
- 新增 trail 相关测试：`TestBeginFinishLiveTrailEntry` / `TestFinishWithoutBeginReturnsSentinel` / `TestAppendLiveTrailSurvivesFreeze` / `TestClearSessionWipesEverything` / `TestCooldownIsolation` / `TestLegacyTablesDropped`
- 保留 `TestMultipleSnapshotsSurviveAndScopeCorrectly` / `TestLookupCachedHitAgesOut`
- `cmd/splice/main_test.go` 删 `countToolCalls` 占位 helper 与 `database/sql` import

### 文档
- `docs/CLAUDE_INTEGRATION.md`：splice.db 注释去掉 `tool_calls/results`

## 跑过验证

`go test ./...` 全 10 包绿，无失败。

## 副作用 / 设计影响

- **PostToolUse sidecar 丢失情形更安全**：以前会留下"无 result 的 phantom call"污染 `tool_calls`；现在直接 `AppendLiveTrail` 一条终态行，干净。
- **DB 体积下降**：长会话不再无限增长这两张表，无需 TTL 清理逻辑。
- **决策正确性**：完全无变化——决策本来就走 `live_trail` / `pre_compact_trail` / `cooldown`。

## 跳过的项继续跟进

⏭ A5（watcher replay 跳过到最后 compaction）：与你讨论后认定"优化"会丢 splice 在 Codex 上真正想要的多次压缩历史记录，留待重新设计或不做。

---

# v0.5.0：per-session 文件 + 单 snapshot + N-no-hit 删除 + live_trail fence

时间：2026-05-20。

## 设计转变

v0.4.x 把所有 session 的数据塞在 `<cwd>/.splice/splice.db` 一个文件里，靠 `session_id` 列做隔离；多次压缩可以共存多个 snapshot；用 24h TTL 老化。

讨论后发现这套机制是在补"长会话跨多次压缩还能命中 cache"这个**伪问题**——真实用户每次压缩之间事情主题已经变了，模型重跑的目标永远是"最近一次"。复杂度全是浪费。

新模型：

- **per-session 一个 SQLite 文件**：`<cwd>/.splice/sessions/<sid>.db`
- **任意时刻最多一份 snapshot**：每次 freeze 整体替换
- **N-no-hit 删除**：压缩后连续 N 次 PreToolUse 没命中 → 删 snapshot；命中归零
- **live_trail 也参与 fence 检查**：用户 99% 在两次压缩之间会改文件，新逻辑覆盖了这个常见场景
- **/clear 直接删整个 session 文件**

## schema 瘦身

`internal/store/store.go` 从 ~750 行降到 ~600 行。删除：

- `pre_compact_trail.session_id` / `snapshot_id` / `snapshot_at` 列 + 索引
- `live_trail.session_id` 列
- `cooldown.session_id` 列
- `MaxTrailAge` / 24h 老化
- 多 snapshot scoping 全部相关代码

新增：

- `meta` 表：eviction_counter / rollout_last_offset
- `BumpEviction` / `ResetEviction` / `DropSnapshot`
- `LastRolloutOffset` / `SetLastRolloutOffset`：watcher 续读所需
- `OpenSession(cwd, sessionID)` / `Drop()` / `DropSessionFiles(path)` / `SessionDBPath` / `SessionsDir`

## fence 规则升级

旧：snapshot 内 P 之后扫 fence。  
新：snapshot 内 P 之后 ∪ **整个 live_trail** 都扫 fence。任意一边出现 Edit/Write/NotebookEdit / 侧效 Bash → miss。

`internal/store/store.go::LookupCachedHit` 现在分两步调用 `snapshotFencedAfter` + `liveTrailFenced`，任一为真即放行。

## N-no-hit 删除机制

`config.SnapshotEvictionAfter`（默认 20，0 = 关闭）。`maybeEvictSnapshot` 在 PreToolUse 没命中时 BumpEviction，达到阈值即 DropSnapshot；命中走 ResetEviction。每次 freeze 也重置计数器（新的 post-compact 窗口从 0 开始）。

## watcher per-session 重构

`Watcher` 不再持全局 store handle。每个 session tail goroutine 自己 `OpenSession(cwd, sid)`，启动时 `LastRolloutOffset()` 续读，每批事件后 `SetLastRolloutOffset(...)`。watcher 重启不再双写——这其实顺带解决了一个 v0.4.x 的隐性 bug（重启后 live_trail 行重复）。

## 测试覆盖

`internal/store/store_test.go` 重写：
- `TestSnapshotReplacedOnEachFreeze`：单 snapshot 替换语义
- `TestLiveTrailFenceInvalidatesHit`：live_trail fence 关键场景
- `TestBeginFinishLiveTrailEntry` / `TestFinishWithoutBeginReturnsSentinel` / `TestAppendLiveTrailSurvivesFreeze`：trail 生命周期
- `TestDropClosesAndDeletesFiles`：/clear → Drop
- `TestCooldownIsScopedToSession`：per-session 隔离
- `TestEvictionCounterTrips` / `TestFreezeResetsEvictionCounter`：N-no-hit 机制
- `TestRolloutOffsetPersists`：watcher offset 持久化
- `TestSessionsAreIsolatedByFile`：不同 session 真的不同文件

`internal/codex/watcher_test.go`：所有断言改成在测试里独立 `OpenSession(cwd, sid)` 而不是访问 watcher 内部 store handle。

## 其他变更

- `pendingDir` / `writePendingID` / `readPendingID` / `clearPendingID` 全部加 `sessionID` 参数 → `<cwd>/.splice/sessions/<sid>.pending/<hash>`
- `runSessionStart` 的 `/clear` 分支改用 `store.DropSessionFiles` 而不是 `ClearSession`
- `runPreCompact` 用 `FreezePreCompact()`（无参）替代旧的 `(sessionID, snapshotID, time.Time)`
- README / CLAUDE_INTEGRATION.md 同步更新
- 删 `internal/store/fs.go`（`ensureDir` 已无人用）

## 验证

`go test ./...` 全 10 包 0 失败。

## 已知边界

- **跨 cwd resume 视为新会话**：splice 是 cwd 粒度的（与 Claude Code 自身存储模型对齐）。Codex 用户从其他目录 `codex resume <id>` 时 splice 找不到旧 sid 的文件，当全新会话处理。这是 Claude Code 唯一支持的 resume 路径，对 Codex 99% 用法也契合。
- **session 文件不主动 GC**：暂未实现"rollout 不在了 → 删 splice 文件"。手动用 `rm -rf <cwd>/.splice/sessions/<sid>.*` 即可。如果用户长期使用 splice，未来需要补一个被动 GC（启动时扫一次）。


