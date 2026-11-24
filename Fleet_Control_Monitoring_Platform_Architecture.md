# Fleet Control & Monitoring Platform — Architecture Design Document

**Version 1.2 (Markdown edition)** · Status: design phase, no code written yet
**Scope:** design and architecture only. Implementation is a separate, later phase.

---

## 0. Terminology (fixed vocabulary)

To avoid confusion, these words are used consistently throughout:

| Term | Meaning |
|---|---|
| **Main server** / **main machine** | The single control-plane server. **Never** called an "agent." |
| **Agents** | The worker and monitor machines, collectively. |
| **Worker agent** | A machine that executes operator commands and captures evidence. |
| **Monitor agent** | A dedicated headless prober. No shell, no command executor. |
| **Agent software** | The one Go binary (`fleet-agent`) that runs on every agent, in one of two role profiles. |
| **Project** | A container grouping tests (and their Maneuvers/evidence/monitor data) by customer/engagement. |
| **Baseline item** | A reusable command preset (template + default inputs). Global library. |
| **Maneuver** | A reusable load plan: grouped baseline items + a load pattern on an interactive timeline. |
| **Test** | One execution of a Maneuver against a concrete target, inside a project. |

---

## 1. Executive Summary

This document specifies a fleet orchestration, monitoring and load-generation infrastructure consisting of:

- **One main server (control plane):** web panel, command dispatch, Maneuver planning, evidence archive, monitor-data store, watchdog policy management, internal CA, backup coordination. Runs on Linux, behind an authentication proxy (Authelia/Pomerium) with 2FA.
- **~10 worker agents (later 20–30):** execute operator commands (including coordinated load **Maneuvers**), capture all outbound traffic produced by those commands as evidence, and run a local watchdog for resource governance.
- **3–4 monitor agents:** headless probers that continuously measure target availability and response (HTTP(S), TCP connect, ICMP, DNS) and stream results back every 2–3 seconds. They have no command/shell role and **never stop**, even when the main server is down.

All machine-to-server communication runs over a single persistent TLS/mTLS WebSocket channel. The agent software is one Go binary with two role profiles (worker / monitor). Evidence logs live locally on each worker agent as checksummed segment files, are synchronized to the main server by checksum-diff pull or idle push, then archived in plain, uncompressed form with unlimited retention. The main server can replicate its archive to a **storage-only backup server**.

The platform's distinctive capability is the **Maneuver engine**: operators compose reusable command presets into timed load plans and run them against a single target, with a **Load Controller** on each worker agent that regulates CPU/RAM usage to a time-varying ceiling by launching and killing processes under startup dead-time. This is the one genuinely novel subsystem; most other components reuse existing tools.

The document ends with an explicit list of open decisions deferred to the implementation phase.

---

## 2. Requirements and Locked Design Decisions

Every row below is a decision confirmed by the operator. The rest of the document elaborates them; nothing here is re-opened silently. Rows changed or added in v1.2 are marked **[1.2]**.

