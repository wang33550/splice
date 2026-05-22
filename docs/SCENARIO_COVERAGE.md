# Scenario Coverage Matrix

This file defines functional coverage for splice. A scenario counts as covered
only when it has an automated test that exercises the runtime behavior, not
just a unit helper.

## Semantic Contract

splice is not a generic command-output cache. Its job is to repair a specific
post-compaction failure mode: the model forgets the most recent causal trail
and starts reasoning from before a just-finished operation.

The runtime contract is:

- If the repeated operation was a stable terminal result and no fence happened,
  intercept and restore the repeated operation plus every later observed tool
  call/result up to the compaction boundary.
- If the repeated operation is a live/external status query, allow it to run
  again; historical live observations may appear only as context after an
  earlier stable hit, explicitly marked as pre-compaction history.
- If any known side-effect happened after the candidate hit, allow the repeat;
  stale restoration is more dangerous than duplicated work.
- Unknown non-Bash host/MCP tools are treated as side-effect fences unless
  explicitly classified as read-only by splice.
- If a task was still running at compaction, notify rather than deny/ask; the
  model must verify whether the old task is still active before relaunching.
- User-configured `never_cache_bash_patterns` can describe custom live/status
  queries, but cannot turn known dangerous commands into non-fences.

## Core Claude Hook Flow

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| C01 | Repeat before any compaction | Allow, no injected context | `TestNoPreCompactNoIntercept` |
| C02 | Successful stable Bash repeated after compaction | Ask by default with prior output and terminal context | `TestPreCompactCacheHitAsks` |
| C03 | Same hit repeated again in same window | Allow via cooldown; next compaction clears cooldown | `TestPreCompactCacheHitAsks`, `TestRuntimeCooldownClearsOnNextCompaction` |
| C04 | `ask_on_intercept=false` | Deny and inject terminal context | `TestConfigAskOff` |
| C05 | Bypass mode where ask is swallowed | Deny and inject terminal context | `TestBypassModeAutoDegradesToDeny` |
| C06 | Failed terminal result | Allow, never cache | `TestFailedResultNotCached` |
| C07 | Interrupted terminal result | Allow, never cache | `TestInterruptedResultNotCached` |
| C08 | Timeout terminal result | Allow, never cache | `TestTimeoutResultNotCached` |
| C09 | Terminal repeat has later read-only/tool observations before compaction | Inject repeated call plus all later pre-compaction tool outputs | `TestTerminalHitInjectsFullPostCallTrail` |
| C10 | Terminal repeat has later failed read-only/tool observation before compaction | Inject later failed result with explicit exit/status marker, not as successful context | `TestTerminalHitTrailPreservesFailedAfterEventStatus` |
| C11 | Intercepted terminal hit is rejected/blocked and no PostToolUse arrives | Next compaction preserves the terminal fact instead of freezing splice's own PreToolUse bookkeeping as in-flight | `TestInterceptedTerminalHitDoesNotBecomeInFlightOnNextCompaction` |

## Core Codex Hook Flow

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| CX01 | Codex `shell` repeated after compaction | `decision.behavior=ask` with prior output and terminal context | `TestCodexPreCompactCacheHitAsks` |
| CX02 | Codex bypass/full-auto mode | `decision.behavior=deny` and inject terminal context | `TestCodexBypassModeAutoDegradesToDeny` |
| CX03 | Codex volatile status query repeated after compaction | Allow rerun, no cached injection | `TestCodexVolatileBashStatusQueryNotCached` |
| CX04 | Codex `shell` label canonicalizes to Bash label | Fence/cache decisions see command text | `TestCodexPreCompactCacheHitAsks` |
| CX05 | Codex terminal repeat has later read-only/status observations | Inject full post-call trail in `additionalContext` | `TestCodexTerminalHitInjectsFullPostCallTrail` |
| CX06 | Codex intercepted terminal hit has no PostToolUse because duplicate execution was rejected/blocked | Next compaction preserves terminal fact instead of stale in-flight | `TestCodexInterceptedTerminalHitDoesNotBecomeInFlightOnNextCompaction` |

