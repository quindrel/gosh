# SRS — no per-TLD capability matrix; some endpoints reject by registry policy

**Filed:** 2026-05-04 (during examples/srs-tier1-writes validation)
**Status:** open

## Summary

SiteHost's `/srs/*` endpoints present a uniform interface for
domain operations across every TLD the platform supports. But
**many endpoints are silently TLD-specific** — they work on some
registries' domains and reject on others, with no upfront way to
know which.

There's no `/srs/tld_capabilities` (or similar) endpoint that
returns "for TLD `nz`, supported writes are X, Y, Z; unsupported
are A, B, C." Consumers discover the limitation only when the API
rejects an otherwise-well-formed call with a TLD-policy error.

## Confirmed example: `.nz` and lock/unlock

`/srs/lock_domain.json` and `/srs/unlock_domain.json` reject `.nz`
domains with:

> Error: This domain cannot be locked.

Because the .nz registry uses the **UDAI (transfer authorisation
code) model** rather than EPP-style transfer locks. Transfer
protection on .nz is achieved by withholding the UDAI, not by
locking. The API surface gives no upfront indication that lock
won't work on `.nz` — same wrapper, same call shape, different
TLD = different outcome.

Other TLD-specific behaviours are likely:
- Privacy protection support varies by registry.
- Contact-update rules differ (.nz has registrant-name change
  restrictions; .au has eligibility checks; etc.).
- UDAI / EPP-auth-code model differs between registries entirely.

## Why this matters

1. **AI agents can't pre-flight.** Without a capability matrix,
   an agent can't say "for .nz domains, here's what I can do."
   It has to attempt the op and parse a free-text error to learn.
2. **SDK ergonomics.** The Go wrappers on this package don't
   per-TLD-gate their methods (rightly — that knowledge belongs
   server-side), but the rejection pattern shows up identically
   regardless of cause: a string error message. SDKs can't
   distinguish "transient platform issue" from "this op never
   works on this TLD."
3. **Documentation gap.** The KB doesn't enumerate per-TLD
   capability differences in one place — consumers have to learn
   them by pinging the API.

## Workaround in gosh

Each known TLD-specific behaviour is documented in the affected
wrapper's doc-comment as an explicit warning. Currently:

  - `srs.LockDomain` / `srs.UnlockDomain` — `.nz` rejects.

As more rejections are observed during validation, they'll be
added to the relevant wrapper docs.

## Open questions for the API team

1. **Could the API expose a per-TLD capability matrix** —
   something like `/srs/tld_info?tld=nz` returning the supported
   ops list? Even a static document linked from the KB would
   help.
2. **Could the rejection error codes be structured** —
   distinguishing "TLD doesn't support this op" (permanent) from
   "domain in wrong state for this op" (transient) from "API
   internal error" (try again)?
3. **Is there an internal capability matrix** SiteHost maintains
   that could be exposed publicly without much work?

## Related KB / docs gaps

- `https://kb.sitehost.nz/domains/` covers the .nz UDAI process
  but doesn't cross-reference back to which `/srs` API endpoints
  are .nz-only or non-.nz-only.
- `https://docs.sitehost.nz/api/v1.5/?path=/srs/lock_domain` does
  not mention the .nz exclusion; example uses `example.com`
  implicitly assuming .com applicability.