| # | Area | Decision | Design consequence |
|---|------|----------|--------------------|
| 1 | Fleet size | ~10 workers now, 20–30 later; 3–4 monitors; 2–3 targets per monitor | Scales on one main-server process; no clustering needed |
| 2 | Command scope | Worker agents: arbitrary commands incl. root. Monitor agents: no shell/commands at all | One agent binary, two role profiles; role disables the command executor |
| 3 | Command dispatch | **Manual** commands run strictly one at a time per worker agent, queued. **Maneuvers run many concurrent process replicas by design** — the one-at-a-time rule applies only to operator-typed commands **[1.2]** | Serial executor for manual commands; Load Controller owns Maneuver concurrency |
| 4 | Dangerous commands | Allowed, but require typed confirmation phrase | Pattern list + confirmation enforced **in the agent**, not the browser |
| 5 | Evidence scope | Only traffic produced by panel-executed commands; agent-side capture | Capture bound to command/Maneuver sessions, not always-on |
| 6 | Protocol coverage | All TCP + UDP + ICMP | Single capture path, protocol-tagged records |
| 7 | Direction | **Worker agents: outbound only.** Monitor agents measure response time via probes | Filter at capture point; monitors use probes, not capture |
| 8 | Record content | Full payload; above 4 KB: first 4 KB + total byte count + SHA-256 of full payload | Proves completeness without storing oversized payloads; threshold configurable per node |
| 8b | Capture completeness | **Best-effort capture with a reported drop count** per segment/session **[1.2]** | At Mbps rates drops are ~0; evidence stays honest about any gap |
| 9 | Evidence export | pcapng and CSV/JSON | Archive retains enough metadata to regenerate both |
| 10 | Tamper-proofing | **Not required — trust exists between the parties** | No hash chain; per-segment SHA-256 is transport integrity only |
| 11 | Volume / retention | No rate limit; keep forever; plaintext, uncompressed on main server | zstd switch reserved in format but off by default; disk guard: 80% warn / 90% pause intake |
| 12 | Source IP recorded | Private target → machine's private IP (NIC owning the route). Public target → public IP via reflector consensus | Reflector set of 6 HTTP services; ≥2 agreeing = confirmed, exactly 1 alive = adopt; refreshed at boot, every 15 min, on network change, and via manual panel button |
| 13 | Session attribution | All packets grouped by `session_id`; command metadata stored once per session | Compact records; session table carries command, user, host, timestamps |
| 14 | Log flow | Evidence stays local (segment files); panel "pull" does checksum-diff sync; agents may push when idle | Chapter 7 defines segments, manifest, delta protocol |
| 15 | Monitoring probes | HTTP(S), TCP connect, ICMP, DNS; type chosen at monitor creation | Probe engine with pluggable prober per type |
| 16 | Monitor frequency | 300 ms – several seconds; global default + per-target override, pushed from panel | Frequency and timeout are task-config fields |
| 17 | Monitor timeouts | Pushed from server; agent keeps local fallback defaults only (HTTP 2 s, TCP 1.5 s, ICMP 1 s) | Server authoritative; local values last-resort |
| 18 | Monitor data | Returns every 2–3 s; called **monitor data** (distinct from worker **evidence logs**) | Separate storage path, separate API, separate UI vocabulary |
| 19 | Charts | All-monitors merged view first; per-machine and per-target views also | Downsampling on display; raw points stay queryable |
| 20 | Panel authentication | Authelia/Pomerium in front, with 2FA; no hand-built user management | Panel trusts forwarded auth headers |
| 21 | Metrics stack UI | Grafana embedded inside the main panel (iframe/plugin, SSO-shared) | Single web app |
| 22 | Watchdog | Automatic ladder: throttle → kill offending session / restart service → reboot; cooldowns, daily caps, boot window, hysteresis | Chapter 11; policy profiles pushed from server |
| 23 | Watchdog liveness | `Restart=always` + independent 1-minute systemd timer that verifies liveness | Two-layer restart; trust-model caveat documented |
| 24 | Capture mechanism | **cgroup v2 + eBPF / nftables `meta cgroup`** (all agents are Ubuntu). netns+veth fallback dropped **[1.2]** | OD-1 **resolved** |
| 25 | Connectivity mode | **Default `server_dial`**; `agent_dial` kept as a per-machine fallback flag **[1.2]** | OD-2 **resolved** (see 4.2) |
| 26 | TLS without domain | Internal private CA; server cert carries the server IP as SAN; per-agent client certs → mTLS | No domain, no public CA, no DNS |
| 27 | Onboarding | Panel form: IP + SSH username + password (or key) + role → automated provisioning | Chapter 4.5 |
| 28 | Design artifacts | This design is maintained as a single `.md` file | Docx/pdf exports are optional derivatives |
| 29 | Projects **[1.2]** | Tests are grouped into **projects**; **only one test may be active system-wide** at a time; completed tests listed by date within each project | Single active-test lock; project filter on evidence/monitor/reports |
| 30 | Baseline items & Maneuvers **[1.2]** | Reusable command presets ("baseline items") composed into reusable "Maneuvers"; both are a **global library** usable by all projects | Chapter 9 |
| 31 | Maneuver target **[1.2]** | A Maneuver targets **one `(ip, origin)`** (multiple **paths** of the same origin allowed); reusing a Maneuver needs only a fresh ip (+optional url) | Target identity = `(ip, origin)` |
| 32 | Load resource **[1.2]** | The resource regulated and reported is **live CPU + RAM, per worker agent, measured live**; shown in absolute units — **CPU as cores used**, **RAM as GB/MB** — per-item, per-second, and per-Maneuver total | Chapter 10 |
| 33 | Load Controller **[1.2]** | Maneuver execution uses reservation (Smith predictor) + feed-forward from the pattern + slow feedback, with per-PID activation detection; over-launch-and-cull **deferred** but its structural hooks built now | Chapter 10 |
| 34 | Monitor jobs **[1.2]** | **Stateful** monitor jobs, independent of test duration; **no duplicate job per `(ip, origin)`**; `ip` required, `origin` optional but shown first; unique `(ip, origin)` history; a test must have a monitor (confirm-to-start dialog) | Chapter 8 |
| 35 | Synchronized start **[1.2]** | Future-timestamp start with readiness ACKs; an agent that receives the order after `T` reports "missed," never starts late | Chapter 4.9 |
| 36 | Main-server outage **[1.2]** | Worker agents **continue the remaining Maneuver plan** on their own; on recovery they rejoin at the current wall-clock timeline position (no replay). Monitor agents **never stop** | Chapter 4.3 |
| 37 | Backup server **[1.2]** | A **storage-only** backup server (not a standby main); additive replication over SSH to a constant unique path; diff check + nightly schedule suggestion; manual backup + import buttons; **CA keys included** | Chapter 7.5 |
| 38 | Alerting **[1.2]** | No out-of-panel alerting; a human supervisor watches the panel | Everything visible in-panel; no email/Slack/Telegram |
| 39 | Operators **[1.2]** | One stateful full-access panel; operators only; no per-user permission levels | No RBAC layer beyond the auth proxy |
| 40 | Test report **[1.2]** | Per-test **HTML** report (interactive) | Chapter 13 |

---

## 3. System Topology and Components

```
                       ┌────────────────────────────────────────────────┐
                       │                   MAIN SERVER                   │
   Operator browser    │  ┌───────────┐  ┌──────────────────────────┐    │
  ┌──────────────┐     │  │ AuthProxy │→ │ Panel (single web app)   │    │
  │ Auth + 2FA   │────→│  │ Authelia/ │  │ · machines & sessions    │    │
  └──────────────┘     │  │ Pomerium  │  │ · command runner / term  │    │
                       │  └───────────┘  │ · projects & tests       │    │
                       │                 │ · baseline items library │    │
                       │  ┌───────────┐  │ · Maneuver timeline editor│   │
                       │  │ Fleet     │← │ · monitor charts         │    │
                       │  │ Server    │  │ · watchdog policy        │    │
                       │  │ (Go)      │  │ · evidence browser       │    │
                       │  │ WS hub,   │  │ · backup console         │    │
                       │  │ store, CA │  │ · embedded Grafana       │    │
                       │  └─────┬─────┘  └──────────────────────────┘    │
                       │  ┌─────┴────┐ ┌──────────┐ ┌──────────┐         │
                       │  │ Evidence │ │ Monitor  │ │ Maneuver │         │
                       │  │ archive  │ │ data DB  │ │ / policy │         │
                       │  └──────────┘ └──────────┘ └──────────┘         │
                       │        │ persistent mTLS WebSocket(s)           │
                       └────────┼────────────────────────────────┬──────┘
                                │                                 │
        ┌───────────────────────┼───────────────┐         ┌──────┴───────┐
        │                       │               │         │ Backup server│
 ┌──────┴───────┐        ┌──────┴───────┐  ┌─────┴──────┐  │ (storage only│
 │ Worker 1..N  │        │ Worker N+1.. │  │ Monitor 1..4│ │  over SSH)   │
 │ fleet-agent  │        │ fleet-agent  │  │ fleet-agent│  └──────────────┘
 │ (worker)     │        │ (worker)     │  │ (monitor)  │
 │ · executor   │        │              │  │ · probe loop│
 │ · capture    │        │              │  │ · no shell  │
 │ · Load Ctlr  │        │              │  │ · watchdog* │
 │ · watchdog   │        │              │  │             │
 │ · segments   │        │              │  │             │
 └──────────────┘        └──────────────┘  └────────────┘
```

