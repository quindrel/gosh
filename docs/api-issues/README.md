# API issues

Actionable bug reports against the SiteHost v1.5 API surfaced
during gosh SDK development. Each file is one issue, formatted as
if it were going to GitHub but kept in-repo so it travels with the
SDK and is visible to anyone reading the codebase.

Distinct from `docs/open-api-questions.md`, which captures the
broader narrative around discoverability gaps and design
questions.

## Workflow

- **New issue:** add a file `<short-slug>.md`. Lead with the
  one-line summary as the title; include filed-date, status,
  summary, repro, why-it-matters, gosh-side workaround (if any),
  and open questions for the API team.
- **Resolved:** update the file's `Status:` line to `resolved`,
  add a "Resolution" section at the bottom with the date and
  what changed (server-side fix, doc update, etc.). Keep the file
  for historical context — don't delete.
- **Tracking elsewhere:** if an issue gets opened in an internal
  SiteHost tracker (Linear, etc.) cross-link it in the file so
  the in-repo doc stays in sync.

## Current

- [`cloud-image-delete-transient.md`](./cloud-image-delete-transient.md) —
  `cloud.image.Delete` returns success at HTTP layer but the
  scheduler job fails with a misleading "contact support" message
  on freshly-built images. Self-resolving in ~10s.
