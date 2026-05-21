# Bug Report: splice — sess-D 在 fresh sandbox 上首次调用 `npm test` 时被误注入 sess-A 的 orphan 上下文

## 项目仓库
`/path/to/splice`（Go 1.25 + modernc.org/sqlite，single binary CLI）

## 涉及文件
- `cmd/splice/main.go`：`runPreToolUse()` 函数，约 70-153 行
- `internal/store/store.go`：`FindLatestByHash()`，约 101-128 行
- `internal/fingerprint/fingerprint.go`：`Compute()`，整文件

## 现象

执行如下 8 步 e2e 序列（在 `/tmp/splice-e2e` 沙盒里，每步通过 stdin pipe JSON 给 `splice pre-tool-use` / `splice post-tool-use`），其中 T7 出现**异常输出**：

```
T1: PreToolUse, session_id=sess-A, command="npm test"          → ALLOW（预期）
T2: PostToolUse, session_id=sess-A, command="npm test", exit=0
T3: PreToolUse, session_id=sess-A, command="npm test"          → DENY readonly + 注入"上次 exit 0, 12 passed"（预期）
T4: PreToolUse, session_id=sess-B, command="git push origin main"  → 误注入 orphan "之前启动过 Bash（22 秒前）但未拿到结果\n调用：git push origin main"
T5: PreToolUse, session_id=sess-B, command="git push origin main"  → DENY side-effect（预期）
T6: PreToolUse, session_id=sess-C, command="go test ./..."     → 误注入 orphan "之前启动过 Bash（23 秒前）"
T7: PreToolUse, session_id=sess-D, command="npm test"          → 误注入 orphan "之前启动过 Bash（23 秒前）但未拿到结果\n调用：npm test"
```

预期：**T4、T6、T7 都是各自 session 的第一次调用**，按 `WHERE session_id = ? AND args_hash = ?` 应当查不到任何匹配，应当返回纯 ALLOW（无 `additionalContext`）。

实际：T4/T6/T7 都返回了 ALLOW + orphan 注入，且注入文本里的"X 秒前"指向 sess-A T1 的时间戳。

## 已经验证的事实

1. **代码顺序正确**：`main.go:96` 的 `FindLatestByHash` 调用在 `main.go:101` 的 `now := time.Now()` 和后续 `RecordCall`（已被我之后改成 willDeny 分支）之**前**。所以查询时这个 session 的本次调用还没写入 DB。

2. **SQL 语句没有显式 bug**：
   ```sql
   SELECT id, session_id, tool_name, args_json, args_hash, COALESCE(cwd,''), started_at
   FROM tool_calls
   WHERE session_id = ? AND args_hash = ?
   ORDER BY started_at DESC LIMIT 1
   ```
   两个 `?` 都是参数化绑定，传入的是 `(in.SessionID, hash)`。

3. **fresh sandbox 简化复现 = 行为正常**：
   ```
   rm -rf .splice
   echo '{"session_id":"sess-A",...,"command":"npm test"}' | splice pre-tool-use   # ALLOW
   echo '{"session_id":"sess-D",...,"command":"npm test"}' | splice pre-tool-use   # ALLOW（无 orphan 注入，符合预期）
   ```
   只要彻底清空沙盒，sess-D 第一次调用就**不会**命中 orphan。

4. **bug 触发场景的 DB dump**（出 bug 的那次执行结束后）：
   ```
   tool_calls:
   ('call_940c3d66067ad25c', 'sess-A', 'Bash', 'fc6c8e3f...', '2026-05-19T09:32:15.5047211Z')
   ('call_4484268ad2a34029', 'sess-A', 'Bash', 'fc6c8e3f...', '2026-05-19T09:32:15.6957267Z')
   ('call_e604a8d7af3da0d2', 'sess-B', 'Bash', '2508e913...', '2026-05-19T09:32:15.7952379Z')
   ('call_d05fd0ea42b1bf91', 'sess-B', 'Bash', '2508e913...', '2026-05-19T09:32:15.9804992Z')
   ('call_82b6803be5b15b2e', 'sess-C', 'Bash', 'a35fc206...', '2026-05-19T09:32:16.0603712Z')
   ('call_d0138109b4c68bf1', 'sess-C', 'Bash', 'a35fc206...', '2026-05-19T09:32:16.1503671Z')
   ('call_fae3e23525ebcda6', 'sess-D', 'Bash', 'fc6c8e3f...', '2026-05-19T09:32:16.2288065Z')

   tool_results:
   ('call_940c3d66067ad25c', 0, ...)
   ('call_e604a8d7af3da0d2', 0, ...)
   ```
   注意：T7 注入说"23 秒前"，但 sess-D 这一行的时间戳是 `09:32:16.22`，并且 sess-D 在 DB 里只有 1 条 call。如果 `FindLatestByHash(sess-D, fc6c8e3f...)` 真的只查 sess-D 的行，**它要么返回 nil（在 RecordCall 之前查），要么返回这条自己的 call（在 RecordCall 之后查）**——后者算出来的 `ago` 应该是接近 0 秒，不是 23 秒。

5. **触发条件的相关性**：每次 T4/T6/T7 都是**fresh session_id 的第一次调用**，但它们落到的都是**前面已有 session 用过的 args_hash**（T4 = git push 前面没用过；T6 = go test 前面没用过；T7 = npm test 前面 sess-A 用过）。**T4/T6 也异常**，意味着不只是"sess-A 数据残留" → 这条线索更宽。