\* Monitor agents run the watchdog in a stripped headless config.

Components:

1. **Fleet Server (Go):** WebSocket hub, machine registry, manual-command queue, Maneuver scheduler, session store, evidence archive, monitor-data ingest, watchdog policy distribution, internal CA, backup coordinator.
2. **Panel:** server-rendered frontend + xterm.js terminal + Maneuver timeline editor + embedded Grafana. No own user management.
3. **Agent software (`fleet-agent`, Go):** role-profiled daemon. Modules in chapter 5.
4. **Internal CA:** generated once at server install; issues the server certificate (IP SAN) and one client certificate per machine. **CA keys are part of the backup set** (chapter 7.5).
5. **Backup server:** storage only; receives additive evidence/archive/CA replication over SSH (chapter 7.5).

---

## 4. Communication and Command Architecture

### 4.1 Channel design

- One persistent **WebSocket (TLS/mTLS)** per machine carries everything: commands, live output chunks, telemetry, monitor data, file/log transfer. Multiplexed by message type.
- Why WebSocket and not MQTT: MQTT needs a broker as an extra component to run and secure, and its pub-sub/QoS model fits telemetry, not request/response command execution, streamed output with backpressure, and large file transfers. A raw WebSocket gives all of that with zero extra infrastructure and a first-class Go client (`coder/websocket`). (Decision locked.)

### 4.2 Connection initiation — default `server_dial`, `agent_dial` fallback (OD-2 resolved)

Who dials whom is a **per-machine config flag**; both directions are supported by the same binary. The **default is `server_dial`**.

| | `server_dial` (default) | `agent_dial` (fallback) |
|---|---|---|
| Who opens the connection | Main server connects to the agent's listener | Agent dials out to the main server |
| Fits | All machines on one LAN behind the same NAT/firewall (the expected deployment) | An agent on a different network / the public internet |
| Main server keeps | IP:port registry per agent + reconnect/backoff logic | Nothing; agent self-manages reconnection |
| Agent firewall | One inbound allow rule from the main server | None |
| Control semantics | Identical — commands flow main → agent down the open channel | Identical |

**Why `server_dial` is the default:** the expected deployment has the **main server itself behind the same NAT** as the agents, on one local network. In that setting reachability is symmetric, so the choice is the operator's preference for **master-initiated** connections, and `server_dial` matches it with no downside. `agent_dial` remains per-machine selectable for the future case of a remote agent that the main server (behind NAT) cannot dial — that agent dials out instead (its NAT, or the main's, is port-forwarded as needed).

**Recovery after a main-server outage** is therefore main-initiated: when the main server returns it re-dials every agent (for any `agent_dial` machines, those agents re-dial the main with capped backoff, reconnecting within one retry interval). See 4.3.

**Main-server IP change:** under `server_dial` the agents never need the main's IP (the main dials them), so a main IP change is transparent to the control channel. For any `agent_dial` machines, the main pushes its new IP to them **over SSH** (it holds their SSH credentials from onboarding). An agent's own IP change is updated in the panel registry.

### 4.3 Behavior when the main server is down

- **Manual commands:** the executor queue pauses (master down = no new orders). Correct and intended.
- **Running Maneuvers:** worker agents **continue the remaining Maneuver plan on their own** (the whole plan is pushed before start, so no live supervision is needed). The watchdog keeps protecting the machine throughout. On the main server's return, a worker agent **rejoins at the current wall-clock position of the Maneuver timeline** — it does **not** replay the slice it missed, so it stays aligned with its peers and the total time. If the Maneuver already ended during the outage, it stays done.
- **A worker agent that itself rebooted mid-Maneuver** rejoins the remaining schedule at the current timeline position on return.
- **Monitor agents never stop.** Probing continues; results buffer locally and flush when the main server is reachable again.
- Evidence segments keep being written locally throughout.
- On return, agents reconnect (backoff + jitter) and flush buffered monitor data and evidence (via normal delta sync).

### 4.4 TLS and certificates (no domain)

- On first install the main server generates an **internal private CA** (one time).
- It issues a **server certificate** whose SAN contains the server's **IP address** — clients validate "cert SAN matches the IP I connected to."
- Each machine gets a **client certificate** during provisioning → **mTLS**.
- Agents pin the CA certificate locally. No public CA, no DNS, no domain.
- Because the **default is `server_dial`** (the main dials the agent, so the *agent* is the TLS
  server), each machine's certificate carries **both** `serverAuth` and `clientAuth` EKUs and its
  **own IP in the SAN**. The same cert then works in either dial direction, and whichever side acts
  as the TLS server has its IP-SAN verified by the dialer. (This paragraph set was first written for
  the agent-dials-server case; both-EKU certs keep mTLS correct under the `server_dial` default.)
- **CA private keys are included in the backup set** (7.5): losing them forces re-enrollment of every machine.

### 4.5 Onboarding workflow

Per machine the operator provides: **IP + SSH username + password (or key) + role (worker/monitor)**.

