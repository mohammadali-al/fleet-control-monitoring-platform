---
id: T-0017
title: capture-live-hang-fix
state: merged
touches: [internal/capture/capture_live.go, internal/capture/capture.go, internal/capture/capture_test.go]
tests: [agent]
after: [T-0008]
shares: {}
model: {claude: "opus high"}
operator_check: "Supervisor validates on a real root machine: the live capture smoke completes promptly (no hang) and cleans up its nft table + cgroup."
---

## Why

Running T-0008's live capture on a **real machine** (root, nft, cgroup v2 all present) **hangs** — the
smoke test was killed at a 30 s hard timeout (exit 124). The unit tests skip the root path, so this was
invisible until a real run. Evidence capture does not actually work on real hardware until this is fixed.

## Observed

- `sudo capture.test -run TestLiveCaptureSmoke` prints `=== RUN TestLiveCaptureSmoke` then **never
  returns** (killed at 30 s).
- **No `fleet_capture_*` nftables table was left behind** after the kill, which suggests the hang is
  **before `installRule`** — i.e. in `openNFLOG` / the hand-rolled nfnetlink config exchange (blocking
  netlink recv waiting for an ack), or alternatively the reader/`Close` path never unblocks. Confirm the
  exact spot.

## What to build

**Backend (Go), `internal/capture` live path.**

1. **Find and fix the hang** so the full smoke sequence (`newLiveSource` → send one packet → read/2 s
   select → `Close`) completes **promptly**. Likely areas: the NFLOG netlink socket setup/config
   (`openNFLOG`/`nflogConfig`) blocking on a recv; and `Close` not interrupting the reader goroutine's
   blocking `recvmsg`.
2. **Make every blocking netlink op bounded / interruptible:** deadlines (e.g. `SO_RCVTIMEO` or a
   non-blocking socket with poll) on netlink reads; config exchanges must not wait forever for an ack;
   **`Close` must always return** and must reliably unblock/stop the reader (e.g. close the fd / use an
   eventfd or `SO_RCVTIMEO` wakeups), with no goroutine leak.
3. **Harden the smoke test** so a hang **fails fast** (an internal timeout that `t.Fatal`s) instead of
   blocking a CI/root run forever.

## Out of scope

- Record/trim/SHA/source-IP/segments/manifest logic (unchanged — already tested).
- Switching to eBPF (stay on nftables/NFLOG; fix it).

## Done means

- The live smoke completes promptly on a real root machine (**validated by the supervisor on a real
  box** — the sandbox has no root), sets up and **tears down** its temporary nft table + cgroup, and
  leaves nothing behind.
- `Close` is proven to return and stop the reader with no goroutine leak (unit-test the stop path with a
  fake/loopback fd where possible, without needing root).
- The hardened smoke test fails fast on a hang rather than blocking.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh agent` passes (the live test still **skips**
  cleanly off-root).

## Facts this brief relies on

- Live mechanism is cgroup v2 + nftables `socket cgroupv2` → NFLOG (keep it): `internal/capture/capture_live.go`;
  `Fleet_Control_Monitoring_Platform_Architecture.md:261`.
- The subsystem being fixed (T-0008): `workflow/tasks/T-0008-evidence-capture/task.md`.
- Finding recorded during T-0009 (live path untested off-root): `workflow/tasks/T-0009-evidence-sync-export/log.md`.

## Notes for the doer

- You **cannot** exercise the live path in the sandbox (no root). Write the fix with careful reasoning
  about the hand-rolled nfnetlink framing and netlink blocking semantics; make the stop/close path
  unit-testable without root; the **supervisor will run it on a real machine and iterate with you** via
  messages, so structure the code so a fix attempt is easy to validate (clear logs on setup/teardown).
- Prefer `golang.org/x/sys/unix` socket options (`SO_RCVTIMEO`, non-blocking + `poll`) over more
  hand-rolled netlink where it reduces the chance of an indefinite block.
