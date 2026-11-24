# T-0009 evidence-sync-export — doer log

## 2026-11-24 — initial implementation

New package `internal/evidence` (consumes T-0008's segments + manifest from `internal/capture`, rides on
`internal/channel`). Go 1.26.8.

**One protocol for pull and push (`sync.go`).** Bytes always flow agent -> server; the server owns the
archive, so it alone diffs and decides where to resume.
- pull: server `ev-manifest-req` -> agent `ev-manifest` (same id).
- idle push: agent sends the *same* `ev-manifest` unprompted (id `push-N`); the server, seeing no request
  waiting for that id, runs the very same `syncFrom` routine. There is exactly one diff-and-fetch code path.
- then, per missing/changed segment: server `ev-range {file, sha256, offset}` -> agent `ev-chunk*`
  (last flagged eof) -> server hashes the whole file -> `ev-verify {file, ok}`.
- Channel additions (small, `envelope.go`): `ev-manifest-req`, `ev-manifest`, `ev-range`, `ev-chunk`,
  `ev-verify` (named `ev-*` because `chunk` already means exec output). No channel redesign.
- Resume: in-flight bytes accumulate in `archive/<machine>/.partial/<file>.<sha16>.part`; the next sync
  resumes at that file's size (the digest is in the name, so a changed segment never resumes onto stale
  bytes). Verification hashes the *whole* file, so a damaged partial is caught too: a mismatch on a
  resumed partial is discarded and refetched once from byte 0; a mismatch on a fresh transfer is rejected
  (discarded, never archived, reported in `Result.Rejected`, agent told via `ev-verify ok=false`).
- Agent serves only segments listed in its manifest, only up to the manifest byte count, refuses a stale
  digest / bad offset / path-like names. The busy agent (`Idle()` false or nil) sends nothing; an idle
  agent offers only segments the server has not yet confirmed with `ev-verify`.
- Wiring is by plain handlers (`Agent.Handle`, `Syncer.Handle`) so a later task can install them as the
  channel `Handler`; `Sender` is satisfied by both `*channel.Dialer` and `*channel.Listener`.

**Archive (`archive.go`, `manifest.go`).** `DataDir/archive/<machine>/<session>/<segment>` plus the
server's manifest copy at `archive/<machine>/manifest.json` (atomic temp+rename). Plaintext, no deletes.
A segment is filed under its first record's session; all sessions it contains are indexed in the manifest.
Disk guard: 80 % warn (logged once per transition) / 90 % pause (`ErrIntakePaused`, checked before every
segment; fails safe, never deletes). Machine/segment names are checked as single path components.

**Export (`export.go`).** `Selector{Machine, Session, From, To}` (any combination; From inclusive,
To exclusive). `ExportPCAPNG` (gopacket `pcapgo.NgWriter`, link type RAW, one interface per machine),
`ExportCSV`, `ExportJSON`. Packets are regenerated from the record fields; lengths are those of the
original, so a trimmed payload yields capture length < original length (like a snaplen capture), checksums
are computed only when the whole payload is present. Both formats stream.

**Notes / findings for the supervisor**
1. **T-0008 gap:** `BuildRecord` stores `payload_head` only when the payload is *trimmed*; a `full`
   record keeps just the SHA-256 and length, not the bytes (architecture 6.3 says "full payload up to the
   trim threshold"). I cannot fix that here (outside touches), so exported pcapng packets for `full`
   records carry headers only (capture length < original length) unless the record has a head. The export
   code uses `payload_head` whenever present, so it needs no change once the writer stores it.
2. pcapng packets have no per-packet field for the record's payload SHA-256 in this gopacket writer; the
   SHA-256 travels in the CSV/JSON export (and the interface comment says so).
3. T-0008 creates one `SegmentWriter` per session over the same machine dir and manifest.json; concurrent
   sessions would race on that manifest. Not touched here; the sync code reads the manifest fresh from
   disk for every request. Worth a look in a T-0008 follow-up.
4. `go.mod`/`go.sum`: `pcapgo` imports `golang.org/x/net/bpf`, so `golang.org/x/net v0.42.0` was added
   (indirect). A module fetch ran (online).

**Tests** (both ends in-process over the real mTLS loopback channel, synthetic segments/records, D-0002;
all pass under `-race`, repeated 8x): delta pull (first / idle / new+changed segments, byte counts on both
ends, archive SHA match), mid-segment resume (the agent drops the connection before the 6th chunk, a fresh
agent listens on the same address, the server resumes: complete segments not resent, interrupted one
continues from its partial, only the remainder is served), corrupt segment rejected, corrupt partial
healed by one refetch, idle push over the same protocol + busy agent sends nothing, archive layout +
reopen, disk guard 85 % warn / 90 % pause / recover, agent refusals; export: valid pcapng read back with
`pcapgo.NgReader` and decoded with gopacket (TCP/UDP/ICMPv4/IPv6/ICMPv6, trimmed vs full), CSV/JSON equal
to the records, scope selectors (session / machine / time range / combinations), empty selection.
External check: a pcapng written by the export opens in `capinfos` and `tshark` (6 packets, Raw IP,
nanosecond timestamps, dissected as TCP/HTTP/UDP/ICMP/ICMPv6).

Self-check: `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` and
`scripts/test.sh agent` pass.

<!-- wf: commits=6e0e1fe left=- tests=server:pass,agent:pass blocked=no -->