## Fence And Cache Safety

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| F01 | Edit/Write after cached call before compaction | Allow repeated old call | `TestFenceLetsRepeatThrough` |
| F02 | Read-only Bash after cached call | Still cache-hit old stable call | `TestReadOnlyBashAfterDoesNotFence` |
| F03 | Side-effect Bash after cached call | Allow repeated old call | `TestSideEffectBashAfterDoesFence` |
| F04 | Mixed read-only and side-effect Bash chain | Side-effect fences old call | `TestMixedBashFenceTriggersOnce` |
| F05 | Compound Bash starts with read-only prefix but mutates later | Fence old call | `TestCompoundBashAfterDoesFence` |
| F06 | Shell redirection, pipe, background, substitution | Classified as side-effect/fence | `TestClassifyBash` |
| F07 | Unknown Bash command | Conservative side-effect/fence | `TestClassifyBash` |
| F08 | Terminal hit followed by real side-effect and then duplicate running same call | Real side-effect still fences; duplicate-running suppression must not bypass it | `TestPriorTerminalBehindRunningStillHonorsRealFence` |
| F09 | Edit/side-effect before candidate terminal call | Earlier fence does not invalidate a later successful candidate result | `TestFenceBeforeCandidateDoesNotInvalidateLaterHit` |
| F10 | Unknown non-Bash host/MCP tool after candidate terminal call | Conservative side-effect/fence; allow repeat and do not inject stale context | `TestUnknownToolAfterCandidateFencesHit`, `TestUnknownToolAfterStableCandidateFencesRuntimeHit` |
| F11 | External read-only host tool after candidate terminal call | Does not fence stable local hit, but appears only as historical post-call context | `TestExternalReadOnlyToolAfterCandidateDoesNotFence` |

## Volatile External State

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| V01 | Live status query repeated after compaction | Allow rerun, no cached injection | `TestVolatileBashStatusQueryNotCached` |
| V02 | Volatile query after stable command | Does not fence stable command | `TestVolatileBashAfterDoesNotFenceStableHit` |
| V03 | Project-configured never-cache Bash pattern | Allow rerun, no cached injection | `TestConfiguredNeverCacheBashPatternNotCached` |
| V04 | Global configured never-cache Bash pattern | Allow rerun, no cached injection | `TestGlobalNeverCacheBashPatternNotCached` |
| V05 | Pattern matching fallback for invalid glob | Contains match still works | `TestNeverCachePatternInvalidGlobFallsBackToContains` |
| V06 | Store-level cacheable callback rejects volatile Bash | Lookup misses despite successful row | `TestLookupCachedHitHonorsBashCacheableCallback` |
| V07 | Configured unknown live/status query after stable command | Does not fence stable hit but is marked historical in terminal context | `TestConfiguredNeverCacheUnknownStatusDoesNotFenceStableHit` |
| V08 | Configured pattern matches known side-effect command | Still fences; config cannot mark dangerous commands safe | `TestConfiguredNeverCacheCannotUnfenceKnownSideEffect` |
| V09 | Project-configured unknown stable command | Can be restored after compaction when `force_cache_bash_patterns` marks it stable | `TestConfiguredForceCacheUnknownStableCommand` |
| V10 | Project-configured command that looks read-only but mutates state | `force_fence_bash_patterns` invalidates earlier hits and prevents restoring the command itself | `TestConfiguredForceFenceOverridesBuiltinReadOnly` |
| V11 | Same command matches both force-cache and force-fence | Force-fence wins; stale reuse is worse than duplicated work | `TestConfiguredForceFenceWinsOverForceCache` |
| V12 | Force-cache matches known dangerous Bash or shell control syntax | Known danger still wins; config cannot turn side effects into cached facts | `TestConfiguredForceCacheCannotOverrideKnownDangerousBash` |

## Fingerprinting

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| FP01 | Map key order differs | Same hash | `TestFingerprintIsDeterministic` |
| FP02 | Leading/trailing Bash whitespace differs | Same hash | `TestFingerprintBashWhitespace` |
| FP03 | Internal Bash whitespace differs | Different hash | `TestFingerprintBashPreservesInternalWhitespace` |
| FP04 | Different tool names | Different hash | `TestFingerprintDistinctTools` |
| FP05 | Empty optional args differ | Same hash | `TestFingerprintIgnoresEmpty` |
| FP06 | Codex `shell` aliases to Claude `Bash` | Same hash | `TestFingerprintShellAliasMatchesBash` |
| FP07 | Codex rollout JSON args match hook args | Same hash | `TestCrossPackageFingerprintConsistency` |
| FP08 | Nested object/array args normalize recursively | Same hash for equivalent structure | `TestFingerprintNestedObjectsAndArrays` |
| FP09 | Codex bad args JSON | Stable `_raw` fallback hash | `TestFingerprintBadArgsUsesRawFallback` |

