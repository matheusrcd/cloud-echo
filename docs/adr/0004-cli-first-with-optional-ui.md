# ADR-0004 — CLI-first, UI for what is inherently visual

**Status:** Accepted · **Date:** 2026-08-27

## Context

Two of the product's requirements are inherently visual: understanding a
dependency graph, and editing mock responses while watching live traffic. Most of
the rest — scan, plan, up, down, logs — is better as a command.

## Decision

The CLI is the complete interface. Every capability is reachable from it and from
the control API. The web UI (`cloud-echo ui`) is optional and covers only what is
genuinely better visually:

- the dependency graph, with evidence on click and edge editing
- the integrations panel and live traffic timeline
- building a mock rule from an observed request

**Constraint: the UI is a client of the same control API the CLI uses.** No
capability may exist only in the UI. If a feature can't be expressed in the API,
it isn't designed yet.

## Consequences

**Good**
- Scriptable and CI-ready from day one without a second implementation.
- The API-first constraint keeps the architecture honest — UI-only state is how
  tools become undebuggable.
- The UI can ship late (M5) without blocking anything.
- Users who live in a terminal never pay for the UI; users who want a map get one.
- Remote/devcontainer setups work, since the UI is served over HTTP.

**Bad**
- Some things are clumsy in a CLI (`cloud-echo mock` rule editing is not going to
  be pleasant). Accepted: the UI covers it, and rules are editable as YAML.
- Two surfaces to keep in sync. Mitigated by the UI having no independent logic.
- Graph output in a terminal is limited. Mitigated by exporting DOT/Mermaid, which
  is arguably better anyway — it pastes into a PR.

## Alternatives rejected

- **CLI only** — smallest surface and fastest to ship, but "see every integration
  and edit responses in real time" degrades to hand-editing YAML and restarting.
  That's the feature people would come for.
- **UI-first** — better onboarding and a better demo, but roughly doubles v1,
  blocks CI adoption, and invites UI-only state.
