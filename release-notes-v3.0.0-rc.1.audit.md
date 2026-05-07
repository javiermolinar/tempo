# Tempo v3.0.0-rc.1 release notes audit

Source: `CHANGELOG.md` sections `# v3.0.0-rc.1` and `# v3.0.0-rc.0`.
Draft: `release-notes-v3.0.0-rc.1.md`.

## Entry coverage

| Section | CHANGELOG bullets | Draft bullets |
|---|---:|---:|
| Breaking Changes | 23 | 23 |
| Changes | 20 | 20 |
| Features | 12 | 12 |
| Enhancements | 31 | 31 |
| Bugfixes | 34 | 34 |

- Missing changelog entries in draft: none
- Missing canonical PR references in draft: none
- Extra canonical PR references in draft: none

## Corrections applied while generating the draft

- Replaced CHANGELOG issue link `#6451` with PR link `#6532` for the block-builder block/WAL config bugfix.
- Replaced CHANGELOG issue link `#6558` with PR link `#6576` for the compactor deduped spans metric bugfix.
- Used GitHub PR authors instead of the following CHANGELOG author values:
  - PR(s) #6313: CHANGELOG `@oleg-kozlyuk`; GitHub `@oleg-kozlyuk-grafana`
  - PR(s) #6684: CHANGELOG `@cmarchbanks`; GitHub `@csmarchbanks`
  - PR(s) #6976: CHANGELOG `@xaque208`; GitHub `@zachfi`
  - PR(s) #6612: CHANGELOG `@bejaratommy`; GitHub `@ricardbejarano`

## Notes

- The draft intentionally excludes the generated GitHub dependency/doc/chore noise unless it already appears in the curated CHANGELOG release-note sections.
- The `New Contributors` section comes from GitHub generated release notes, not from `CHANGELOG.md`.
