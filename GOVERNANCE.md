# Governance

## Roles

- **Contributors**: anyone who opens an issue, PR, or review.
- **Maintainers**: can merge, cut releases, and triage security reports. They are listed below.

## Maintainers

| Name | Areas |
|---|---|
| _TBD before public launch_ | all |

## Decisions

- Day-to-day changes use **lazy consensus**: a PR approved by one maintainer, with no maintainer objection within 72 hours (or immediately for small fixes), may merge.
- **Design changes** (anything that alters SPEC.md, the report schema, the human-gate model, or the forge Provider interface) need an ADR in `docs/adr/` and approval from a majority of maintainers.
- Changes that **weaken a human gate** require unanimous maintainer approval.

## Becoming a maintainer

Sustained, high-quality contributions and reviews over roughly three months. An existing maintainer nominates you, and a majority approves.

## Releases

SemVer. The plugin version, the MCP binary version and the report schema version are released together. Any maintainer may cut a release from `main` by pushing a signed `vX.Y.Z` tag.
