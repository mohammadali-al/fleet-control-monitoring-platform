---
id: T-0071
title: enroll-success-confirmation
state: merged
touches: [web/src/pages/MachinesPage.jsx, web/src/hooks/, internal/enroll/, internal/panel/api.go,
          internal/panel/machines_test.go]
tests: [server]
after: [T-0047]
shares: {}
model: {claude: "sonnet high"}
operator_check: "After generating a token and the machine enrolling with it, the 'Add agent with a token' panel detects the enrollment and flips to a SUCCESS state showing the newly-enrolled machine (id, role, and its reported specs/IP) instead of sitting on the now-spent code. It then offers 'Done' (close) / 'Add another' (new token). The spent code is cleared and not left usable-looking."
---

## Why

When an operator generates a token and the machine enrolls with it, the enroll panel just keeps showing the
now-consumed code — no feedback that it worked or which machine joined. Make the panel react to the token
being used.

## What to build

1. **Detect the token's enrollment.** After `POST /api/enroll/tokens`, watch for that token being consumed:
   - **Preferred (precise):** expose a token→machine link — have the enroll service record the enrolled
     machine id against the token it redeemed (`internal/enroll/enroll.go` `Enroll`), and surface it either
     as a token-status the panel can poll (e.g. `GET /api/enroll/tokens/{id}` / include the issuing-token id
     on `GET /api/enroll/machines`) via `internal/panel/api.go`. The token is one-use/one-machine, so the
     mapping is 1:1.
   - **Fallback (front-end only):** snapshot `/api/enroll/machines` at issuance and detect the first NEW
     machine appearing after, driven by the existing poll + the `/ws/events` `machine` event.
   Pick the precise path if cheap; otherwise the fallback. State which in the log.
2. **Success state (React, `web/src/pages/MachinesPage.jsx` enroll section).** When the token is consumed,
   replace the code/one-liner view with a **confirmation**: the enrolled machine's id, role, and reported
   specs (OS/arch, version, IP — whatever the enroll-machines payload carries), plus **Done** (closes the
   panel) and **Add another** (resets to generate a new token). Clear/retire the spent code. If the token
   expires unused, show that too (and let them regenerate). Use the existing hook/live patterns
   (`web/src/hooks/`, `web/src/lib/events.jsx`). Rebuild + commit the web bundle (`./web/build.sh`).

## Out of scope

- No change to the enrollment protocol/security, the token TTL/one-use semantics, or the Stop/Detach list;
  no onboarding (SSH) changes; no multi-machine-per-token. Keep the existing generate/copy flow intact up to
  the point of success.

## Done means

- Generating a token then enrolling a machine flips the panel to a success confirmation naming the machine +
  its specs, with Done / Add another; the spent code is retired; an expired-unused token is shown as such.
- `gofmt -l .` empty, `go vet ./...` clean, `go build ./...` ok (no Node), `scripts/test.sh server` passes
  (if a token-status/link endpoint was added, test it); web bundle rebuilt + committed.

## Facts this brief relies on

- Enroll API: `POST /api/enroll/tokens` -> {code, expiry, one-liners}; `GET /api/enroll/machines` -> enrolled
  machines with specs (`internal/enroll/api.go:22-26`). The service redeems the token with the machine in
  `internal/enroll/enroll.go:243` (`Enroll`); `Machines()` at `:382`. React enroll flow + the machines poll:
  `web/src/pages/MachinesPage.jsx:827,1069,1429-1475` (generate token, code display, result card).

## Notes for the doer

- Keep it to the enroll panel's success path; don't disturb the generate/copy UI before success. Rebuild +
  commit the bundle. Run `./scripts/wf start T-0071` before committing; commit inside touches + the log. Do
  NOT push/merge/branch-delete or edit decisions/generated/other tasks/`state:` (except via `wf start`).
