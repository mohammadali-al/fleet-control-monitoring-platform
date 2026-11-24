---
id: T-0002
title: internal-ca
state: ready
touches: [internal/pki/ca.go, internal/pki/ca_test.go,
          internal/pki/tlsconfig.go, internal/pki/tlsconfig_test.go]
tests: [server]
after: [T-0001]
shares: {}
model: {claude: "sonnet high"}
operator_check: ""
---

## Why

The internal CA is the root of trust for the whole system: the mTLS control channel (T-0003), agent
identity, and onboarding all depend on it. Build it as a standalone, well-tested `internal/pki`
library before anything uses it. Keep it stdlib-only.

## What to build

**Backend (Go), package `internal/pki`.**

1. **CA (`ca.go`).**
   - `NewCA(opts) (*CA, error)` — generate a CA key (ECDSA P-256 **or** Ed25519; note the choice in a
     comment) and a self-signed CA certificate (long validity, e.g. 10y, `IsCA`, `KeyUsageCertSign`).
   - `(*CA) Save(dir)` / `LoadCA(dir)` — CA cert + key as PEM files on disk; a loaded CA issues valid
     certs identical in capability to a fresh one.
   - `(*CA) IssuePeer(id string, ips []net.IP, validity time.Duration) (certPEM, keyPEM []byte, error)`
     — one leaf cert per machine (the main server is also a "peer"). **Both** EKUs
     (`serverAuth` + `clientAuth`); `id` in CN and a SAN; `ips` in `IPAddresses`. Signed by the CA.

2. **TLS config (`tlsconfig.go`).** Helpers that both ends use, trusting **only** our CA (no system
   roots):
   - `ServerTLSConfig(certPEM, keyPEM, caPEM) (*tls.Config, error)` — presents the peer cert, and
     `RequireAndVerifyClientCert` against our CA (`ClientCAs`).
   - `DialTLSConfig(certPEM, keyPEM, caPEM, expectedIP net.IP) (*tls.Config, error)` — presents the
     peer cert, verifies the far side against our CA, and checks the far side's cert covers
     `expectedIP` (IP-SAN match; set `ServerName` to the IP, which Go matches against IP SANs).
   - `PeerID(*tls.ConnectionState) (string, error)` — extract the verified peer's `id` from its cert.

## Out of scope

- Any networking, WebSocket, or HTTP (that is T-0003).
- The real on-disk key layout in server/agent config, onboarding/SSH provisioning.
- Revocation / CRL / OCSP — **deferred** (note it in a comment; architecture mentions revoke at §14,
  not needed for the channel to work).

## Done means

- `NewCA` → `Save` → `LoadCA` round-trips; the loaded CA issues a usable peer cert.
- A peer cert issued with an IP validates when that IP is dialed, and **fails** when a different IP is
  expected (IP-SAN mismatch).
- In-test mTLS over an in-memory pipe (e.g. `net.Pipe` + `tls.Server`/`tls.Client`): a handshake
  **succeeds** between two peers of our CA; **fails** when one side presents a cert from a *different*
  CA; **fails** when the dialing side presents no cert against a `ServerTLSConfig`.
- `PeerID` returns the `id` that was issued.
- `gofmt -l .` empty, `go vet ./...` clean, `scripts/test.sh server` passes.

## Facts this brief relies on

- Internal private CA; **server cert carries the server IP as SAN; per-agent client certs → mTLS**;
  agents pin the CA: `Fleet_Control_Monitoring_Platform_Architecture.md:74`, `:138`, `:179`, `:180`,
  `:181`, `:182`.
- **Both-EKU, IP-SAN per-machine certs** work in either dial direction (default `server_dial` makes
  the agent the TLS server): `Fleet_Control_Monitoring_Platform_Architecture.md:183`.
- mTLS on every channel; machine auth = per-machine certificate: `:581`, `:583`.
- Stack: Go `crypto/x509` + internal CA (`workflow/decisions/D-0001-stack.md`).

## Notes for the doer

- **Stdlib only** (`crypto/*`, `encoding/pem`, `net`, `time`). No external modules.
- Tests generate throwaway CAs/keys at runtime; never read or write real key material (D-0002).
- Keep PEM `[]byte` as the interchange type; don't invent a config/file layout — T-0003 and later
  tasks decide where keys live.
