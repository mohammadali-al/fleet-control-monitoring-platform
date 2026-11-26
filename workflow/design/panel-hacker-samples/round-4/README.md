# Fleet Panel — Hacker Restyle Samples, Round 4 (T-0034)

Five all-in-one standalone HTML samples exploring **background patterns only** on top of the fixed **Round-3 unified design system** (`Space Mono` + `Red Hat Mono`, carbon-ink / graphite-bone box outlines + offset block-shadow frames, and both the warm-paper **Light** and void-black + bone-white + signal-red **Dark** palettes).

Every file is **all-in-one**:
- **In-page Light / Dark toggle** via the topbar theme button (reusing the panel's existing `.theme-toggle` button).
- **Browsable panel sections** via the sidebar navigation (`Machines`, `Projects & tests`, `Command runner`, `Terminal`, `Baseline items`, `Maneuver`, `Monitor data`, `Watchdog`, `Evidence`, `Backup`), plus working tabs, filters, disclosure panels, and mock toast feedback.

## Samples

1. **[01-revised-dots.html](01-revised-dots.html) — Revised Calibrated Dots** *(revised dot pattern)*:
   Replaces round-2/3's corner-clipped `20px` CSS radial-gradient dots with a crisp `64×64px` inline SVG tile aligned to the `1rem` (`16px`) layout grid (`r=0.75px` centered minor dots every `16px`, `r=1.35px` major registration dot every `64px`), with tuned ink/bone contrast in both themes.
2. **[02-drafting-grid.html](02-drafting-grid.html) — Fine Drafting / Graph-Paper Grid** *(static pattern)*:
   Crisp `16px` (`1rem`) minor hairlines paired with `80px` (`5rem`) major subdivision rules for an architectural engineering sheet feel.
3. **[03-crosshatch.html](03-crosshatch.html) — Diagonal Crosshatch** *(static pattern)*:
   Delicate `45° / -45°` hairline crosshatch at `14px` pitch, giving the canvas a subtle engraved drafting-vellum texture.
4. **[04-hex-circuit.html](04-hex-circuit.html) — Faint Hex-Dump & PCB Circuit Trace** *(hacker-style static background)*:
   Low-opacity `320×240px` repeating SVG texture combining faint memory-offset hex-dump lines (`0x0040`, `SYN/ACK :6789`, `mTLS SHA256`) with 45°-chamfered circuit traces and solder vias behind the cards.
5. **[05-matrix-rain.html](05-matrix-rain.html) — Matrix Live Falling Code Rain** *(live animated `<canvas>` background)*:
   Green falling katakana/hex code rain rendered via `requestAnimationFrame` on a fixed full-viewport `<canvas>` behind the shell (`z-index: 0`) at low opacity (`0.16` light / `0.22` dark) so every card, table, and control remains fully legible in both themes (respects `prefers-reduced-motion`).
