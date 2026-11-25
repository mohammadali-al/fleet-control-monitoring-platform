# T-0026 token-enrollment: log

## 2026-11-25: token enrollment built (Go 1.26.8), two commits (backend, then UI + wiring) + one `.patch`

**Split, as the brief allowed.** Commit 1 (`222c0e8`) is the whole backend; commit 2 (`2857354`) is the Machines-page UI,
the `cmd/fleet-server` wiring and this log. Both land clean; nothing was left half done.

**Built.**

- **`internal/enroll/tokens.go`.** One-time token store: a 256-bit secret plus an 8-character code from an
  unambiguous alphabet (no 0/O/1/I/L), 10-minute TTL. The code only fetches the script (which carries the
  secret); only the secret authorises downloads and redeems. `redeem` runs the enrollment under the store lock
  and binds the token to the machine only if it succeeded, so two machines cannot both win a token and a bad
  request (garbage CSR, macOS worker) does not burn it.
- **`internal/enroll/enroll.go`.** `Service`: `Mint`, `Script`, `Enroll`, `Stop`, `Detach`, `Machines`,
  `Revoked`. `Enroll` signs the CSR's public key (`pki.SignCSR`) for the id the server picked (from the host
  name; unique across all registry entries, detached ones included) and ignores the request's own
  subject/SANs, records the machine (role, `agent_dial`, cert fingerprint, specs, source address,
  `enrolled_via=token`) and persists + reconciles. The enrollment endpoint's own TLS cert is issued by the CA
  and its public key is what the one-liner pins.
- **`internal/enroll/server.go`.** The token-gated endpoint, on its **own TLS listener** (so it bypasses the
  auth proxy): `GET /enroll/e/{code}` (script), `GET /enroll/ca.crt`, `GET /enroll/agent?os&arch`,
  `POST /enroll/csr` (CSR + specs form, reply = machine id line + certificate). Every token failure is the
  same 401; failed attempts are rate-limited per address (10 per minute, then 429). The token is only ever in
  an `Authorization` header, never logged (nor is the code).
- **`internal/enroll/install.go`.** One source for everything installed: the bash script, the systemd unit
  (`Restart=always`, `WantedBy=multi-user.target`, enabled), the launchd plist (`RunAtLoad`, `KeepAlive`),
  the agent config (role, `connect_mode=agent_dial`, `server_addr`, cert/key/CA paths), a `fleet-agent`
  wrapper (`start|stop|status`, else the real binary), and the Stop/Start commands. The script generates the
  key + CSR with `openssl` **on the machine**, sends only the CSR + specs, checks the CA against a baked
  SHA-256, and refuses a macOS worker before downloading anything. It is wrapped in `main() {...}` so a piped
  script cannot be eaten by a command reading stdin. `OneLiner` is
  `curl -fsSk --pinnedpubkey 'sha256//<pin>' 'https://host:port/enroll/e/<CODE>' | sudo bash` (only the short
  code travels; `-k` is safe because the pin is checked regardless, verified against real curl).
- **`internal/enroll/api.go`.** Operator API (mounted beside the panel, behind the proxy, JSON-only POSTs):
  `POST /api/enroll/tokens`, `GET /api/enroll/script?role=` ("show script", token placeholder),
  `GET /api/enroll/machines`, `POST /api/enroll/machines/{id}/stop|detach`.
- **`internal/pki/ca.go`.** `CA.SignCSR`: checks the CSR self-signature (proof of possession), accepts
  ECDSA/Ed25519/RSA>=2048, issues the same peer leaf as `IssuePeer` (CN/DNS SAN = the server-chosen id, both
  EKUs) for the requester's key. The private key never exists on the server.
- **`internal/registry/registry.go`.** `Specs` (hostname, OS, version, kernel, arch, CPUs, memory),
  `EnrolledVia`, `Stopped`; `clone` deep-copies Specs.
