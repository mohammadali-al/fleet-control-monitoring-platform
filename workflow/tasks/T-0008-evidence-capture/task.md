---
id: T-0008
title: evidence-capture
state: merged
touches: [internal/capture/capture.go, internal/capture/records.go, internal/capture/srcip.go,
          internal/capture/segments.go, internal/capture/manifest.go, internal/capture/capture_test.go,
          internal/capture/records_test.go, internal/capture/segments_test.go,
          internal/config/config.go, go.mod, go.sum]
tests: [agent]
after: [T-0007]
shares: {}
model: {claude: "opus high"}
operator_check: ""
---

## Why

The platform's distinctive, contractual output: capture the outbound traffic a command produces, bound
to its session, as checksummed local segment files. This is the technically riskiest subsystem
(kernel-level), so it is scoped as the agent-local capture + record + local storage; server sync and
export are the next task (T-0009).

## What to build

**Backend (Go), worker-agent side.** Binds to the session + cgroup from T-0007.

1. **Capture** (`internal/capture/capture.go`): **cgroup v2 + a kernel matcher** — eBPF via
   `cilium/ebpf`, or nftables `meta cgroup` — filtering to the session's cgroup, **outbound only**, all
   TCP/UDP/ICMP. Enable for the session's lifetime; tag every packet with its `session_id`.
   **Best-effort** with a **dropped-packet count** per segment/session (do not block the command).
2. **Records** (`internal/capture/records.go`): the §6.3 record — `session_id`, timestamp (sub-ms,
   NTP-disciplined clock), protocol, src/dst, flags, seq/ack/window, `payload_status` (full/trimmed),
   `payload_bytes_full` (always), `payload_sha256` of the **full** payload (always, at capture time),
   `payload_head` (first ≤4 KB when trimmed; threshold configurable per node), `src_ip_used`.
3. **Source IP** (`internal/capture/srcip.go`) per §6.4: **private target** → the machine's private IP
   on the NIC owning the route to the target (looked up at capture time); **public target** → public IP
   via **reflector consensus** (6 HTTP services; ≥2 agree → confirmed, exactly 1 alive → adopt; refresh
   at boot, every 15 min, on network-change, and on demand). Each record stores the **cached** public IP
   at capture time.
4. **Segments + manifest** (`internal/capture/segments.go`, `manifest.go`): write records to **segment
   files** under **`DataDir/evidence/<machine>/`** (D-0006) — new segment on **5 min or 50 MB**,
   whichever first; plaintext, uncompressed, with a reserved (off) zstd flag. A **manifest** lists each
   segment with **SHA-256**, byte size, record count, time range, schema version. Local **disk guard**:
   80 % warn / 90 % pause capture writes (§6.5).

## Out of scope

- **Sync to the main server and export** (pull/push, pcapng, CSV/JSON) — that is **T-0009**.
- Session creation / command execution (T-0007) — consume the session + cgroup it provides.
- Tamper-proofing / hash chains (not required, decision #10).
- Monitor-agent probing (different subsystem).

## Done means

- Given a session cgroup running a command that makes outbound TCP/UDP/ICMP, capture produces records
  tagged with the `session_id`, **outbound only**, with correct `payload_bytes_full` and a `payload_sha256`
  of the full payload; payloads > the threshold are trimmed to `payload_head` with status `trimmed`.
- A dropped-packet count is recorded (best-effort); capture never blocks the command.
- Source IP: a private-target record uses the route's NIC IP; a public-target record uses the reflector
  consensus result (tested with a mock reflector set: ≥2-agree, single-alive, all-fail paths).
- Segments roll on 5 min / 50 MB; the manifest's SHA-256, counts, and ranges match the files;
  re-reading the manifest verifies every segment. Disk guard pauses writes at 90 %.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh agent` passes.

## Facts this brief relies on

- Capture mechanism cgroup v2 + eBPF/nftables, outbound only, session-tagged:
  `Fleet_Control_Monitoring_Platform_Architecture.md:261`, `:256`, `:54`, `:265`.
- Record format incl. always-recorded `payload_sha256`: `Fleet_Control_Monitoring_Platform_Architecture.md:269`, `:277`.
- Source IP (private NIC / public reflector consensus): `Fleet_Control_Monitoring_Platform_Architecture.md:289`, `:290`, `:60`.
- Segments (5 min / 50 MB) + manifest (SHA-256, count, range, schema): `Fleet_Control_Monitoring_Platform_Architecture.md:303`, `:304`.
- Evidence under the base data dir: `workflow/decisions/D-0006-data-directory.md`.
- Session + cgroup provided by the executor: `workflow/tasks/T-0007-command-executor/task.md`.

## Notes for the doer

- Adds `github.com/cilium/ebpf` and/or `github.com/google/gopacket` (updates `go.mod`/`go.sum`) —
  **needs module downloads**; if offline, stop and report blocked.
- Kernel capture needs root; gate real-capture tests behind a root/capability check and **skip clearly**
  when unavailable, but keep record encoding, trimming, SHA-256, source-IP consensus, and segment/
  manifest logic fully unit-testable **without** root (feed synthetic packets).
- Pick eBPF **or** nftables `meta cgroup` during implementation against the real kernel; keep the record
  format and everything downstream identical either way (that is the whole point).
- This is a large task; if it must be split, split along §6 (capture+records+srcip) vs §7.1 (segments+
  manifest) and tell the supervisor — do not silently drop scope.
