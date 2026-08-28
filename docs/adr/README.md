# Architecture Decision Records

Append-only. To reverse a decision, add a new ADR that supersedes the old one and
mark the old one `Superseded by ADR-XXXX`. Never rewrite history — the value of an
ADR is knowing what was believed at the time and why.

Format: Context → Decision → Consequences → Alternatives rejected.

| # | Title | Status | Date |
| --- | --- | --- | --- |
| [0001](0001-go-for-the-core.md) | Go for the core | Accepted | 2026-08-27 |
| [0002](0002-floci-as-emulation-layer.md) | Floci as the emulation layer | Accepted | 2026-08-27 |
| [0003](0003-slice-from-seed.md) | Materialize a slice from a seed resource | Accepted | 2026-08-27 |
| [0004](0004-cli-first-with-optional-ui.md) | CLI-first, UI for what is inherently visual | Accepted | 2026-08-27 |
| [0005](0005-single-egress-chokepoint.md) | Single egress choke point (Echo Gateway) | Accepted | 2026-08-27 |
| [0006](0006-read-only-by-construction.md) | Read-only by construction | Accepted | 2026-08-27 |
| [0007](0007-allow-list-over-name-prefix.md) | An explicit allow-list, not a name prefix | Accepted · refines 0006 | 2026-08-27 |
