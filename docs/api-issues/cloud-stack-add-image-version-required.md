# `cloud.stack.Add` requires explicit image version tag — vague error

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open

## Summary

`cloud/stack/add.json` rejects compose bodies that reference a
custom image without an explicit version tag:

```yaml
image: 'registry-clients.sitehost.co.nz/g_<id>/<code>'
```

…with:

> There was no image version provided. Provide a valid image
> version

The error doesn't say where to source the version from, what
format it expects, or whether `:latest` would work. The compose
body's `image:` line looks like a normal Docker image reference
where omitting the tag implies `:latest`, but on SiteHost it's a
hard requirement.

## Reproduction

```yaml
# Rejected: no tag
image: 'registry-clients.sitehost.co.nz/g_979387/my-image'

# Accepted: explicit "1.0-<build_id>" tag
image: 'registry-clients.sitehost.co.nz/g_979387/my-image:1.0-26365'
```

The version comes from `cloud/image/version/list_all.json`
(per-image build history), in the `version` field of each
returned record. Pattern is `1.0-<build_id>`.

## Why this matters

1. **Docker convention violated silently.** Standard `docker
   pull` semantics treat omitted tags as `:latest`. Customers
   used to that convention have no warning that SiteHost doesn't.
2. **Error doesn't point at the fix.** Nothing in the message
   suggests "look at /cloud/image/version/list_all" or "use
   format 1.0-<build_id>". Consumers have to read separate API
   docs to find the version source.
3. **Repeated friction.** Every iteration on a stack's compose
   body has to look up the latest build's version tag and splice
   it in. Without a `:latest` shortcut, iteration is slower than
   it needs to be.

## Workaround in gosh

`examples/custom-image` queries `cloud.image.version.ListAll`
right after the build completes (or via the `WaitForBuild`
helper which returns the version), then formats the compose ref
as:

```go
fmt.Sprintf("registry-clients.sitehost.co.nz/g_%s/%s:%s",
    clientID, imageCode, imageVersion)
```

## Open questions for the API team

1. **Could the API support `:latest`** (or some equivalent) as
   shorthand for "the most recent successful build of this
   image"? Removes a per-iteration lookup.
2. **Could the error message** suggest the canonical version
   source — something like "image reference must include a
   version tag (see /cloud/image/version/list_all for available
   versions)"?
3. **Is the `1.0-<build_id>` format stable**, or could the
   major.minor prefix change in future? Affects whether SDKs
   should parse it or treat as opaque.
4. Could the API docs for `/cloud/stack/add` state the version
   tag requirement explicitly, with an example?
