# AGENTS.md

This file provides engineering conventions for agents and collaborators working in the Harbor repository.

## Design and Implementation Standards

**Core standard: do only the "most reasonable" design required by the current problem—neither reserve abstractions that won't be used (over-engineering), nor leave a temporary/simplified version that is bound to be reworked just to save effort. Both are wrong.**

- "Minimal necessary" ≠ "simplified/basic version". A solution whose constraints are just enough and that directly solves the current problem is the correct one; do not disparage it as "simple/basic".
- Before introducing an abstraction or framework, you must be able to point to a **currently existing** requirement, constraint, or failure that has already occurred as justification; do not build mechanisms in advance for a "hypothetical future".
- Evolve incrementally as needed: for example, when a certain CLI version is incompatible with existing code, add a compatibility branch in the corresponding adapter **keyed by version number**, rather than pre-building a variants/constraints framework.
- Use neutral language in review and communication; do not call a reasonable design "simplified/temporary".

## Communication and Wording Standards

Wording should make the reader **trust that the implementation is reliable**; do not use phrasing that makes the author appear unsure of themselves.

- When stating a correct decision, **state the conclusion directly and give the justification**, without hedging words (such as "currently sufficient/temporary/for now/basic version").
- When a real limitation exists, **state it precisely and locally**; do not magnify it into a deficiency of overall capability.
- Discuss extension or evolution only **when there is a concrete triggering condition**; do not pre-list "possible future upgrades" to imply a current shortcoming.
- When genuinely uncertain, explicitly give the **uncertain point and the way to verify it**, rather than leaving it vaguely open.
- Do not use "simple/basic/simplified" to describe a design that is already reasonable.

## Code Quality Conventions

- **Code comments are always in English** (identifiers, comments, and commit messages are externally readable).
- A single line must not exceed **120** characters (enforced by `golangci-lint`'s `lll`).
- Function decomposition prioritizes **readability**: even if the total complexity cannot be reduced, split it into smaller functions.
- Comments must be **self-contained** and must not reference external design documents; naming and comment semantics must be consistent.
- Eliminate duplicated code; name interfaces and types clearly and in a way that fits their semantics.

## Build and Checks

- `make build`, `make vet`, `make test` (with `-race`), `make lint`.
- `golangci-lint` must be built by **Go ≥ the version of the module's `go` directive**; a binary of a lower version will refuse to run or crash.

## Git: do not commit automatically

**Never commit automatically.** Run `git commit` only when the current request explicitly says to commit; otherwise leave the changes uncommitted (staged or unstaged) for review. Never `git push` unless explicitly asked.
