# T-0078 Frontend Widget Design (Design-First Review)

## 1. Data Contract & Propagation (Confirmed)

- `fleet.MachineInfo` now carries `Resource *ResourceState` (`json:"resource,omitempty"`).
- Both `GET /api/machines` and `/ws/events` (`type: "machine"`) serialize `MachineInfo` directly, so `machine.resource` arrives in `useMachines()` (`web/src/hooks/useMachines.js`) with no panel-server changes.
- Payload shape when present:
  ```json
  {
    "cpu_percent": 42.5,
    "mem_percent": 68.2,
    "load_per_cpu": 0.75,
    "egress_mbps": 12.4,
    "at": "2026-11-28T15:45:00Z"
  }
  ```
- When absent (monitor agents, offline machines before first report, or older worker agents), `machine.resource` is `undefined`.

---

## 2. Table Row Widget (`web/src/pages/MachinesPage.jsx`)

### Placement
- Add a **`Resources`** column immediately before **`Disk`** in the main registered-machines table (`Machine | Role | State | Version | Connect mode | Addresses | Resources | Disk | Last seen | Actions`, updating the empty-state row `colSpan` from `9` to `10`).
- Keeping `Resources` as its own column prevents crowding the `Disk` guard bar/chips while letting operators scan CPU, memory, and disk side by side.

### Visual Form (Reusing `.meter` & Theme Tokens)
- Reuse the existing `.meter`, `.meter__bar`, `.meter__fill`, `.meter__text`, `.meter--warn`, and `.meter--danger` CSS classes (`web/assets/css/components.css:200-206`) and existing theme tokens (`--meter-fill`, `--warn`, `--danger`, `--surface-2`, `--border-strong`, `--text-muted`, `--text-faint`) so light and dark themes remain consistent with zero shared-CSS changes outside `touches`.
- Inside the cell (when `m.resource` is present):
  - Stack two compact `.meter` bars vertically (`gap: var(--sp-1)`, `minWidth: 9.5rem`):
    1. **CPU bar**: `.meter__bar` + `.meter__fill` (`width: ${clampPct(cpu)}%`), `.meter__text` showing `CPU` on the left and `${Math.round(cpu)}%` on the right (`tabular-nums`).
    2. **MEM bar**: `.meter__bar` + `.meter__fill` (`width: ${clampPct(mem)}%`), `.meter__text` showing `MEM` on the left and `${Math.round(mem)}%` on the right (`tabular-nums`).
  - Color thresholds per bar:
    - `< 75%`: default `.meter` (`--meter-fill`)
    - `75% – 89.9%`: `.meter--warn` (`--warn`)
    - `>= 90%`: `.meter--danger` (`--danger`)
  - Tooltip (`title`) on the wrapper shows `Load/CPU ${load_per_cpu.toFixed(2)} · Egress ${(egress_mbps || 0).toFixed(1)} Mbps · Updated ${fmtAgo(at)}`.
  - If `m.state !== 'online'` while `m.resource` is still held in memory, render the cell at reduced emphasis (`opacity: 0.65`) and prefix the tooltip with `Last reported while online`.
- Absent / unreported state (`!m.resource`):
  - Render `<span className="faint">Unreported</span>`, matching `VersionCell`'s unreported indicator (`MachinesPage.jsx:235`).

---

## 3. Details Drawer (`MachineDetailsDrawer`)

### Placement
- Add a new `DetailResourceSection` card directly beside `<DetailDiskSection disk={machine.disk} />` (`MachinesPage.jsx:766`) inside the drawer's `.stack` layout.

### Visual Form
- Reuse the exact card + meter + definition-list structure of `DetailDiskSection` (`MachinesPage.jsx:113-178`):
  - **Card header**: `<h3>Resources</h3>` + status chip:
    - `!machine.resource`: `<span className="chip chip--outline">Unreported</span>`
    - `machine.state !== 'online'`: `<span className="chip chip--outline">Last known</span>`
    - `max(cpu, mem) >= 90`: `<span className="chip chip--danger">High usage</span>`
    - `max(cpu, mem) >= 75`: `<span className="chip chip--warn">Elevated</span>`
    - Otherwise: `<span className="chip chip--ok">Normal</span>`
  - **Card body (when `machine.resource` is present)**:
    - Two full-width `.meter` blocks with `role="progressbar"`, `aria-valuemin={0}`, `aria-valuemax={100}`, `aria-valuenow={Math.round(pct)}`, `aria-label`:
      - **CPU**: `${cpu.toFixed(1)}%`
      - **Memory**: `${mem.toFixed(1)}%`
    - `<dl style={DETAIL_DL_STYLE}>` with `DETAIL_DT_STYLE` / `DETAIL_DD_STYLE`:
      - `Load / CPU` -> `{load_per_cpu.toFixed(2)}` (`mono`)
      - `Egress` -> `{(egress_mbps || 0).toFixed(2)} Mbps` (`mono`)
      - `Updated` -> `{fmtAgo(at)} ({fmtDateTime(at)})`
  - **Card body (when `!machine.resource`)**:
    - `<div className="muted" style={{ fontSize: 'var(--fs-sm)' }}>Unreported — no live CPU or memory telemetry has been received from this agent.</div>`

---

## 4. Scope & Touches Check

- Implementation will modify only `web/src/pages/MachinesPage.jsx` plus the rebuilt SPA bundle (`web/assets/dist/`, `web/dist/index.html` via `./web/build.sh`), staying strictly within `touches`.
- No Watchdog or Monitor pages touched; no historical charts or time-series added.
