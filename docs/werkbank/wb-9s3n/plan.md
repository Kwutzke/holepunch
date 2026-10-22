# Plan: reliable reconnect (wb-9s3n)

The design is `docs/werkbank/wb-9s3n/design.md`. Its box 10 holds the acceptance tests A1–A6 and J, and box 8 the criteria they prove. This plan says what the exploration found and how the work is cut.

## Approach and sequencing

1. **The base lands first, as its own pull request.** The ~870 uncommitted lines in the main checkout are finished feature work: the ASG bastion resolver, the tray, upstream resolver discovery and session error classification. Every reconnect change edits the same `runService` and `ssmSession`, so the base has to be reviewable on its own before anything is stacked on it. Its laconic findings and the one flaky test it carries are fixed in that pull request.
2. **The reconnect work is one pull request**, in the design's order:
   1. **Process-group stop (A1).** It comes first because A5 and J assert that the old process group is gone, which only holds once stop kills the group.
   2. **Start retry (A2, A3).** It comes before the watcher because J needs a reconnect after a network event to survive a failing start.
   3. **Watcher and engine reaction (A4, A5, J).**
   4. **Daemon log (A6).** It is independent of the rest and goes last.

## Traps

- **Starting → Reconnecting is an illegal transition today.** The table at `internal/state/machine.go:35` allows only Connected and Failed from Starting. `TestTransition` lists Starting→Reconnecting among the invalid transitions (`internal/state/machine_test.go`). The retry has to add the edge and move that test case to the valid list. Otherwise `transition` fails and the loop returns, the same bug by another path.
- **Starting → Stopping is illegal too** (`machine.go:35`). `stopService` ignores the error (`engine.go`, `_ = si.transition(state.Stopping)`). With retries, a service spends more time in Starting, so `down` during a start leaves the state unchanged. A2 asserts that `down` while retrying ends in Disconnected.
- **The process group has to be set in `SSMManager.Start`, after the command factory runs** (`internal/session/manager.go:59-62`). The test factory `fakeCLIFactory` (`manager_test.go:233`) builds its own `exec.CommandContext`. Setting the group in the factory would leave production uncovered. Context cancel needs `cmd.Cancel` to signal the group, not just the `aws` process. `go.mod` declares Go 1.26.1, so `Cancel` and `WaitDelay` are available.
- **A network-event stop must not cancel the service context.** `stopService` (`engine.go:512`) cancels it, and then the loop returns at `engine.go:427`. The reaction has to stop only the session, so `sess.Wait` returns with the context still live and the reconnect path runs.
- **The backoff is not safe for concurrent use** (`internal/reconnect/backoff.go`, no lock). Only the service loop may touch it, so the network reaction must not reset it from the event goroutine.
- **The engine's events are dropped when the channel is full** (`engine.go:96-101`, 100-slot buffer). A network event with 16 services emits about 64 state changes. Tests that count events must wait on state instead.
- **`TestTrayRegister_StreamsEnvelopes` is flaky on the base.** It failed once in a full run and passed 5 times in isolation. The `CmdUp` call races the `CmdTrayRegister` subscription, so events published before the subscription registers are lost (`internal/daemon/server_subscribe_test.go:132-175`). Slice 1 fixes this before stacking work that is judged by green CI.
- **The daemon's output is discarded at launch, not inside the daemon.** `launchDaemon` sets `Stdout`/`Stderr` to nil (`cmd/up.go:97-100`), and the `aws` children inherit `os.Stderr` (`manager.go:62-65`). Pointing the daemon's stdio at the log file at launch therefore captures both the `log` output and the stderr of `aws`. `restart` launches through the same path (`cmd/restart.go`).
- **The Stop hook lints every file the branch touches.** Each slice inherits laconic findings in the base files it edits. Slice 1 clears them, so slice 2 starts clean.

## Verified facts

- The terminal start failures are the `Failed` + `return` blocks at `internal/engine/engine.go:340-342`, `351-353`, `362-364`, `381-383` and `402-404`.
- Expired credentials stay terminal at `engine.go:454-464`. The target-rotation fast retry is at `engine.go:470-484`, with the delay set at `engine.go:510`.
- `ssmSession.Stop` signals `s.cmd.Process` alone, with SIGTERM and then SIGKILL (`internal/session/manager.go:140-170`).
- The daemon is detached with `Setsid` (`cmd/proc_unix.go`, `sysProcAttr`). It shuts down through `eng.Stop` on SIGINT/SIGTERM (`cmd/daemon.go:108-112`).
- Base test run on 2026-10-22: every package passes except `internal/daemon`, which fails `TestTrayRegister_StreamsEnvelopes` once. `go vet ./...` is clean.

## Existing tests at risk

- `internal/engine/engine_test.go`:
  - `TestBastionResolverErrorFailsService`, `TestSessionStartFailure` and `TestDNSAddFailure` assert the terminal behaviour and are rewritten to A2.
  - `TestSessionDropReconnects`, `TestTargetNotConnectedTriggersInvalidateAndFastRetry`, `TestCredentialsExpiredHaltsReconnect` and `TestStopServices` must stay green.
- `internal/state/machine_test.go` `TestTransition`: Starting→Reconnecting moves from the invalid list to the valid one.
- `internal/session/manager_test.go` `TestSSMManager_Wait_CredentialsExpired` and `TestSSMManager_Wait_TargetNotConnected` run real `sh` children through the factory. They must stay green once the process group is set.
- `internal/daemon/server_subscribe_test.go` `TestTrayRegister_StreamsEnvelopes` (the flaky test above).

## Slice block

```json
{
  "slices": [
    {
      "title": "the ASG bastion, tray and resolver work lands with its laconic findings resolved",
      "reason": "",
      "merge_blocking": false,
      "subtasks": [
        {"title": "the uncommitted ASG bastion, tray, resolver and session-error work from the main checkout is carried into the slice worktree unchanged, tracked and untracked files alike", "check": "a diff of the worktree against the main checkout's working tree over the carried files is empty, and the build and vet both exit 0"},
        {"title": "the tray-register stream test no longer races the Up call: events are published only after the subscription is registered", "check": "the tray-register streaming test passes 50 consecutive runs under the race detector, and the full suite passes under the race detector"},
        {"title": "every laconic gate finding in the carried files is fixed, and each warn finding is either fixed or kept with a laconic:ignore reason", "check": "laconic over the branch's changed files reports zero gate findings and no unexplained warn findings, and the full suite still passes under the race detector"}
      ]
    },
    {
      "title": "tunnels recover from sleep and network changes, retry failed starts, leave no plugin behind and keep a log",
      "reason": "value on its own",
      "merge_blocking": false
    }
  ],
  "many_slices": ""
}
```
