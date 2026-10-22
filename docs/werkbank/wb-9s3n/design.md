# Reliable reconnect

**Scope.** This is an argumentative design for werkbank ref `wb-9s3n`, tracked in kata `holepunch#m0ef` under the charter `holepunch#ced8`. It decides how holepunch notices a dead tunnel, how it cleans up its child processes, and which failures it retries. The slicing and the code shape belong to the plan. The missing `127.0.0.127` loopback alias is out of scope: it is a setup step (`holepunch setup`), not a code defect.

## Contents

1. Problem and goal · 2. Non-goals · 3. Constraints · 4. The domain · 5. Architecture ·
6. Data flow · 7. Public errors · 8. Acceptance criteria · 9. Acceptance journey ·
10. Acceptance tests · 11. Work order · Verification

## 1. Problem and goal

1.1 holepunch reconnects a tunnel only when the `aws ssm start-session` process exits. When the connection to AWS dies, `session-manager-plugin` keeps running and keeps listening locally. So `aws` never exits, the status says Connected and the tunnel carries nothing. On 2026-10-22 the machine had 59 such plugins. Each had its AWS-side socket `CLOSED` from an earlier network (`172.20.10.11` or `192.168.68.60`) and its local `LISTEN` socket still open.

1.2 Stopping a session signals `aws` only. Its plugin child is reparented to PID 1 and lives on. Every `down`, `restart` and daemon exit leaks one plugin per service.

1.3 When any step of starting a session fails (DNS entry, bastion lookup, process start, local listener), the service goes to Failed and stays there. So a reconnect attempt that runs before the network is back ends the service for good.

1.4 The daemon's output is discarded, so nothing about an incident survives a restart.

1.5 **Goal:** every configured tunnel stays usable without a manual `holepunch restart`, and every behaviour in box 8 is proven by an acceptance test that fails on the current code.

## 2. Non-goals

2.1 Probing a tunnel end to end. The plugin accepts local connections even when dead, and the remote services (postgres, redis, HTTP) share no protocol a generic probe could speak.

2.2 Cleaning up after `down -f` (SIGKILL). A killed daemon cannot signal its children. Sweeping orphans at the next start would also kill plugins the user started by hand.

2.3 Log rotation beyond a single size cap.

2.4 Changing `session-manager-plugin` or replacing `aws ssm start-session` as the transport.

2.5 Fixing the existing 59 orphans: that is a one-off `pkill`.

## 3. Constraints

3.1 macOS and Linux, pure Go, no cgo. The charter says so, and the repository already splits platform code by build tag (`setup_darwin.go` / `setup_linux.go`).

3.2 `aws` and `session-manager-plugin` are external and cannot be modified. holepunch controls only how it starts and signals them.

3.3 **Deviation from a settled decision.** Today a failure while starting a session is terminal: the service goes to Failed and its loop ends, and three engine tests assert this (`TestBastionResolverErrorFailsService`, `TestSessionStartFailure`, `TestDNSAddFailure`). This design reverses that. Such failures retry on the reconnect backoff. Expired credentials stay terminal (`TestCredentialsExpiredHaltsReconnect` keeps its meaning).

3.4 The ~870 uncommitted lines in the main checkout (ASG bastion resolver, tray, resolver discovery, session error classification) are the base this work builds on. They ship first with their laconic findings resolved, so the reconnect work reviews against a committed base.

## 4. The domain

4.1 A **service** is one forwarded endpoint (`profile/service`). It holds at most one **session** at a time.

4.2 A **session** is one `aws ssm start-session` process together with the `session-manager-plugin` it spawns. The two live and die as a unit, the **session process group**.

4.3 A **dead tunnel** is a session whose process group is alive but whose connection to AWS is gone. It cannot be observed from the process state.

4.4 A **network event** is anything after which every session's connection to AWS must be presumed dead: the host resumed from sleep, or the set of local non-loopback addresses changed.

4.5 A **transient start failure** is any failure while starting a session except expired credentials.

## 5. Architecture

5.1 **Session.** This unit owns the session process group. It starts `aws` as the leader of its own process group, and stopping or cancelling a session signals the whole group: SIGTERM, then SIGKILL once a deadline passes. Its existing classification of exit errors (expired credentials, target not connected) does not change.

5.2 **Network watcher.** This is a new unit that knows nothing about sessions. It ticks on a fixed interval and emits a network event when either:
- the wall-clock time between two ticks exceeds the interval by a margin, which means the host was asleep; or
- the set of local non-loopback interface addresses differs from the previous tick.

It depends only on the clock and the interface list. Both are injectable, so tests can drive it without sleeping or changing the network.

5.3 **Engine.** It keeps its per-service loop and gains two behaviours:
- **(a)** On a network event, it stops the session process group of every Connected service. Each loop then sees its session end and takes the existing reconnect path.
- **(b)** A transient start failure moves the service to Reconnecting and waits out the backoff, instead of going to Failed and returning.