6. **23 秒 ≈ 测试运行的总耗时**：T1-T7 串行跑下来的总时间。23 秒前 = 沙盒最早一条记录的时间。但 sess-A 最早的 call 时间是 `09:32:15.50`，T7 执行时间在 `09:32:38` 左右——刚好 23 秒。**所以注入文本里的"23 秒前"对应的可能是某条记录的 started_at，但不一定是 sess-A 的某条**。

## 已经做了什么"修复"

我改了 `runPreToolUse` 的逻辑：**只在决定 ALLOW 时才 RecordCall**，DENY 时不写。这避免了"被 deny 的调用留下 phantom orphan 污染未来同 session 同 hash 的命中"。

修复后：fresh sandbox 跑一遍 8 步 e2e 全绿。

**但**：这个修复**没有定位**"为什么 sess-D 这个全新 session 的第一次调用会查到不属于它的行"这个根因。修复让症状消失，是**因为它消除了"deny 写 phantom call"这个污染源**——可能正好就是这个 bug 的真正成因，也可能不是。

## 我有的几个假设（**未验证**）

1. **DB 残留假设**：之前某次实验（我跑过 `/tmp/splice-fresh` 的 sess-X、sess-Y 实验，但不记得有没有用过 sess-D；也可能某次中间状态我没看到）在 sess-D 这个 session_id 下留过同 hash 的 phantom call。这是最简单的解释，但**无法解释 T4 sess-B 的 git push 也异常**——除非那次实验也用过 sess-B。

2. **WHERE clause 在某种场景下被跳过**：modernc.org/sqlite 的纯 Go 实现是否有边界 case？可能性低但没排除。

3. **scan 时把别的 row 拼接进来**：`row.Scan(&c.ID, &c.SessionID, &c.ToolName, &c.ArgsJSON, &c.ArgsHash, &c.Cwd, &startedAt)` 共 7 列，SELECT 也是 7 列，看起来没问题。但如果 schema 演化有列 drift，可能出错。当前 schema 是 fresh 的，应该不会。

4. **注入文本里的 ago 计算用错了起点**：`ago := now.Sub(matched.Call.StartedAt)`。如果 `matched.Call.StartedAt` 解析时区出错或返回了 epoch，`ago` 会变成不合理的大数。但 T7 实际算出"23 秒"是合理数值，所以这条不太成立。

## 修 bug 时建议的步骤

1. **重现脏 DB 状态**：去 `/tmp/splice-e2e/.splice/splice.db.bak`（如果还在；如果不在，照下面手工构造）：
   ```bash
   cd /tmp/splice-e2e
   rm -rf .splice
   # 跑前 6 步（不清沙盒）
   ...直到 T6
   # 这时拷一份 DB 备份
   cp .splice/splice.db /tmp/dirty-db-snapshot.db
   # 然后只跑 T7，看 sess-D 是否命中
   echo '{"session_id":"sess-D",...,"command":"npm test"}' | splice pre-tool-use
   ```

2. **用 sqlite3 CLI 直接验证查询**：
   ```sql
   SELECT id, session_id, args_hash, started_at FROM tool_calls
   WHERE session_id = 'sess-D' AND args_hash = 'fc6c8e3f54b4c5ff86d98be82c9758216b3583f7542a0e93730947fff7a6819b'
   ORDER BY started_at DESC LIMIT 1;
   ```
   如果**返回行**：那就是 DB 里真有 sess-D 的脏数据（验证假设 1）。**返回什么具体行**很关键，能定位污染源是哪个调用流程写的。

   如果**不返回行**：那就是 Go 层的 SQL 调用 / scan 有 bug，需要在 `FindLatestByHash` 里加 `log.Printf("query session=%q hash=%q", sessionID, argsHash)` 和 `log.Printf("scanned: %+v", c)`，对比传入参数和扫描结果。

3. **加防御性日志**：
   ```go
   func (s *Store) FindLatestByHash(sessionID, argsHash string) (*MatchedCall, error) {
       defer func() {
           // 关键：scan 完后打印 c.SessionID，对比传入 sessionID
       }()
       // ... 原有逻辑
       if c.SessionID != sessionID {
           return nil, fmt.Errorf("INVARIANT VIOLATED: queried for %q got row for %q", sessionID, c.SessionID)
       }
   }
   ```
   这个 invariant check 应该作为永久代码留下来——它廉价、能立即把"WHERE 没生效"这类深层 bug 暴露出来。

4. **确认 modernc.org/sqlite 的版本**：`go.mod` 锁定的是 `v1.50.1`。查 issue tracker 看是否有相关 query bug 报告。

5. **加单元测试**：在 `internal/store/store_test.go` 新增一个用例：先在 sess-A 写一条 hash=H 的 call（无 result），再用 sess-B 查同 hash，必须返回 nil。这个跨 session 隔离的测试**当前已经有了**（`TestSessionIsolation`），但只测了一个简单 case。补一个"多 session 多 call 多种 hash 混合"的测试。

## 修 bug 时**不应该做**的事

- 不要直接通过"清空 DB 再跑就绿了"或者"我已经改了 deny 不写 call 所以不会再出现"来收尾。**根因没定位前不能算修好。**
- 不要删掉那个"deny 不写 call"的修复——它本身是好的设计。但要在它**之外**找到真正的根因。
- 不要假设是 modernc.org/sqlite 的锅然后立刻换 driver。先验证假设 1。

## 验收标准

- 用步骤 1 的脏 DB 重现 + 单跑 T7 → 沙盒里能稳定复现"sess-D 误命中"。
- 步骤 2 的 sqlite3 CLI 直接查询 → 给出**确定的**返回（有行 / 无行），从而判断锅在 SQL 层还是 Go 层。
- 写一个最小测试 reproduce 这个 bug（红 → 修复 → 绿）。
- invariant check 留下，作为永久护栏。