## In-Flight And Long Tasks

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| I01 | PreToolUse started but no PostToolUse before compaction | Notify only, do not deny/ask | `TestInFlightAloneEmitsNotify` |
| I02 | In-flight task with later completed events | Notify with full causal chain | `TestInFlightWithFollowupEmitsCausalChain` |
| I03 | Started task finishes before compaction | Terminal cache behavior with terminal context | `TestInFlightFinishesNormallyBecomesTerminalHit` |
| I04 | PostToolUse arrives after PreCompact cleared live row and no fence occurred since the original start | Later repeat uses the completed live result instead of stale in-flight snapshot, preserving context both before and after the late result; official `tool_use_id` can recover the frozen running row without relying on hash sidecars | `TestPostToolUseAfterCompactSupersedesInFlightSnapshot`, `TestInFlightSnapshotUsesLiveTerminalResultWhenPostArrivesAfterCompact`, `TestPostToolUseWithToolUseIDAfterCompactSupersedesInFlightSnapshot`, `TestCodexPostToolUseWithToolUseIDAfterCompactSupersedesInFlightSnapshot` |
| I05 | In-flight task later completes but any side-effect happened after its original start | Late result is fenced whether the side-effect was before result, after result, or in the frozen snapshot; repeat is allowed, including official `tool_use_id` recovery path | `TestPostToolUseAfterCompactHonorsFenceBeforeResult`, `TestPostToolUseWithToolUseIDAfterCompactHonorsFenceBeforeResult`, `TestLiveTerminalAfterInFlightSnapshotHonorsFenceBeforeResult`, `TestLiveTerminalAfterInFlightSnapshotStillHonorsFence`, `TestLiveTerminalAfterInFlightSnapshotHonorsSnapshotFenceAfterStart` |
| I06 | Codex watcher sees compaction between rollout tool_call and tool_result | Snapshot preserves running call and emits in-flight notification on repeat | `TestWatcherInFlightAcrossCompactionEmitsRunningHit` |
| I07 | Codex watcher sees tool_result after compaction | Later repeat restores completed result instead of stale in-flight | `TestWatcherPostCompactResultSupersedesInFlightSnapshot`, `TestCodexPreToolUseConsumesWatcherRecoveredLateResult` |
| I08 | Codex watcher restarts between compaction and tool_result | Frozen running call_id lets late result recover a terminal live hit | `TestWatcherRestartCanAttachResultToFrozenRunningCall`, `TestAppendTerminalFromFrozenRunningRecoversAfterRestart` |
| I09 | Codex hook and watcher both record the same call, terminal plus duplicate running | Terminal result wins; duplicate running row must not produce stale in-flight warning | `TestCodexHookAndWatcherDoubleSourcePrefersTerminalResult` |
| I10 | Prior terminal plus duplicate running, then newer live terminal arrives | Newer live terminal wins over older duplicate-source terminal | `TestLiveTerminalResultWinsOverPriorDuplicateTerminal` |
| I11 | Prior terminal plus duplicate running plus real later context | Duplicate running is filtered from restored trail, but real later after-events remain | `TestPriorTerminalBehindDuplicateRunningKeepsRealAfterEvents`, `TestCodexDoubleSourceSkipsDuplicateRunningButKeepsLaterContext` |
| I12 | Real repeated command is running at compaction after an older terminal result | Latest real running call remains in-flight; older terminal result is not reused; multiple same-hash running rows prevent duplicate-source fallback | `TestRealRepeatedRunningAfterPriorTerminalRemainsInFlight`, `TestDuplicateSourceFallbackStopsAtInterveningRunning` |

