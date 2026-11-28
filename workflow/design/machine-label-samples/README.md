# Machine label samples (T-0072, round 2)

Static, standalone mock HTML. Not wired to the product; a decision aid only. Dark default.

Round 2 rebuilds the samples on the REAL panel design system (round 1 was rejected for ad-hoc classes, see `review.md`).
Each file inlines a verbatim copy of `web/assets/css/{tokens,base,components,live}.css`, the real SVG icon sprite, and the
three panel fonts as data URIs (no external fetch), and uses the real app shell (`app`, `sidebar`, `nav__link`, `topbar`,
`page-head`) and real components: `card` / `card__head` / `card__foot`, `card stat col-3`, `chip chip--ok|warn|danger|info|accent|outline|mono`,
`btn` / `btn--primary` / `btn--sm`, `icon-btn`, `table table--sticky-first` with `cell-primary` / `cell-sub`, `meter__bar/fill/text`,
`menu` / `menu__pop` / `menu__item`, `list` / `list__item` / `list__main`, `tabs` / `tab`, `term-ph`, `field` / `select`, `tip`, `alert`,
and the T-0059 details-drawer header + `card` / `dl` sections. No invented classes.

Shared mock data: **web-01 / web-02 / db-01** all `root` @ public `46.34.170.235`, private `10.0.0.5 / 10.0.0.9 / 10.0.0.21`
(the collision case), plus `ci-runner` and `mon-edge` as non-colliding controls.

Note: the live Registry already leads with the machine id. "Today" in sample 1 renders the username-primary identity from the
brief (`user@public-ip`) in the real row, to show the ambiguity the id-primary label fixes.

| File | Shows | Trade-offs |
|---|---|---|
| `01-id-primary.html` | Two real Registry tables: "Today: username-primary" (three identical `root@46.34.170.235` rows) vs "Proposed: id-primary" (id bold, `user @ public-ip` as `cell-sub`). | Simplest fix; relies on unique, set names; private IP (T-0070) already sits in Addresses. |
| `02-consistent-label.html` | One `name · priv · pub` label in 6 contexts: table row, target picker (select + open menu), details-drawer header, Maneuver agent list, terminal tab + title bar, compact chip. | Most consistent and learnable; longest strings (tabs/chips drop pub); repeated pub IP is noise when identical. |
| `03-label-variants.html` | MachineLabel variants in a real table: full / two-line / compact / chip / id-only + tooltip (`tip`), for trio + controls, plus the two-line variant in the real Registry row. | Choose density per context; id-only is cleanest but hides disambiguation until hover (no touch). |

Open the files directly in a browser.
