
## T-0071 done

Approach: PRECISE. The token store already binds a redeemed token to its machine (RedeemedBy), so I added a
read-only `GET /api/enroll/tokens/{code}` (internal/enroll/api.go, tokens.go `status`, `Service.TokenStatus`)
returning `{state: pending|used|expired, machine_id}` (404 unknown). No change to enrollment protocol, TTL or
one-use semantics. internal/panel/api.go needed no change (the enroll handler is mounted on the whole prefix).
UI (MachinesPage.jsx): after generating, polls the token status every 3s and on /ws/events `machine` events.
Used -> success card (machine id, role, specs, IPs from /api/enroll/machines) with Done (close) and Add another
(reset); code/one-liners hidden. Expired -> expiry notice with "Generate a new token". Bundle rebuilt.
Test: TestTokenStatusLinksMachine. gofmt/vet/build (no node)/server tests pass.

<!-- wf: commits=pending left=- tests=server:pass blocked=no -->
