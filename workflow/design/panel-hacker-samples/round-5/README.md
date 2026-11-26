# Fleet Panel — Hacker Restyle Samples, Round 5 (T-0034)

Four all-in-one standalone HTML samples on the fixed **Round-3 unified design system** (`Space Mono` + `Red Hat Mono`, carbon-ink / graphite-bone box outlines + offset block-shadow frames, and both warm-paper **Light** and void-black + bone-white + signal-red **Dark** palettes), refining the Matrix-rain sample and exploring restrained creative/professional backgrounds in place of plain dots/grids.

Every file is **all-in-one**:
- **In-page Light / Dark toggle** via the topbar `.theme-toggle` button.
- **Browsable panel sections** via the sidebar navigation (`Machines`, `Projects & tests`, `Command runner`, `Terminal`, `Baseline items`, `Maneuver`, `Monitor data`, `Watchdog`, `Evidence`, `Backup`), plus working tabs, filters, dialogs, and mock toast feedback.

## Samples

1. **[01-matrix-rain.html](01-matrix-rain.html) — Refined Monochrome Matrix Rain** *(live `<canvas>` animated background)*:
   Retains Round 4's falling katakana/hex code rain behind the shell (`z-index: 0`), refined so **dark-mode rain is muted graphite/bone monochrome** (`#dcdce4` heads, `#6e6e7a` trails on `#09090b`, no green) and light-mode rain uses matching **carbon-ink / warm-slate** (`#181a18` / `#5c5950` on `#f3efe6`). Low opacity (`0.13` light / `0.18` dark), full legibility, and `prefers-reduced-motion` respected (including clean static redraw on theme toggle).
2. **[02-topographic-contours.html](02-topographic-contours.html) — Topographic Elevation Contours** *(static seamless SVG background)*:
   Seamless `480×480px` periodic cartographic contour map with layered primary index isolines, finer intermediate contours, and sparse survey elevation crosshairs (`EL 420m`, `EL 185m`) in low-opacity carbon ink (light) and muted bone-grey (dark).
3. **[03-technical-blueprint.html](03-technical-blueprint.html) — Faint Technical Blueprint** *(static SVG schematic background)*:
   Restrained `480×360px` engineering drafting sheet featuring corner datum crop marks, dashed construction axes, circular target reticles, chamfered node frames, dimension ticks (`Ø 88.0 MM`), and bus annotations (`BUS 10.20.0.0/24`) in muted slate-blue/ink (light) and blueprint slate-silver (dark).
4. **[04-constellation-mesh.html](04-constellation-mesh.html) — Slow Constellation Mesh Field** *(live `<canvas>` animated background)*:
   Calm, slow-drifting field of crisp square node markers connected by distance-attenuated hairline proximity links, with sparse accent-tinted telemetry nodes (signal-orange light / signal-red dark) at subtle opacity; respects `prefers-reduced-motion`.
