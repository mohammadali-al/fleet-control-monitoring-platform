# T-0017 capture-live-hang-fix — doer log

## 2026-11-24 — bound every netlink recv; make Close always return

**Diagnosis.** In the pre-fix code the only indefinite block on the live path is the reader's
`unix.Recvfrom(s.fd, ...)` in `read()`. `Close()` set `closed`, then called `unix.Close(s.fd)` and
relied on that to wake the blocked `Recvfrom` — but on Linux closing an fd does **not** unblock a
`recvmsg`/`recvfrom` already parked on it in another goroutine's OS thread, so `read()` never returned
and `Close()`'s `s.wg.Wait()` blocked forever. This matches the observed evidence exactly: the smoke
test printed `=== RUN` and hung to the 30 s kill, yet **no `fleet_capture_*` table was left behind** —
because the old `Close()` ran `removeRule()` *before* `wg.Wait()`, so the table was torn down and only
the join hung. (`openNFLOG`'s config exchange is send-only — no `NLM_F_ACK`, no recv — so it was not
the blocker; I hardened it anyway so a future recv can't reintroduce an indefinite wait.)

**Fix (`internal/capture/capture_live.go`).**
- Arm `SO_RCVTIMEO` (`nflogReadTimeout = 250ms`, via new `setRecvTimeout`) on the netlink socket right
  after `Bind`. `read()` now wakes with `EAGAIN` at least every 250 ms even when idle, re-checks
  `closed`, and exits. This is the interrupt mechanism that guarantees the reader stops.
- `read()` loop: check `closed` at the top and after every `Recvfrom`; treat `EAGAIN`/`EINTR` as
  retry, `ENOBUFS` as a counted drop (unchanged), anything else as a logged exit.
- `Close()` reordered to a **safe** sequence: set `closed` → `wg.Wait()` (reader exits within one
  timeout, so this cannot block forever) → `unix.Close(fd)` → `removeRule()` → `close(pkts)`. The fd is
  now closed only **after** the reader has returned, so it is never closed out from under a blocked
  `Recvfrom`. Added an `installed atomic.Bool` so `Close()` only runs `nft delete` for a table it
  actually created (also keeps the stop-path unit test hermetic — no `nft` exec, no root).
- Clear Debug setup/teardown trace in `newLiveSource`, `openNFLOG`, and `Close` so if a hang ever
  reappears the last log line pinpoints the step.

**Tests.**
- `capture_live_test.go` (new, `//go:build linux`): `TestLiveSourceCloseUnblocksReader` proves, with
  no root, that `Close()` returns promptly and the reader exits while `read()` is parked in `Recvfrom`.
  It wires a `liveSource` onto one end of a `socketpair` (nothing is ever sent, so the reader truly
  blocks), then asserts `Close()` returns < 3 s, `Packets()` is closed (no goroutine leak), and a
  second `Close()` is a no-op.
- `capture_test.go`: `TestLiveCaptureSmoke` is now wrapped in a `smokeHangTimeout = 10s` watchdog so a
  regression that blocks setup/read/Close **fails fast** with a pointed message instead of blocking a
  root run to the outer kill. It passes a Debug logger so a real run traces every setup/teardown step.
  The live body still **SKIPs** cleanly off-root.

**Self-check.** `gofmt -l .` empty; `go vet ./...` clean; `GOOS=darwin go vet ./internal/capture/`
clean (non-linux stub unaffected; the new unit test is linux-tagged). `scripts/build.sh` builds both
binaries. `scripts/test.sh agent` passes; `go test -race ./internal/capture/` passes. The live smoke
SKIPs off-root as expected. (Pre-existing: `GOOS=windows` build fails in untouched `segments.go` via
`unix.Statfs` — this package targets unix-like platforms, not a regression.)

**For the supervisor to validate on a real root box.** Run
`sudo go test ./internal/capture/ -run TestLiveCaptureSmoke -v`. Expect: the Debug trace walks
`opening NFLOG socket → NFLOG socket bound → installing nft rule → nft rule installed, starting
reader`, then after the 2 s read window `stopping reader → reader stopped, closing socket → done`, the
test **completes in a few seconds** (well under the 10 s watchdog), and `nft list ruleset` shows **no**
`fleet_capture_*` table and the `fleet-capture-test` cgroup is gone. If it still hangs, the last Debug
line tells us the exact step to iterate on.

<!-- wf: commits=94b135e left=- tests=agent:pass blocked=no -->
