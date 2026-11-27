# T-0043 enroll-addr-default: log

## Entry 1 (doer)

- `deploy/install-server.sh`:
  - Added `FLEET_AGENT_DIAL_PORT` (default `7444`) and `FLEET_ENROLL_PORT` (default `7445`) mirroring `FLEET_AGENT_PORT` across var initialization, `load_answers_file`, `:-default` fallback, `run_preflight` `check_port_state`, `collect_answers` custom-ports prompt, and `write_install_answers` (`install-answers.env`). No new CLI flag added; write-if-absent behavior for `config.json` preserved.
  - Updated `render_server_config_example()` to include `"agent_dial_addr": ":${FLEET_AGENT_DIAL_PORT}"`, `"enroll_addr": ":${FLEET_ENROLL_PORT}"`, and `"enroll_host": "${FLEET_PANEL_DOMAIN}"` in the `server` block without reordering existing fields.
- `deploy/REQUIREMENTS.md`: moved `7444/tcp` (`server.agent_dial_addr`, agent-dial mTLS) and `7445/tcp` (`server.enroll_addr`, token-enrollment HTTPS bypassing the proxy with pinned-pubkey TLS) into the main Ports table as on by default, noting `FLEET_AGENT_DIAL_PORT` and `FLEET_ENROLL_PORT` overrides, and removed the disabled-by-default optional listeners list.
- `deploy/README.md`: updated the architecture diagram and section 4 to document token enrollment as on by default (all three fields written on fresh installs), that both `7445/tcp` and `7444/tcp` must be reachable from enrolling machines, the `FLEET_ENROLL_PORT` / `FLEET_AGENT_DIAL_PORT` overrides, and that existing upgrades preserve `config.json` (requiring adding all three fields + restarting `fleet-server`, or a fresh reinstall).
- Verification: `bash -n deploy/install-server.sh` passed; `shellcheck deploy/install-server.sh` (v0.10.0 installed temporarily in `/tmp` and cleaned up) passed cleanly; `python3` `json.loads` dry-render of `render_server_config_example` parsed cleanly with `enroll_addr: ":7445"`, `enroll_host: "fleet.example.com"`, and `agent_dial_addr: ":7444"`.

wf: T-0043 done
