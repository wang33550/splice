# Debug Report: splice phantom orphan 误注入修复结论

## 背景

原始问题见 `BUG_REPORT.md`：在脏 DB 场景下，新的/看似首次使用的 session 执行 `npm test`、`go test ./...`、`git push origin main` 等命令时，`pre-tool-use` 可能错误注入 orphan context：

```text
[splice] 之前启动过 Bash（X 秒前）但未拿到结果
```

报告要求确认：这是 SQL/Go 查询层跨 session 误命中，还是 DB 中真实存在脏数据。

## 复现与验证

### 1. 脏库复现

本机没有保留 `/tmp/splice-e2e/.splice/splice.db.bak`，所以使用仓库里的旧 `splice.exe` 手工构造了等价脏库：

1. `sess-D` 首次 `npm test`：允许并记录 call。
2. `sess-D` `post-tool-use`：记录 exit 0 和 `12 passed`。
3. `sess-D` 第二次 `npm test`：旧逻辑会 deny，但仍写入新的 `tool_calls`。
4. 因为 deny 后工具不会真实执行，所以不会有对应 `tool_results`，形成 phantom orphan。
5. 再次单跑 `sess-D / npm test`：稳定复现 orphan 注入。

复现输出中的关键异常：

```json
{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "additionalContext": "[splice] 之前启动过 Bash（0 秒前）但未拿到结果\n调用：npm test\n可能由于压缩、宿主重启或后台任务未完成。本次会真正执行。"
  }
}
```

### 2. 直接 SQL 查询

对复现出的脏库执行等价查询：

```sql
SELECT id, session_id, args_hash, started_at
FROM tool_calls
WHERE session_id = 'sess-D'
  AND args_hash = 'fc6c8e3f54b4c5ff86d98be82c9758216b3583f7542a0e93730947fff7a6819b'
ORDER BY started_at DESC
LIMIT 1;
```

查询返回真实行，例如：

```text
('call_4670f460003da2ec', 'sess-D', 'fc6c8e3f54b4c5ff86d98be82c9758216b3583f7542a0e93730947fff7a6819b', '2026-05-19T10:37:21.5686721Z')
```

结论：SQL 层没有跳过 `WHERE session_id = ? AND args_hash = ?`；Go `Scan` 也没有把其他 row 拼进来。误注入来自 DB 中真实存在的同 session/hash phantom call。

## 根因

根因是旧的 `runPreToolUse()` 状态机不完整：

- `PreToolUse` 在发现已有结果后，会对 read-only 命令返回 `deny`，并把上次输出注入 context。
- 但旧逻辑即使决定了 `deny`，仍然调用 `RecordCall()` 写入 `tool_calls`。
- 被 deny 的工具不会真正运行，因此不会触发对应 `PostToolUse`，也不会写入 `tool_results`。
- 下一次 `FindLatestByHash()` 按 `started_at DESC` 取到的就是这条最新但无 result 的 phantom call，于是被解释为 orphan。

所以本问题不是 SQLite driver bug，也不是跨 session 查询泄漏；它是“被拒绝执行的调用被当成已启动调用记录”的业务状态错误。

## 修复内容

### 1. 保持正确状态机

`cmd/splice/main.go` 已采用正确行为：只有在工具会真实执行时才记录 in-flight call。

```go
if !willDeny {
    callID := newID()
    st.RecordCall(...)
    writePendingID(...)
}
```

该逻辑保证：

- `ALLOW`：写 `tool_calls`，等待 `PostToolUse` 补 `tool_results`。
- `DENY`：不写 `tool_calls`，不写 pending id。
- 真实崩溃/压缩导致的缺失 result：仍然能被识别为 orphan。
- 被 deny 的缓存命中/副作用重复：不会污染 orphan 判断。

### 2. 增加永久 invariant 护栏

在 `internal/store/store.go` 的 `FindLatestByHash()` 中加入结果校验：

```go
if c.SessionID != sessionID || c.ArgsHash != argsHash {
    return nil, fmt.Errorf("store: invariant violation: ...")
}
```

这个检查不改变正常路径，只在数据库查询层真的出现不符合 WHERE 条件的结果时快速失败，避免静默注入错误上下文。

### 3. 增加回归测试

新增测试：

- `cmd/splice/main_test.go` / `TestDeniedPreToolUseDoesNotCreatePhantom`
  - 覆盖完整 CLI 流程：首次 allow、post 记录结果、第二次 deny、第三次仍应 deny cached result。
  - 断言 deny 不留下 pending id。
  - 断言 deny 不新增 `tool_calls`。
  - 断言后续不会出现 orphan context。

- `internal/store/store_test.go` / `TestSessionIsolationMixedCallsAndHashes`
  - 覆盖多 session、多 hash 混合数据。
  - 断言 fresh session 查询已有 hash 不会跨 session 命中。
  - 断言同 session 不同 hash 不会误命中。

## 对正常功能的影响

该修复不是针对现象的暴力规避，而是补全状态机语义。正常功能影响如下：

- 首次执行命令：不受影响，仍会记录 call。
- `PostToolUse` 结果挂载：不受影响，仍通过 pending id 或 fallback 匹配。
- read-only 重复命中：不受影响，仍 deny 并注入上次结果。
- side-effect 重复命中：不受影响，仍 deny 并提示需要显式确认。
- 真 orphan 检测：不受影响，只要 call 是实际允许执行后缺少 result，仍会被识别。
- 审计语义：被 deny 的“尝试”不再混入 `tool_calls`。如果未来需要审计 deny 事件，应新增独立 events/decisions 表，而不应复用 `tool_calls`。

## 验证结果

执行：

```powershell
gofmt -w cmd/splice/main_test.go internal/store/store.go internal/store/store_test.go
go test ./...
```

结果：

```text
ok  	github.com/wang33550/splice/cmd/splice
ok  	github.com/wang33550/splice/internal/classify
ok  	github.com/wang33550/splice/internal/fingerprint
?   	github.com/wang33550/splice/internal/hook	[no test files]
?   	github.com/wang33550/splice/internal/inject	[no test files]
ok  	github.com/wang33550/splice/internal/store
```

额外验收脚本确认：

- 旧 `splice.exe` 可以构造脏库并稳定复现 orphan。
- 直接 SQL 查询返回真实同 session/hash phantom 行。
- 当前源码重复 deny 路径后，DB 中只有 1 条真实执行过且有 result 的 `tool_calls`，不会新增 phantom。

## 最终结论

`sess-D` 的 orphan 误注入不是因为 SQL 查询跨 session 漏行，也不是 sqlite driver 问题。根因是旧逻辑在 `PreToolUse` 已经决定 `deny` 后仍写入 `tool_calls`，导致被拒绝执行的调用变成没有结果的最新 call。修复方式是只记录会真实执行的调用，并用 invariant 与回归测试防止该状态机错误复发。
