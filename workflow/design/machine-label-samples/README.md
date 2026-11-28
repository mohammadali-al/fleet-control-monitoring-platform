# Machine label samples (T-0072)

Static, standalone mock HTML (dark, round-5 look, inline CSS, no external fetch). Not wired to the product; a decision aid only.
Shared mock data: **web-01 / web-02 / db-01** all `root` @ public `46.34.170.235`, private `10.0.0.5 / 10.0.0.9 / 10.0.0.21` (the collision case), plus `ci-runner` and `mon-edge` as non-colliding controls.

| File | Shows | Trade-offs |
|---|---|---|
| `01-id-primary.html` | Machines list: today's username-primary (three identical `root` rows) beside id/name-primary with username and shared public IP demoted. | Simplest fix, solves the ambiguity; relies on names being unique and set; needs private IP (T-0070) to show the second line. |
| `02-consistent-label.html` | One `name · priv · pub` label in a table row, command-runner picker, details header, Maneuver agent list, terminal tab, chip. | Most consistent and learnable; longest strings (tabs/chips must drop pub); repeated pub IP is noise when all identical. |
| `03-label-variants.html` | MachineLabel variants: full, two-line, compact, chip, id-only + tooltip. | Lets the operator pick density per context; id-only is the cleanest but hides disambiguation until hover (no touch support). |

Open the files directly in a browser.
