# `cloud.stack.Add` write-time resource gate — opaque error, ambiguous cause

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open, workaround in examples

## Summary

`cloud/stack/add.json` against an existing CCS sometimes returns:

> Unable to update stack, the number of new images required
> exceeds the number of available images on this server.

The gate is real and enforced at write-time — the call fails
deterministically once a CCS hits whatever cap is being checked.
What the gate actually *measures* is unclear from the public APIs:

- Per-CCS image count?
- Host disk space?
- Container count?
- Something else?

The error message names "available images" but the read-side APIs
don't surface enough state to confirm.

## Reproduction

May 2026, against `ch-faraday` (product code `905`), a CCS that
had 17 stacks deployed at the time of the failure.

`cloud.server.List` for the same CCS returns:

```
images_used=[]              ← count 0
images_remaining=0
containers_remaining=0
```

The two values don't add up — `used + remaining = 0` should mean
"empty CCS with 0 cap", but the CCS clearly has 17 running
stacks. So the displayed quota fields aren't a reliable read of
the live cap.

`server/list_resources` is account-level (only VPS Disk Space and
VPS Memory exposed). Notably, the account also had:

```
attribute_name: VPS Disk Space
available_units: -100      ← overcommitted by 100GB
```

…which could equally be the actual cause of the failure.

## Why this matters

1. **Consumers can't anticipate it.** With no usable read-side
   view of the cap, a write that's about to fail is
   indistinguishable from one that'll succeed until the
   server-side scheduler trips.
2. **Diagnosis is guesswork.** "Image count" was an early
   hypothesis, but nothing in the public APIs proves which gate
   actually fired. AI agents can't choose the right remediation
   without knowing.
3. **Quota fields displayed but unusable.** The
   `images_used` / `images_remaining` / `containers_remaining`
   fields on `cloud.server.List` look like capacity-planning
   inputs, but their values during this session were inconsistent
   with reality. Either the implementation is broken or the
   semantic is "informational, not authoritative" — which the
   API doesn't currently signal.

## Workaround in gosh

`examples/custom-image` accepts `JOURNEY_PROVISION_CCS=1` —
provisions a fresh CCS in `AKLCITY` (zero-cost staff region) just
for the run, sidestepping whatever resource cap exists on
shared / loaded CCSes. Reuse modes (`JOURNEY_KEEP_CCS=1`,
`JOURNEY_REUSE_IMAGE=...`) amortise the ~5min provision cost
across iterative runs.

## Open questions for the API team

1. **What does the gate actually measure?** Per-CCS image count,
   disk space, container count, or some composite? Knowing this
   lets SDKs report a meaningful "you can't do this because X"
   to the consumer.
2. **Could the API return a distinct error code** for each
   underlying cause, so consumers can branch on the right
   remediation (provision a new CCS, free disk, etc.)?
3. **Are the `images_used` / `images_remaining` /
   `containers_remaining` fields on `cloud.server.List`
   meaningful?** If yes, what's wrong with the values currently
   returned (used=0 + remaining=0 with 17 stacks deployed)? If
   no, should they be removed or marked deprecated?
4. **Could `cloud.server.List` (or a new endpoint) surface a
   reliable "remaining capacity for X type of write" view** so
   consumers can pre-flight their changes?

Happy to retest against any candidate fix.