## Scope, Session, And Lifecycle

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| S01 | Different sessions same cwd | Isolated DBs and cache state | `TestCrossSessionIsolation`, `TestSessionsAreIsolatedByFile`, `TestSameProjectDifferentConversationsAreIsolated` |
| S02 | Cooldown scoped per session | No cross-session bleed | `TestCooldownIsScopedToSession` |
| S03 | Fresh freeze replaces previous snapshot | Only latest snapshot is used | `TestSnapshotReplacedOnEachFreeze` |
| S04 | No-hit eviction counter trips | Snapshot dropped and runtime hook stops injecting old context | `TestEvictionCounterTrips`, `TestRuntimeSnapshotEvictionDropsOldTrailAfterNoHits` |
| S05 | Freeze resets eviction counter and cooldown window | Counter restarts at zero; next compacted repeat can intercept again | `TestFreezeResetsEvictionCounter`, `TestRuntimeCooldownClearsOnNextCompaction` |
| S06 | `/clear` clears session trail and pending state | No stale cache after clear; late hash-only PostToolUse from pre-clear commands cannot resurrect old results; hash-only post-clear completion degrades to in-flight instead of trusting ambiguous output | `TestClearDropsSessionTrail`, `TestClearDropsLatePostToolUseWithoutPending`, `TestClearDropsHashOnlyPostToolUseEvenWithNewPending`, `TestClearTrailStateKeepsDBAndClearsRuntimeRows` |
| S07 | Session marker write is private and valid JSON | Marker visible to watcher only | `TestSessionMarkerAndPendingFilesArePrivate` |
| S08 | `/clear` with official `tool_use_id` pairing | Old pre-clear PostToolUse cannot claim a new same-hash pending row; new post-clear PostToolUse still records normally | `TestClearAllowsNewPostToolUseWithOfficialID`, `TestClearPreventsOldToolUseIDPostFromClaimingNewSameHashPending`, `TestCodexClearPreventsOldToolUseIDPostFromClaimingNewSameHashPending` |
| S09 | Same desktop conversation switches project cwd | State follows `session_id` and is not split by cwd | `TestSessionStorageFollowsSessionAcrossDesktopProjectSwitch` |
| S10 | Codex desktop projectless conversation | Global marker and session metadata are written without cwd | `TestCodexSessionStartUsesGlobalMarkerForProjectlessDesktopChat` |
| S11 | Codex `/clear` while global watcher is alive | Clear writes a rollout offset barrier and watcher restarts that session tail from the post-clear offset | `TestCodexClearWritesRolloutOffsetBarrier`, `TestWatcherRestartsTailWhenClearMarkerAdvancesOffset` |
| S12 | CLI one terminal with one conversation | SessionStart, tool result, compaction, and duplicate recovery all stay inside one session | `TestCliSingleTerminalSingleSessionFlow` |
| S13 | CLI multiple terminals in the same project | Each terminal conversation has its own `session_id`, DB, cooldown, and recovered context | `TestCliMultipleTerminalsSameProjectDifferentSessions`, `TestGlobalWatcherCoversMultipleCliTerminalsInSameProject` |
| S14 | Same desktop conversation repeats the same Bash command after switching projects | Storage still follows `session_id`, but command identity is scoped by project cwd so project A results are not reused in project B | `TestSameSessionDifferentProjectDoesNotReuseScopedBashResult`, `TestWatcherUsesRolloutCwdForHistoricalProjectScope` |

## Store And Persistence

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| P01 | Begin/finish running row | Terminal row is updated | `TestBeginFinishLiveTrailEntry` |
| P02 | Finish without begin | Sentinel error for fallback | `TestFinishWithoutBeginReturnsSentinel` |
| P03 | Append survives freeze | Frozen row is cacheable | `TestAppendLiveTrailSurvivesFreeze` |
| P04 | Drop deletes DB/WAL/SHM/meta | Files removed | `TestDropClosesAndDeletesFiles` |
| P05 | Watcher offset persists | Restart does not replay old bytes | `TestRolloutOffsetPersists` |
| P06 | Global session files and marker/pending sidecars are private | Mode is user-only where supported | `TestSessionMarkerAndPendingFilesArePrivate` |
| P07 | Terminal cache hit returns later snapshot rows | Caller can restore full post-call context | `TestTerminalHitIncludesAfterEventsUntilCompaction` |
| P08 | Legacy session DB lacks `pre_compact_trail.call_id` | OpenSession migrates schema without losing usable old snapshots | `TestOpenSessionMigratesPreCompactCallIDColumn` |