1. Panel "Add machine" form → main server connects over SSH.
2. Main server installs the agent binary and systemd units, creates a dedicated system user, generates the machine ID and client certificate, writes the pinned CA and `connect_mode` flag.
3. Service starts; connection established (per the machine's mode); machine shows **online**.
4. Worker profile enables executor + capture + Load Controller + full watchdog; monitor profile enables the probe loop + headless watchdog and hard-disables the executor.

Removal / re-enrollment is a panel action; re-provisioning the same machine is idempotent (same machine ID → same identity).

### 4.6 One-at-a-time dispatch (manual commands)

- Per worker agent, **manual** commands execute strictly serially: the main server holds a per-machine FIFO queue; the panel shows queued / running / finished.
- This rule governs operator-typed commands **only**. Maneuver load replicas are concurrent by design and are owned by the Load Controller (chapter 10), not the serial queue.
- A crashed/hung manual command ends by timeout (default 300 s) or operator terminate; the queue advances.

### 4.7 Live terminal

- **Mode A — one-shot run (default).** Form: target host(s), command, optional timeout → `session_id`. CI-style live view (`queued → running → exited`), streamed stdout/stderr, evidence capture summary on exit, persistent history table with re-run.
- **Mode B — interactive shell (opt-in).** xterm.js ⇄ WebSocket ⇄ agent PTY (root, resize-aware). Same session tagging; shell traffic lands in the same evidence mechanism. Sessions list with per-session terminate.

### 4.8 Dangerous-command gate

- Server-side configurable pattern list (e.g. `reboot`, `shutdown`, `rm -rf /`, `mkfs`, `dd of=/dev/`, `iptables`/`nft` flush, `systemctl stop fleet-watchdog`).
- Match → modal (Mode A) or in-terminal prompt (Mode B) requiring a typed confirmation phrase.
- Enforcement is **in the agent**: the command message must carry the confirmation token or the agent refuses — including PTY input lines. Browser-side checks are UX only. Refusals are logged and shown.

### 4.9 Synchronized Maneuver start

A Maneuver that runs on several worker agents starts aligned, without firing blind:

1. Main server pushes the full plan and a **future start timestamp `T`** (a few seconds out).
2. Each selected worker agent **ACKs "ready."**
3. The main server **commits `T` only after all ACKs arrive**; if any agent has not ACKed by a deadline, it aborts/reschedules.
4. Each agent starts locally at `T`. If an agent somehow receives the order after `T` has passed, it reports **"missed"** and does **not** start late.

Alignment is only as good as the agents' clocks: with **chrony/NTP** on a LAN, expect roughly **10–50 ms**. Single-digit ms would need tight NTP/PTP; 1 s is acceptable, less is better. The tradeoff is purely clock-sync quality, no runtime cost.

---

## 5. Agent Architecture

One binary, `fleet-agent`, two role profiles selected by config:

| Module | Worker | Monitor | Notes |
|---|---|---|---|
| Channel client (WS, mTLS, reconnect/backoff, both dial modes) | ✔ | ✔ | Chapter 4 |
| Command executor (serial queue, exec + PTY, confirmation gate) | ✔ | ✘ | Disabled by role |
| **Load Controller** (Maneuver execution engine) | ✔ | ✘ | Chapter 10 |
| Session capture controller (chapter 6) | ✔ | ✘ | Only active with a live session |
| Evidence segment writer + manifest (chapter 7) | ✔ | ✘ | Local disk |
| Public-IP reflector client (6.4) | ✔ | (✔, cheap) | Workers mandatory |
| Monitor data buffer/flusher | ✘ | ✔ | 2–3 s flush cadence |
| Probe engine (HTTP(S), TCP, ICMP, DNS) | ✘ | ✔ | Chapter 8 |
| Watchdog (chapter 11) | ✔ full | ✔ headless | Same binary, stripped policy |
| Local config + policy applier (live reload) | ✔ | ✔ | Server-pushed profiles |

Runtime: systemd services (`fleet-agent.service`, `fleet-watchdog.service`, liveness timer). Agent requires root (capture, PTY, watchdog actions, cgroup control for the Load Controller).

---

## 6. Evidence Capture Subsystem

### 6.1 Session attribution

- Every panel-initiated execution (one-shot, shell, or Maneuver replica) runs inside a tracked cgroup owned by a `session_id`; the capture controller tags every packet with it.
- Command metadata is stored **once** per session.

### 6.2 Capture mechanism (OD-1 resolved)

All agents are **Ubuntu**, so capture is **cgroup v2 + a kernel matcher** (eBPF via `cilium/ebpf`, or nftables `meta cgroup`): precise, low overhead, no topology changes. The temporary-netns+veth fallback is **dropped**.

### 6.3 Packet record format

All protocols (TCP/UDP/ICMP), **outbound only**, full payload up to the trim threshold.

| Field | Content |
|---|---|
| `session_id` | Capture session this packet belongs to |
| `timestamp` | Capture time, sub-ms precision, NTP-disciplined |
| `protocol` | TCP / UDP / ICMP |
| `src` / `dst` | IP and port as seen on the wire |
| `flags` | TCP flags or ICMP type/code |
| `seq` / `ack` / `window` | TCP sequencing fields |
| `payload_status` | `full` or `trimmed` |
| `payload_bytes_full` | Exact total payload length — always recorded |
| `payload_sha256` | SHA-256 of the full payload, at capture time — always recorded |
| `payload_head` | First ≤4 KB when trimmed (threshold configurable per node) |
| `src_ip_used` | Source address chosen for attribution (6.4) |

**Honest scope of proof:** outbound-only capture records **what the agent sent, including its own half of the TCP handshake** (its SYN, its ACKs, seq/ack numbers). It does **not** see the target's SYN-ACK, so it does not by itself prove the target replied. **Target responsiveness is measured separately by the monitor agents** (chapter 8).

### 6.3b Drop accounting

Capture is **best-effort**: if the kernel capture buffer fills under burst, excess packets still reach the target but are not recorded. Each segment/session stores a **dropped-packet count**, so the evidence is explicit about any gap. At the expected Mbps rates over minutes-long tasks, drops are effectively zero.

### 6.4 Source address determination

- **Private target:** the machine's private IP on the NIC that owns the route to the target (looked up at capture time).
- **Public target:** the agent's public IP via **reflector consensus** — 6 HTTP "what-is-my-IP" services; ≥2 agree → confirmed, exactly 1 alive → adopt. Refresh at boot, every 15 min, on network-change, and via a manual panel button.
- Each record stores the **cached public IP at capture time**.

### 6.5 Local disk guard

**80% disk → warning in panel; 90% → pause intake** (agent pauses capture writes). Retention stays "forever"; the guard fails safe, it does not delete.

---

## 7. Evidence Log Storage, Synchronization, Export & Backup

### 7.1 Segments and manifest

- Evidence is written as **segment files**: new segment on **5 minutes** or **50 MB**, whichever first.
- A **manifest** per machine lists every segment with **SHA-256**, byte size, record count, time range, schema version.
- Plaintext, uncompressed on both sides; a reserved **zstd flag** (off) allows later compression with no format break.

### 7.2 Delta synchronization ("pull" + idle push)

- Panel **pull** per machine: main server compares its manifest copy against the agent's → downloads only missing/changed segments → verifies each SHA-256 → updates its manifest. Interrupted transfers resume mid-segment.
- **Idle push:** an idle agent (no session, queue empty) may proactively push pending segments. Same protocol as pull.

### 7.3 Archive on the main server

- Layout mirrors segment structure per machine/session; plaintext, unlimited retention, no rate limit.
- Main-server disk guard: **80% warn / 90% pause intake**.

### 7.4 Export

- **pcapng** — regenerated from packet records (trimmed payloads reconstructed as far as stored, with the record's SHA-256 testifying to the original).
- **CSV / JSON** — flat record export.
- Per session, per machine, or per time range; format chosen in the panel.

### 7.5 Backup server (storage only)

A **storage-only** replica of the main server's archive — **not** a standby control plane.

- **Setup:** panel form adds a backup server by **username + password + IP**. The main server owns the connection.
- **Constant unique path:** the backup destination path is **fixed and unique**, provided by the main server (agents/operators do not choose it).
- **Diff + schedule suggestion:** after a backup server is added, the panel computes the **data diff** between main and backup and **suggests a backup schedule around the middle of the night**.
- **Additive only:** nothing is ever deleted on the backup; replication is additive and runs over **SSH with checksum verification** (reusing the segment manifest + SHA-256).
- **Contents:** evidence archive, monitor data, Maneuver/project metadata, and the **internal CA keys** (without them, restore would force re-enrollment of every machine).
- **Manual controls:** an **admin button starts a backup immediately**; a separate **import button restores data from the backup server** (reading from the same constant path).
- **Reminders:** non-intrusive, **at most once a day**; never interrupts an operator mid-task.

---

## 8. Monitor Agents and Monitor Data

### 8.1 Role and probe loop

- Monitor agents are **dedicated, headless**: no shell, no executor. Each is assigned **2–3 targets** at creation.
- Probe types: **HTTP(S)** (status, latency, TLS info), **TCP connect** (connect time, success), **ICMP** (RTT, loss), **DNS** (query latency, answer). Chosen per target.
- Frequency: **300 ms – several seconds**, global default + per-target override, pushed from the panel.
- Timeouts pushed from the server; agent-local fallbacks only if no config has ever arrived.
- Monitor agents **never stop** (4.3): results buffer locally during outages and flush on reconnect.

### 8.2 Monitor jobs are stateful and identity-scoped

- A monitor job is a **stateful** object, **not bound to any test's duration** — it is bound to **tests** (it keeps running as the long-lived watcher of a target).
- **No duplicate job** may exist for the same **`(ip, origin)`**. (`origin` = `scheme://host:port`; two subdomains or two ports are different origins and are treated separately.)
- **Fields:** `ip` is **required**; `origin` is **optional but displayed first** (the more important field, entered as a URL from which origin + path are derived). Multiple **paths** of the same origin are allowed.
- A **unique `(ip, origin)` joined history** is maintained across the platform.
- **A test must have a monitor.** Starting a test without one prompts a **confirm-to-start dialog** that offers to create/attach the monitor first.
- (UX inspiration drawn from the operator's existing `uptime-monitor`, not copied.)

### 8.3 Terminology

Probe results are **monitor data** — never mixed with worker **evidence logs** (different storage, API, UI labels).

### 8.4 Monitor data flow, graphs, and command overlay

- Each monitor flushes results **every 2–3 s**; local buffering covers outages.
- Storage: time-series store (Prometheus/VictoriaMetrics) via the server's ingest path.
- **Charts:** all-monitors merged view is the default landing chart; per-machine and per-target views selectable. Display downsampled; raw points queryable.
- **Live window:** default **60 s** (configurable in settings) — the live monitor shows the last 60 s.
- **Command overlay (not live):** test commands are mapped onto the monitor timeline. **Hovering a point on the timeline shows a brief summary** — kinds of commands and packet counts sent in that interval (interval = the graph's current time resolution). This is **not required to be live**: evidence is not necessarily synced in real time, so the overlay appears whenever the underlying data is available on the main server.
- **History browsing UX:** the history view does **not** default to the 60 s window. It opens as an **overview of the entire timeline**, with dense detail collapsed into **merged labels**; **zooming in progressively reveals detail** (semantic zoom), like scrubbing a video editor.

---

## 9. Projects, Baseline Items, and Maneuvers

### 9.1 Projects

- A **project** groups tests (with their Maneuvers, evidence and monitor data) by customer/engagement.
- **Only one test may be active across the whole system at a time.**
- Completed tests are listed **by date** within each project.

### 9.2 Baseline items (reusable command presets)

A **baseline item** is a reusable command template plus default inputs. Fields:

| Field | Required | Notes |
|---|---|---|
| `name` | ✔ | |
| `command_template` | ✔ | Contains placeholders for `rate`, `duration`, `ip`, `domain`/`url` |
| `description` | — | |
| `rate` | — | Has a **default value**; substituted into the template |
| `duration` | — | Has a **default value**; the per-process self-terminate cap |
| `ip` | — at creation | Required only when the item is placed into a Maneuver |
| `domain` / `url` | — | Optional |
| `warmup` (startup lag) | — | Optional measured value; see 10.6 |

- **Only `name` and `command_template` are mandatory to create an item.** Target fields (`ip`, `origin`/`url`) and tuning fields are optional at creation and are **filled when the item is placed into a Maneuver**.
- All inputs behave the same way: each is substituted into `command_template` to produce the concrete command that gets replicated.
- **Items are a global library** — usable across all projects.
- **Editing an item inside a Maneuver:** the admin chooses, per edit, via buttons — **overwrite the item's defaults** (changes the library item) **or apply only to this Maneuver** (local override).

### 9.3 Maneuvers

A **Maneuver** is a reusable load plan:

- A set of selected baseline items, placed on an **interactive timeline** (video-editor style) with a **load pattern** (9.4) and a configurable **total time**.
- **One Maneuver targets exactly one `(ip, origin)`.** It may not mix items of two different ip/origin; multiple **paths** of the same origin are allowed.
- **A Maneuver is a reusable plan**; a **test** is one execution of it against a concrete `(ip, origin)` inside a project. Reusing a Maneuver needs only a **fresh ip (+ optional url)**.
- **Worker-agent selection:** the operator picks which worker agents run the Maneuver (**default: all selected**). The Maneuver is **mirrored** on each selected worker agent — each runs the whole plan independently, governed by **its own** live CPU/RAM ceiling. More agents = more aggregate load on the single target.
- Maneuvers and patterns are **reusable across all projects**.

### 9.4 Load patterns

A pattern defines, at each instant, the **ceiling of resource** (live CPU + RAM, per worker agent) the Maneuver's replicas may consume — it is a **ceiling, not a target**. The Load Controller spawns replicas to fill up to it when resource is free, and kills replicas to get back under it when it drops (chapter 10). **Default pattern: all-in-one.**

- **All-in-one (default):** ceiling = max from start to end.
- **Linear (sawtooth):** over a configurable **window length**, the ceiling ramps **0 → max**, then resets and ramps again; the window repeats to fill the Maneuver timeline. Replicas step up in **whole copies** (you cannot run a fraction of a command), so the ramp is a staircase.
- **Spikes:** inputs are **`pattern_length`** (e.g. 3 s) and **`spike_duration`** (< `pattern_length`, centered in it). During the spike the ceiling = max; in the gaps the ceiling = **0** (so replicas are killed → genuinely idle); the pattern repeats to fill the timeline.

The timeline editor lets the operator **paint these patterns interactively**. The patterns only set the **maximum** allowed resource per instant; free resource is used when available and possible.

---

## 10. Load Controller (Maneuver execution engine)

This is the one genuinely novel subsystem and deserves its own simulation/tuning effort. It runs on each worker agent and makes the machine's **aggregate CPU + RAM usage track the pattern's ceiling `r(t)`** by only **launching** and **killing** processes — there is no "throttle" state; a process is either running or killed. The hard part is **startup dead-time**: a launched process does not consume resources for up to several seconds, and that lag grows under load and when many processes start at once.

### 10.1 Control structure (v1 = `reserve_pid`)

Three mechanisms together (not alternatives):

1. **Reservation (Smith predictor):** every launched-but-not-yet-active process **counts against the ceiling** from its earliest possible activation. This is what prevents greedy overshoot — low readings are not treated as free headroom because pending work is already booked.
2. **Feed-forward:** launches are planned from the **known pattern** `r(t)`, ahead of each rising edge.
3. **Feedback (slow):** corrects estimates of **background load and footprint bias** only — it does **not** decide how many processes to launch. Kept slow (time constant several × worst lag), with a deadband, and **paused during edges** to avoid windup.

Full MPC is unnecessary: actions are integer launch/kill counts and the profile is known, so a look-ahead rule gives the same result. (MPC would only earn its cost if an optimizer had to choose a mix across many item types.)

### 10.2 Pooled footprint reservation

Each pending process is **not** charged its own worst-case footprint (that over-reserves and leaves a gap after activation, costing a second lag). Instead the budget check pools the uncertainty:

```
need(τ) = background
        + Σ active footprints (measured per-PID after ramp, else estimated)
        + Σ pending footprints that could be active by τ
with the pending/estimated sum charged as  Σ means + z·√(Σ variances)
```

Launch only if `need(τ) ≤ r(τ) − margin` across the look-ahead window.

### 10.3 Activation detection (the biggest single win)

- Watch **each process's own usage per-PID**, polling **≤ 200 ms** (`/proc/<pid>/stat` for CPU as **cumulative-counter diffs**, `statm` for RSS). Put an item's replicas in their **own cgroup** so `cpu.stat` / `memory.current` separate worker load from background.
- Mark a process **active when its usage is actually observed**, not when the clock says it should be. Confirming activations (rather than predicting them) is what keeps the Smith predictor stable under lag drift.
- The poll period also bounds how long any transient blip can last.

### 10.4 Edges and kill order

- **Rising edge:** the reservation rule first allows launches at `t_on − L_lo`, so nothing can activate before the edge. Bursts are **rate-limited and stepwise**, not flood-to-fill.
- **Falling edge:** kill on **schedule** (one poll ahead), do not wait for a measurement. First kill **pending** processes that no longer fit (cheap), then **active** ones until under the new ceiling.
- **Kill order:** **pending → youngest-first** (they are usually still warming up and not yet producing load); **active → oldest-first** (they are nearest their own `duration` expiry anyway), or **best-fit by footprint** when footprints vary.
- **Late ≠ failed:** hold a pending reservation until the process activates or until `L_hi + slack`; only then kill it and free the slot. Replacing late processes early is exactly what makes everything kick in at once.

### 10.5 Bursts and type selection

- **A burst of n is timed by the lag measured at concurrency n−1** (members slow each other down). Pre-measurement must launch several at once, not one in isolation.
- **Early-start probability is per-burst, not per-worker:** the per-member lag quantile is chosen so the chance that **any** member starts early equals the target.
- **Hold back the long-lead type:** a wide-/long-lag type that would otherwise swallow a whole edge **waits** if a tighter-lag type could take the same room a moment later (`type_order = spread`).

### 10.6 Lag model and calibration

- The startup lag is modelled **per item type** as quantiles (e.g. p5/p95) over buckets of **(background load × concurrency)**.
- **Calibration:** test-launch each item at several **concurrencies (e.g. 1, 5, 10 at once)** and **load levels**, measuring both **lag** and **footprint mean + variance**. (A neutral target such as `example.com` + its IP can be used to measure time-to-first-packet.) To estimate contention: launch 1, then 5, then 10, and compare median lags.
- **Lead time uses current (easier) load**; **latest-start and timeouts use the load ahead (harder)**. Overestimating the lead would let processes fire before the edge.
- **Learn online:** every observed activation is a new lag sample in a sliding window; recompute quantiles and widen the band if observed lags fall outside it.

### 10.7 Expiry handling (self-termination)

An item's `duration` is a per-process self-terminate cap. A plateau built at one edge **expires all at once** `duration` seconds later, and the naive replacement burst hits the worst lag. So **plan each replacement like a rising edge** (`launch at expiry − L_lo`); with activation detection, do **make-before-break handover** — launch the replacement early and kill the old process the moment the replacement activates (overlap = one poll). Optionally **jitter durations** to spread expiries.

### 10.8 Over-launch-and-cull — deferred, but hooks built now

Over-launching spares and culling them can fill pulses shorter than the lag spread, **but** its payoff depends on startup contention `c`, which is unknown until the base controller is logging real lags; under moderate/strong contention it can worsen pulses and raise overshoot. **It is deferred to v2.** To avoid a later rewrite, v1 is built with:

1. **Slots separate from processes** — a ledger of reservation slots (amount, earliest start, latest start, expiry); a process **binds** to a slot. v1: one process per slot. v2: spares allowed, first to activate takes the slot, unbound active process killed. (The shortcut version is the most fragile code; we structure it correctly from the start.)
2. **Activation as an event with a decision hook** — v1 just accepts; v2's admission check drops in there.
3. **Explicit concurrent-starter count passed into the lag lookup** (including same-tick planned launches) — never read pending count inside the lookup.
4. **Log every lag** (with load + concurrency at launch) and **every kill** (reason: pending/active, timeout, shed) — this yields the contention/spread/waste numbers that decide whether v2 is worth enabling.

### 10.9 Telemetry and reporting

- Per-item, per-second, and per-Maneuver **absolute** resource usage:
  - **CPU as cores used** (headline), with raw **CPU-seconds** retained underneath.
  - **RAM as GB/MB.**
  - Logging resolution **1 s default (configurable 1–5 s)**; read from the per-item cgroup counters, so no extra instrumentation.
- Feeds the test report (chapter 13) and the monitor timeline hover (8.4).

### 10.10 Simulation harness

A standalone simulator (`~/launchkill-sim/`) models load-dependent startup lag, noisy footprints, 1 Hz aggregate sampling and self-terminating workers, and compares `greedy / reserve_agg / reserve_pid / reserve_pid_cull`. It is the **tuning harness for the implementation phase**: fit `lag_median / lag_sigma / lag_load_coef / lag_conc_coef` to the real machines, then `sim.py sweep` to pick margins and quantiles **before** writing agent code. Its assumed lag model and its percentage units must be replaced with measured values and absolute units; the control logic transfers directly.

---

## 11. Watchdog and Resource Governance

### 11.1 Purpose and trust model

- The watchdog exists to **keep the machine from hitting max resource usage and freezing / becoming inaccessible**. Everything else is handled by a **human supervisor** watching the panel.
- `fleet-watchdog` — part of the agent binary, `fleet-watchdog.service`, `Restart=always`. Full on worker agents, headless/stripped on monitor agents.
- Self-contained metrics: cgroup v2 stats, `/proc`, netlink, sampled **every 5 s** (configurable). No dependency on node_exporter.
- **Trust model:** worker agents accept arbitrary root commands, so the watchdog is a **load-safety net, not a tamper-proof supervisor** — a root operator can stop it. Accepted by design; hardened by `Restart=always` + the liveness timer; every such event is logged.

### 11.2 Interaction with the Load Controller (important)

Maneuvers **deliberately** drive CPU/RAM toward a ceiling, so the watchdog must **not fight** the Load Controller:

- The **Load Controller's `margin`** already reserves headroom for the agent software, sshd and the watchdog, so the machine stays reachable while loaded.
- **Watchdog thresholds sit above the Load Controller's operating ceiling.** The watchdog acts only as a **backstop** for load that *escapes* the controller (a bug, a runaway non-Maneuver command), not for the Maneuver's intended load.

### 11.3 Detection semantics (anti-flapping)

- Sustained-breach window: threshold exceeded for **20 s** (4 consecutive samples) before any action.
- Hysteresis: trigger at breach level, clear only below the recovery level.

### 11.4 Escalation ladder

Per host, scoped to the offending service where attributable; steps enable/disable per policy:

1. **Throttle** — transient cgroup CPU quota (`systemd-run --scope CPUQuota=…`) + optional `tc tbf` on egress; held for `throttle_duration` (default 120 s), lifted early on recovery.
2. **Kill offending session** — the session's cgroup/process group is killed in one action (e.g. a command that opened 1 M connections dies with all of them at once). Never per-socket eviction.
3. **Service restart** — `systemctl restart <unit>`.
4. **Machine reboot** — `systemctl reboot`; agent auto-reconnects after boot.

### 11.5 Guards

- **Cooldowns:** 10 min after throttle, 30 min after restart, 60 min after reboot.
- **Daily caps:** max 3 service restarts and 1 reboot per day; counters persisted to disk (a reboot does not reset them).
- **Boot stability window:** no actions in the first 5 min after boot.
- **Caps exhausted → alert-only mode:** keep measuring, notify the panel, act no further until an operator resets counters (panel button; optional auto-reset at midnight).

### 11.6 Policy profiles

- All parameters live in a **YAML policy profile**: version-stamped, stored on the main server, **pushed over the control channel**, applied **live** without restart. Per-host and per-group overrides; minimal baked-in default for first boot; last-known profile keeps running if the server is unreachable.

### 11.7 Who watches the watchdog

1. `Restart=always` — instant restart on any exit.
2. **Independent systemd timer** (`OnUnitActiveSec=1min`) — one-shot check that `fleet-watchdog.service` is active, starts it if not. Catches a hung watchdog that never exits. Zero steady-state cost.
3. Both units enabled at boot → watchdog returns after reboot. Only a deliberate `disable --now` + mask keeps it down (logged, per the trust model).

---

## 12. Web Dashboard and Operator Workflow

### 12.1 Single-app experience

- One web panel behind Authelia/Pomerium (2FA). One **stateful, full-access** operator role; no per-user permission levels. The panel is served by `fleet-server` itself (Go `net/http`) and **listens on `:6789`**, reachable only through the auth proxy (D-0004).
- **Grafana embedded** inside the panel (SSO-shared).

### 12.2 Pages

1. **Machines** — registry, online state, role, `connect_mode`, public IP (manual refresh), disk state, onboarding form.
2. **Projects & tests** — project list, active-test indicator (one system-wide), completed tests by date.
3. **Command runner** — Mode A one-shot with live streaming, history, fan-out grid.
4. **Terminal** — Mode B xterm.js shells + sessions list with terminate.
5. **Baseline items** — the global preset library (create/edit, defaults).
6. **Maneuver editor** — interactive timeline, item selection, load-pattern painting, worker-agent selection, total time, per-item/live resource view (cores / GB).
7. **Monitor data** — merged + per-machine + per-target charts, 60 s live window, command-overlay hover, semantic-zoom history, probe config editor.
8. **Watchdog** — policy editor, per-host stage/events, counter reset.
9. **Evidence** — session browser, per-session packet records, pull button with sync status, exports.
10. **Backup** — add backup server, diff view, suggested nightly schedule, manual backup button, import button, daily reminder.

### 12.3 Visible-by-default principle

Every command, refusal, capture, watchdog action, Maneuver step, and sync operation appears in the panel. No hidden state, no silent actions. There is **no out-of-panel alerting** — a human supervisor watches the panel.

---

## 13. Test Reports

- Each test produces a **self-contained interactive HTML report**.
- Suggested contents: project/test identity and target `(ip, origin)`; the Maneuver plan and load pattern; participating worker agents; per-worker and aggregate **resource usage over the test window** (cores used, GB RAM); commands run with **packet/flow counts**; monitor charts for the test window (availability, response time) with the command overlay; evidence hashes / manifest references; start-alignment and any "missed" agents; watchdog events during the window; and any capture drop counts.
- Generated from the archive + monitor data after the fact (not necessarily live).

---

## 14. Security Model

- **Transport:** mTLS on every channel; internal CA with IP SAN (4.4); agents pin the CA. CA keys are in the backup set.
- **Operator auth:** Authelia/Pomerium + 2FA in front; no panel-local accounts; one full-access role.
- **Machine auth:** per-machine client certificate; revocation = cert revoke + panel removal.
- **Connectivity:** default `server_dial` on a shared NAT'd LAN. Under `server_dial` the **agent listens** and the main server dials it — that inbound port is the one added surface, restricted by firewall to the main server's IP and protected by mTLS. Under the `agent_dial` fallback the agent opens **no** inbound port (it dials out).
- **Command safety:** dangerous-pattern confirmation gate enforced agent-side (4.8); one-at-a-time manual dispatch; per-command timeout + operator terminate.
- **Trust boundary:** operators are trusted root; the system guards against accidents and mistakes, **not** malicious insiders — a documented, accepted property that applies to the watchdog and the shell alike. Evidence tamper-proofing is intentionally out of scope because the parties trust each other.
- **Data:** evidence plaintext by decision; integrity via per-segment SHA-256 (transport integrity, not tamper-proofing); no hash chains.

---

## 15. Reuse Map: Existing Tools per Module

| Module | Approach | Existing tool / library |
|---|---|---|
| Agent binary | Custom (Go) | — |
| Command channel | Reuse | `coder/websocket` (or gRPC stream variant) |
| TLS/PKI | Reuse | Go crypto/x509 + internal CA; IP SAN certs |
| Kernel capture | Reuse + thin glue | `cilium/ebpf` or nftables `meta cgroup` |
| Packet decode / pcapng | Reuse | `gopacket` (+ pcapng writer) |
| Command executor / PTY | Custom + reuse | `creack/pty`; systemd/cgroups |
| **Load Controller** | **Custom (novel)** | cgroup v2, `/proc`; tuned via `launchkill-sim` |
| Terminal UI | Reuse | `xterm.js` |
| Metrics store | Reuse | Prometheus or VictoriaMetrics |
| Metrics charts | Reuse (embedded) | Grafana |
| Monitor probes | Thin custom loop | Go stdlib (`net`, `http`, DNS), ICMP lib |
| Watchdog actions | Reuse | systemd (`systemd-run`, `systemctl`), `tc` |
| Auth | Reuse | Authelia or Pomerium |
| Provisioning | Custom (Go, SSH) | `golang.org/x/crypto/ssh` |
| Backup replication | Reuse | SSH + manifest/SHA-256 diff (rsync-style) |
| Report | Custom | Static interactive HTML |

---

## 16. Open Decisions (deliberately deferred)

| ID | Decision | Status | Notes |
|---|---|---|---|
| ~~OD-1~~ | Evidence capture mechanism | **Resolved** | All agents Ubuntu → cgroup v2 + eBPF/nftables; netns+veth dropped |
| ~~OD-2~~ | Default `connect_mode` | **Resolved** | Default `server_dial`; `agent_dial` per-machine fallback |
| **OD-3** | Over-launch-and-cull (10.8) | Deferred to v2 | Enable only if measured startup contention is low; v1 builds the structural hooks |
| **OD-4** | Clock sync for synced start (4.9) | Deferred | chrony/NTP expected ~10–50 ms; PTP only if single-digit ms is ever required |
| **OD-5** | Load Controller constants (margins, quantiles, poll) | Deferred | Fit the lag model to real machines, then tune via `launchkill-sim` before coding |

Everything else in this document is locked. Changes to locked decisions are made only by explicit operator instruction, as an edit to this file.