- **`internal/config/config.go`.** `server.enroll_addr` (TLS listener, off unless set), `server.enroll_host`
  (what machines reach this server at), `server.agent_binary_dir` (`fleet-agent-<os>-<arch>`; the existing
  `agent_binary_path` is the fallback for the server's own OS/arch).
- **`cmd/fleet-server/enroll.go` (new) + 6 lines in `main.go`.** `enableEnroll`: builds the service, starts
  the listener, mounts `/api/enroll/` over the panel. `fleetRunner` sends Stop through the manager's command
  queue (`Submit`, confirmed).
- **Machines page.** "Add agent (token)" (role picker, big code, expiry countdown, Linux + macOS one-liners
  with Copy, **Show script** link), a "Token-enrolled agents" table (specs, state, **Stop**, **Detach**, each
  behind a confirm dialog). If enrollment is off the button is disabled with a hint. All server text goes in
  through `textContent`.

**Done means.**

- *Single-use + 10-minute TTL, valid Linux and macOS one-liners:* pass (`TestMintReturnsCodeAndOneLiners`,
  `TestTokenIsSingleUse`, `TestTokenExpires`; a worker gets no macOS one-liner; a used or expired token is
  refused over HTTP too).
- *CSR to cert bound to the machine, private key never sent, registry + specs:* pass
  (`TestEnrollBindsCertToMachineAndRecordsSpecs`, `TestEndpointFullFlowOverPinnedTLS` checks no `PRIVATE KEY`
  crosses the wire; `TestScriptEnrollsALinuxWorker` runs the real script with real curl/openssl against the
  real endpoint and checks the installed key matches the issued cert).
- *macOS worker refused:* pass at all three layers (mint, binary download/CSR, and the script itself), and a
  macOS monitor enrolls with a launchd plist (`TestMacOSWorkerIsRefusedMonitorAllowed`,
  `TestScriptEnrollsAMacMonitorAndRefusesAMacWorker`).
- *Stop disables the unit:* pass for the rendered commands (`systemctl disable --now --no-block
  fleet-agent.service`; `launchctl disable ... && launchctl bootout ...`), the wrapper's `stop`/`start`, and
  the plist/unit (`Restart=always`, `KeepAlive`).
- *Detach revokes:* pass at the registry/manager level, and observed live (see the smoke run below).
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/build.sh` ok, `scripts/test.sh server` passes; the panel
  serves all pages with the API mounted (`TestPanelStillServesAllPagesWithEnrollmentMounted`).

**Real end-to-end smoke (not committed; torn down).** Real `fleet-server` + the real `fleet-agent` binary,
with the fleet patch applied: minted a code through the API, ran the one-liner flow (pinned curl, real
script, stubbed `id`/`systemctl`, paths moved to a temp root), started the installed agent from its
generated config: it **connected over agent_dial and showed online** (monitor and worker, specs recorded),
survived a server restart, Stop on the worker delivered `systemctl disable --now --no-block
fleet-agent.service` to the agent over the channel and showed "stopped" while down and cleared when it came
back, Detach dropped it to offline with the revoked fingerprint recorded, and it **stayed offline**.

**Outside `touches` (needs the supervisor).**
`workflow/tasks/T-0026-token-enrollment/fleet-accessors.patch`: `internal/fleet/open.go` gets `Manager.Registry()`,
`Manager.CA()` and `Manager.Reconcile()` (19 lines, no behaviour change). Enrollment adds machines to the
manager's registry and must tell it to start/stop their links; none of that is reachable from `cmd/` today.
`git apply --check` is clean. `cmd/fleet-server` asserts the manager implements these, so **until the patch is
applied, setting `enroll_addr` is a clear startup error** (enrollment stays off; nothing else changes).

**Gaps and decisions.**

- **Stop on a *monitor* agent is not possible remotely.** There is no channel operation to stop a service,
  and a monitor has no command role, so Stop reaches only worker agents (it is a one-shot root command
  through the manager's queue). For a monitor the panel says so (HTTP 501) and points to
  `sudo fleet-agent stop` on the machine or Detach. Closing this needs a small `service-control` channel
  envelope handled in both agents (`internal/channel`, `internal/agent`, outside this task's touches).
- **Detach keeps a "removed" record** (like the SSH Remove) rather than deleting the entry: the revoked
  fingerprint is recorded and the id is never reissued, so an old certificate cannot match a new machine.
  The channel listener refuses it because the machine no longer has a link; real CRL/OCSP stays deferred
  (see `internal/pki` package doc).
- The machine id is derived from the host name (sanitised, `-2`, `-3` on a clash).
- `check-app` is not installed in this environment; UI checks are the panel tests plus `node --check` on
  `live.js`. The page was not looked at in a browser.
- Operators must put the binaries to serve in `agent_binary_dir` (`fleet-agent-linux-amd64`, ...);
  `agent_binary_path` serves only the server's own platform. Packaging remains a later task.

<!-- wf: commits=222c0e8,2857354 left=- tests=server:pass blocked=no -->
