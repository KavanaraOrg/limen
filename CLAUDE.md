# limen

Context and identity per directory — a Go binary with a descriptor
(`.limen/limen.yaml`), a registry and rolling notes as an anchor point for
agents.

## Home and identity (KavanaraOrg)

- GitHub home: https://github.com/KavanaraOrg/limen — organisation
  **KavanaraOrg** (formerly LevaraOrg, before that `matthiaw` until 2026-08-18;
  old URLs redirect).
- The working identity of this directory comes from `.limen/limen.yaml` (tool:
  limen): githubUser `kavanaraleo`, actor Matthias, Claude config
  `~/.claude-kavanara`.

## Inherited norms

This directory inherits `kavanara-baseline@latest` (see `.limen/meta.yaml`). The
skills are materialised in `.claude/skills/`, the reasoning as ADRs in
`docs/adr/`. In short:

- **ADR-0001** — everything committed is English: code, comments, documentation,
  commit messages, test names, CLI output and error strings. Conversation with
  the user stays in whatever language they write. The same default is
  machine-readable through limen itself: `limen show`, `limen json` and
  `limen list --json` report a `language` field, `english` unless a project
  declares otherwise via `language:` in `.limen/meta.yaml` — ask limen rather
  than guessing the working language.
- **ADR-0002** — test-driven: the failing test first, then the least code that
  makes it pass. Bug fixes are never exempt.
- **ADR-0003** — token-frugal: the cheapest tool that answers the question,
  never at the cost of completeness.
- **ADR-0004** — a 75% line-coverage floor per module (a ratchet where not yet
  met) and a bug-pattern analyzer over the sources.
- **ADR-0005** — dependency, secret and security static-analysis scans gate
  every build; a red-team pass before every release that changes a trust
  boundary.
- **ADR-0006** — large projects (services, several deployable modules) build,
  start and smoke-test as containers in CI. limen is a single binary and is not
  large, so this one does not bind here.
- **ADR-0007** — services work headless: every capability reachable through the
  public interface without the frontend. limen is a CLI with no frontend, so
  this one does not bind here either.
- **ADR-0008** — every build produces a CycloneDX SBOM, and a licence gate fails
  on any component that does not allow commercial use. limen's own licence is
  AGPL-3.0 (copyright Kavanara GmbH) instead of the Apache-2.0 default; the
  decision is recorded in `docs/decisions/0001-agpl-3.0.md`.
- **ADR-0009** — every credential lives in Bitwarden Secrets Manager, one
  project per repository named like the repository; agents refer to a secret
  by its key and never read or print its value.
- **ADR-0010** — AI agents are not co-authors: no `Co-Authored-By:` trailer for
  Claude or any other agent, no "Generated with Claude Code" line in commits,
  pull requests, issues or release notes.

`limen profile check` proves the copies are unchanged — exit 1 on drift. Do not
edit the files under `.claude/skills/` or `docs/adr/` by hand: `limen profile
sync` overwrites them. A norm is changed in the package
(`~/Documents/GitHub/kavanara-baseline`) and its version raised.

Note that limen's own user-facing output is English too, deliberately. ADR-0001
covers error strings, and a tool with an English README and German error
messages would be the mixed state the norm exists to prevent.