## Codex Watcher

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| W01 | Replay existing rollout and freeze on compaction | Usable frozen trail | `TestWatcherReplaysAndFreezes` |
| W02 | Tail newly appended rollout lines | Store updates | `TestWatcherTailsNewLines` |
| W03 | Marker removed | Tail stops and stale in-memory pending calls for that session are dropped | `TestWatcherCleansUpOnMarkerRemoval` |
| W04 | Watcher-produced row can satisfy hook lookup | Cache hit is usable | `TestWatcherProducesUsableHits` |
| W05 | Second global watcher | Lock prevents duplicate ingestion | `TestWatcherSecondInstanceLocked` |
| W06 | Stale lock | Reclaimed | `TestWatcherStaleLockReclaimed` |
| W07 | Unknown rollout event | Skipped without crash | `TestParseHandlesUnknownEventGracefully` |
| W08 | Rollout schema drift for tool call/result fields | Graceful skip or parse | `TestParseHandlesSchemaDriftAliases` |
| W09 | Rollout tool_call persists immediately as running | Compaction can freeze in-flight work even before result exists | `TestWatcherInFlightAcrossCompactionEmitsRunningHit` |
| W10 | Late rollout result after watcher restart | Result attaches via frozen call_id metadata, not volatile in-memory pending | `TestWatcherRestartCanAttachResultToFrozenRunningCall` |
| W11 | Watcher shutdown | All in-memory pending call state is dropped | `TestWatcherShutdownDropsAllPending` |
| W12 | One global watcher covers existing project, new project, and projectless sessions | Each active session is tailed and cached independently | `TestGlobalWatcherCoversMultipleProjectsAndProjectlessSessions` |
| W13 | One global watcher covers multiple CLI terminals in the same cwd | Same-project rollout tails remain isolated by `session_id` | `TestGlobalWatcherCoversMultipleCliTerminalsInSameProject` |
| W14 | Stored rollout offset is beyond current file size at watcher startup | Treat as truncation/rotation and replay current file from byte 0 | `TestWatcherInitialOffsetBeyondTruncatedRolloutReplaysFromStart` |
| W15 | Rollout file shrinks while watcher is tailing | Drop in-memory pending state for that session and replay current file from byte 0 | `TestWatcherRunningTailHandlesRolloutTruncation` |
| W16 | Tail goroutine exits while marker remains | Refresh detects stopped tail and restarts when rollout is available again | `TestWatcherRefreshRestartsExitedTail` |
| W17 | Old active-session marker has no rollout file | Remove stale orphan marker after TTL; keep fresh markers for late rollout discovery | `TestWatcherRemovesStaleOrphanMarkerWithoutRollout`, `TestWatcherKeepsFreshMarkerWithoutRollout` |
| W18 | Multiple rollout files match one session id | Choose the newest matching rollout file | `TestFindRolloutFileChoosesNewestMatch` |
| W19 | A newer rollout file appears for an already-tailed session | Refresh restarts the tail on the newer file and replays it from a safe cursor | `TestWatcherRefreshSwitchesToNewerRolloutFile` |

## Installation And Config

| ID | Scenario | Expected behavior | Current test |
| --- | --- | --- | --- |
| G01 | Missing config | Defaults | `TestLoadDefaults`, `TestLoadMissingFileIsValid` |
| G02 | Project config overrides global/default | Project wins | `TestLoadProjectLocalOverridesGlobal` |
| G03 | Malformed config | Error, hook falls back safely | `TestLoadMalformedJSONReturnsError`, `TestMalformedConfigFallsBackSafely` |
| G04 | Empty config | Defaults | `TestLoadEmptyFileIsValid` |
| G05 | Never-cache patterns load | Patterns available in runtime config | `TestLoadNeverCacheBashPatterns` |
| G06 | Projectless desktop conversation with only global config | Global config still controls hook decisions | `TestProjectlessConversationUsesGlobalConfig` |
| INST01 | Claude install creates hooks | Managed entries exist | `internal/install` tests |
| INST02 | Claude uninstall removes only managed hooks | Unrelated hooks preserved | `internal/install` tests |
| INST03 | Codex install creates hooks | Managed entries exist | `internal/codexinstall` tests |
| INST04 | Codex uninstall removes only managed hooks | Unrelated hooks preserved | `internal/codexinstall` tests |