The engine does not own the watcher. It takes the watcher's events as an input.

5.4 **Daemon.** It wires the watcher to the engine and appends its output to `~/.holepunch/daemon.log`. At startup, a log above 10 MiB is renamed to `daemon.log.1`.

## 6. Data flow

6.1 **Dead tunnel.** Host resumes or the network changes → the watcher emits a network event → the engine stops every Connected service's process group → each service loop sees its session end → Reconnecting → backoff → start. If the network is not back yet, the start fails transiently → Reconnecting → backoff → start, until it connects.

6.2 **Stop.** `down`, `restart` or daemon exit → the engine stops each service → its session signals the whole process group → no plugin outlives the daemon.

## 7. Public errors

7.1 `holepunch status` shows Failed only for expired credentials (with the existing `aws sso login` hint) and for configuration errors that no retry can fix. Unknown profile or service is rejected before any loop starts, as today.

7.2 While a service retries, `status` shows it as Reconnecting, with the last start error in the ERROR column. The user sees why it is not connected without the retrying stopping.

7.3 A failure to stop a process group is logged as a warning and does not fail `down`.

## 8. Acceptance criteria

8.1 After the watcher observes a sleep gap or a change of local address, every service that was Connected reaches Connected again through a new session, with no user action, within 30 s of the network being reachable.

8.2 A session whose plugin stand-in never exits on its own is replaced after a network event. Its old process group no longer exists.

8.3 After `down`, `restart` or a daemon exit by signal, no process from any session's process group is alive.

8.4 When starting a session fails transiently, the service retries on the backoff and reaches Connected once the failure clears. It does not stop at Failed.

8.5 When a session ends with expired credentials, the service still stops at Failed and emits the credentials-expired event.

8.6 Daemon output from before a restart can still be read from `~/.holepunch/daemon.log` after it.

## 9. Acceptance journey

The user's laptop runs holepunch with two services Connected. Each session's `aws` stand-in spawns a child that never exits, which is the dead plugin. The laptop "sleeps": the injected clock jumps 10 minutes and the first two bastion lookups after it fail. Without anyone calling holepunch, both services pass through Reconnecting and reach Connected on new sessions, and the old process groups are gone. Then `down` runs, and no stand-in process from any session remains. The daemon log contains the network event, the failed lookups and the reconnects.

## 10. Acceptance tests (one per criterion, each written failing first)

| # | Criterion | Test shape | Fails today because |
|---|---|---|---|
| A1 | 8.3 | Real `SSMManager` whose command factory runs a shell `aws` stand-in that spawns a `sleep` child. Call `Stop`, then assert the child's PID is gone. Repeat with context cancel. | Only `aws` gets SIGTERM, so the child survives. |
| A2 | 8.4 | Engine with a bastion resolver that fails twice and then succeeds. Assert the states go Starting → Reconnecting → … → Connected. Do the same for session start and DNS add. | The service goes to Failed and its loop returns. The three tests in 3.3 are rewritten to this. |
| A3 | 8.5 | The existing credentials-expired test stays green unchanged. | — (guard) |
| A4 | watcher | A watcher with a fake clock and a fake address list. A clock jump above the margin emits one event, as does an address-set change. A steady state emits none. | The unit does not exist yet. |
| A5 | 8.1, 8.2 | Engine wired to the real `SSMManager`, a never-exiting stand-in and a fake watcher. Emit an event, then assert a second `Start` per service, the old PIDs gone and the state Connected. | Nothing reacts to a network event. |
| A6 | 8.6 | Start the daemon with a temporary home, log a line, stop it, start it again, then assert the first line is still in `daemon.log`. | The output goes to nil. |
| J | box 9 | A5 extended with a failing resolver and a `Stop` at the end. It is the journey test. | Combination of the above. |

## 11. Work order (the cut the plan will refine)

1. **Land the base.** Move the uncommitted diff into the worktree, resolve its laconic findings (the gate findings are fixed; each warn finding is fixed or kept with a `laconic:ignore` reason), and get `go test ./...` green.
2. **Process-group stop** (A1).
3. **Retry transient start failures** (A2, A3).
4. **Network watcher + engine reaction** (A4, A5, J).
5. **Daemon log file** (A6).

## Verification

- `go test ./... -race` is green in each slice's worktree.
- Every acceptance test is shown failing on the commit before its fix: the red run is recorded in the slice's PR.
- Manual check after slice 4: `holepunch up`, switch Wi-Fi to the phone hotspot, and confirm that within 30 s `holepunch status` shows every service Connected with a reset uptime and `psql service=staging -c 'select 1'` succeeds. Then `holepunch down` and confirm `pgrep -f session-manager-plugin` prints nothing new.
