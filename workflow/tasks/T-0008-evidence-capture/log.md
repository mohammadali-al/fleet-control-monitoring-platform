# T-0008 evidence-capture — doer log

## 2026-11-24 — initial implementation

Built the worker-agent evidence-capture subsystem (architecture ch.6 + §7.1) in a new
`internal/capture` package, bound to the session + cgroup from `internal/exec` (T-0007) via the
plain `sessionID` + `CgroupPath()` strings. One reviewable unit — not split.

**Capture mechanism:** cgroup v2 + **nftables `socket cgroupv2`** matcher (not eBPF). Per session we
install a tiny `inet` table whose `output` chain matches the session cgroup and mirrors every
matching outbound packet to a per-session **NFLOG** group with `log group N` (non-terminating, so it
never affects or blocks traffic — best-effort, §6.3b). A `NETLINK_NETFILTER` socket bound to the
group receives full-packet copies (`copy_range 0` ⇒ whole packet, so `payload_sha256` is of the full
payload even for large packets), which `decode()` turns into §6.3 records. ENOBUFS and a full
userspace buffer are counted as drops.

**Files**
- `records.go` — §6.3 record + gopacket decode (TCP/UDP/ICMP over IPv4/IPv6), always-recorded
  `payload_bytes_full` + full-payload `payload_sha256`, trim to `payload_head` (threshold configurable,
  capped at 4 KB).
- `srcip.go` — §6.4: route-NIC source IP for private targets (connectionless UDP route lookup),
  reflector consensus (6 services, ≥2 agree → confirmed, 1 alive → adopt, none → error) with a cached,
  timer/manual-refreshed `PublicIPCache`.
- `segments.go` — rolling segment writer (5 min / 50 MB), running SHA-256, disk guard (80 % warn /
  90 % pause, fail-safe no-delete), dropped-count accounting.
- `manifest.go` — per-machine manifest (SHA-256, bytes, record count, time range, schema, compression
  reserved `none`), atomic temp+rename writes, `Verify()` re-hashes every segment.
- `capture.go` — `Controller` (public-IP cache lifecycle, per-session NFLOG group allocation) and the
  per-session record loop; capture is best-effort and never blocks the command. `capture_live.go`
  (linux, root) is the NFLOG path; `capture_other.go` stubs non-linux.
- `config.go` — added `CaptureConfig` under `AgentConfig` (trim threshold, segment limits, disk
  thresholds, reflectors, NFLOG group); all optional, zero ⇒ package defaults.

**Out of scope (left for T-0009):** server sync/pull/push, pcapng/CSV/JSON export, hash chains.

**Tests** (synthetic packets, throwaway dirs, mock reflectors — no root, D-0002): records/trim/sha,
UDP+ICMP+IPv6, reflector consensus (≥2 / single / all-fail), source-IP resolver, segment roll by
size and by age, manifest SHA/counts + `Verify` tamper detection, disk-guard pause, dropped-count,
full session end-to-end via a fake source. Pass under `-race`.

**Root-gated / NOT exercised:** `TestLiveCaptureSmoke` (the real nft+NFLOG kernel path) **skips**
clearly without root/nft/cgroup-v2. The live kernel capture was **not** exercised in this sandbox
(no root); the nft orchestration + netlink NFLOG reader are implemented and compile but are untested
against a real kernel — flag for a root run at review.

Self-check: `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh agent`
passes. Adds `github.com/google/gopacket v1.1.19`; `golang.org/x/sys` promoted to a direct require.

<!-- wf: commits=<sha> left=- tests=agent:pass blocked=no -->
