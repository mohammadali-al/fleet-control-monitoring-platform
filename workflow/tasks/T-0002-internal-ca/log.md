# T-0002 internal-ca - log

## 2026-11-24 - doer (sonnet high)

Built `internal/pki` (stdlib only): `ca.go`, `tlsconfig.go`, plus tests.

- **CA key algorithm: ECDSA P-256** (noted in a comment on `NewCA`). CA cert: self-signed, 10y default,
  `IsCA`, `KeyUsageCertSign|CRLSign`, path length 0. Save writes `ca.crt` (0644) and `ca.key` (0600,
  PKCS#8); `LoadCA` checks the key matches the cert.
- `IssuePeer(id, ips, validity)`: fresh P-256 leaf, both EKUs, `id` as CN and DNS SAN, `ips` as IP SANs,
  random 127-bit serial, NotBefore backdated 5 min, rejects empty/non-ASCII/wildcard id and validity past
  CA expiry.
- `ServerTLSConfig` (RequireAndVerifyClientCert, ClientCAs = our CA only), `DialTLSConfig` (RootCAs = our
  CA only, ServerName = expected IP), both TLS 1.3 minimum. `PeerID` reads CN from `VerifiedChains[0][0]`
  only (never unverified `PeerCertificates`).
- Revocation (CRL/OCSP) not implemented; deferred, noted in the package comment.
- Tests: Save/Load round trip (both loaded and fresh CA issue certs that verify), IP-SAN match and
  mismatch, mTLS success, reversed roles (both-EKU), different-CA rejected in both directions, no client
  cert rejected, system roots not used, PeerID, constructor/validation errors. All keys are runtime
  throwaways in `t.TempDir()`/memory.
- Test note: the handshake helper wraps `net.Pipe` with a queued-write conn, because the unbuffered pipe
  deadlocks when one side aborts with an alert while the other is mid-write. TLS 1.3 also reports a
  client-cert rejection to the client only at its first Read, so helpers do one Read/Write after
  Handshake.
- Self-check: `gofmt -l .` empty; `go vet ./...` clean; `go test -race -count=5 ./internal/pki` and
  `scripts/test.sh server` pass; `scripts/build.sh` ok (bin/ removed afterwards).

<!-- wf: commits=00c8363 left=- tests=server:pass blocked=no -->
