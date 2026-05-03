# `cloud.image.Delete`: scheduler job fails with misleading "contact support" message on freshly-built images

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open, workaround in gosh

## Summary

`cloud/image/delete.json` returns success at the HTTP layer
(`status:true`) but the scheduler job it queues comes back as
`Failed` with the verbatim message:

> We could not delete your custom image right now. Please contact
> support@sitehost.co.nz

Empirically observed to be **transient** — the same delete
succeeds within ~10 seconds on retry. Appears to fire when delete
is issued shortly after a build completes / the image's GitLab
repo has been pushed to. Likely a GC or lock window on the
platform side; the customer-facing message points at support but
no contact appears to be needed.

## Reproduction

Repeatedly via `examples/custom-image-smoke` (echo and pecl modes)
and `examples/custom-image` against a fresh fork. Every fresh
image's first delete attempt failed; second attempt within ~10
seconds succeeded. See session work in `feat/examples` — commits
`77b108e` (helper) and `f31a904` (corrected docs).

## Why this matters

1. **Misleading error message.** "Please contact support"
   suggests a permanent, support-needed condition. In practice
   it's self-resolving and no support contact is needed.
2. **Silent orphaning.** Without retry, every fresh-image cleanup
   leaves an orphan record. AI agents driving the SDK accumulate
   them run-over-run with no indication anything's wrong (the
   API-layer call returned success).
3. **Substring-matching workaround.** The transient is only
   detectable by parsing the user-facing English in the job's
   `Message` field — a fragile contract for SDKs.

## Workaround in gosh

`cloud.image.DeleteAndWait` (in `pkg/api/cloud/image/deleteandwait.go`)
wraps `Delete` + scheduler-job polling, recognises the transient by
substring match on the job's `Message` field, and retries with
backoff up to N attempts. Examples cleanup via the helper rather
than bare `Delete`.

## Open questions for the API team

1. **Is the rejection always transient?** Or are there states
   (e.g. image-in-use-by-running-container) where the same message
   indicates a genuine terminal failure that retry will mask?
2. **Could the API surface a distinct error code** for the
   transient vs the terminal cases, so consumers don't have to
   substring-match on user-facing English?
3. **Could the message itself be updated** to remove the
   misleading "contact support" guidance for the transient case —
   e.g. "Image is locked while a recent build settles, retry in a
   few seconds" or similar?
4. Is there a server-side fix to either avoid the transient
   condition entirely (block delete until the lock window passes)
   or fast-fail with a clearer code?

Happy to test any candidate fix.
