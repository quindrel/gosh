# `cloud.stack.Add` Label must be FQDN — misleading error message

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open

## Summary

`cloud/stack/add.json` accepts a `label` field that, by name,
sounds like a free-text human-readable label for the stack.
**Actually, the API requires `label` to be a valid FQDN** (the
hostname the stack will serve under).

If `label` isn't a valid hostname, the call fails with:

> Unable to add stack, the hostname is invalid.

…which doesn't mention `label` at all — leaving the consumer to
hunt for which "hostname" field is being rejected. It could
plausibly be `VIRTUAL_HOST` in the compose body, the `name`
field, or something else.

## Reproduction

```go
cloud.stack.Add({
  Name:  "ccXXXXXXXXXXXXXXXX",  // from cloud.stack.GenerateName
  Label: "gosh custom-image abc123",  // free-text — REJECTED
  ...
})
// → 200 Error: Unable to add stack, the hostname is invalid.

cloud.stack.Add({
  Name:  "ccXXXXXXXXXXXXXXXX",
  Label: "gosh.<ip>.sth.nz",  // FQDN — accepted
  ...
})
// → succeeds
```

`examples/build-a-site/main.go` (commit history) documents the
discovery the same way — the comment there reads:

> cloud.stack.Add's Label MUST be the FQDN; the API rejects
> non-FQDN labels with "Error: Unable to add stack, the hostname
> is invalid."

## Why this matters

1. **Field name implies wrong semantic.** "Label" is universally
   a free-text human-readable string in cloud APIs. Hijacking it
   to mean "primary FQDN" is surprising and not documented.
2. **Error message names the wrong thing.** The error talks
   about "hostname" but the offending field is `label`. This is
   the kind of error that wastes hours of debugging.
3. **Multiple plausible candidates.** A stack-add request has
   several hostname-like inputs (label, VIRTUAL_HOST in compose,
   nz.sitehost.container.website.vhosts label). Without the
   error pointing at the specific field, narrowing it down means
   trial-and-error.

## Workaround in gosh

Both `examples/build-a-site` and `examples/custom-image` set:

```go
Label: r.siteHost  // the FQDN, e.g. "gosh.<IP>.sth.nz"
```

…with an inline comment explaining the constraint.

## Open questions for the API team

1. **Could the error message be more specific?** Something like
   "label must be a valid FQDN; got '<value>'" would point the
   consumer straight at the field.
2. **Should the field be renamed?** `primary_hostname` or
   `vhost_primary` would match the actual semantic. (Aware
   that's a breaking change; could be done in v2.)
3. **Is there a separate label/display-name field planned**, or
   does SiteHost intend Label to remain hostname-shaped
   permanently?
4. Could the API docs for `/cloud/stack/add` explicitly state
   "label must be a valid FQDN" rather than leaving it as a
   trial-and-error discovery?
