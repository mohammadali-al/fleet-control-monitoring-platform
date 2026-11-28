# T-0068 log

## Panel update endpoint + Machines UI
- Backend: POST /api/machines/{id}/update and POST /api/machines/update-outdated (body {workers_only}); errors mapped (unknown 404, offline 409, binary unavailable 503). Backend interface gained UpdateAgent/UpdateOutdated.
- Front-end (MachinesPage.jsx): version cell with Outdated flag (agent version != server version), update status text, per-row Update icon + row menu item, details-drawer Update agent button, page-level "Update outdated (N)" action calling the endpoints above. Bundle rebuilt via ./web/build.sh.
- Checks: gofmt clean, go vet clean, go build ok without node, server + agent test groups pass.

wf: T-0068 done
<!-- wf: commits=see-git left=- tests=server:pass,agent:pass blocked=no -->
