# Open API questions / discoverability gaps

Things gosh's authors and consumers can't currently find via the
public API or KB, surfaced during examples-driven validation. Each
entry should resolve into either a documented endpoint, a wrapper
addition, or an explicit "this is internal-only — pass via env."

## CCS image catalogue

`server.ListImages` (which calls `server/list_images.json`) returns
only older `ubuntu-<release>.amd64.cloud` salt-container variants
(focal, xenial, trusty). The provision-able CCS image used by the
platform today is named differently — e.g. `ubuntu-cc-2404-20260323`
— and **does not appear** in the `list_images` response.

Discovered while building `examples/probe-tls-default/main.go`:
provision attempts with `ubuntu-focal.amd64.cloud(.3002-2)` failed
with the platform-internal error *"The image
'ubuntu-focal.amd64.cloud.3002-2' could not be found."* The
working image code was provided by the user from internal
scheduler-table lookup; no public API surfaced it.

**Open question:** what endpoint (if any) returns the current CCS
image codes the way customers / AI agents discover them?

Until answered, examples that provision a CCS hardcode the image
code (with a TODO referencing this doc) and accept an env-var
override for callers who know the current value.

## CCS provisioning vs. VPS provisioning

`server/provision.json` accepts both VPS product codes (XENLIT etc.)
and CCS product codes (CLDCON*-P) plus an image code. Whether the
combination produces a VPS or a CCS depends on the (image,
product_code) pair — but no public docs enumerate which combinations
are valid. Combinations that are syntactically valid but mismatched
(e.g. CCS image with VPS product) don't get rejected at validation;
they fail at the scheduler stage with an opaque error.

**Open question:** is there a public reference for valid (image,
product_code) combinations, or is the `cloud-server-provision`
flow expected to live behind a different endpoint that isn't
documented?

## Server.Delete force option

`server/delete.json` rejects deletion of a CCS that has containers,
databases, or users present (i.e. any fresh CCS, since infra
auto-deploys). The fix is `force_delete=1`, but the param name
isn't documented anywhere I can find — discovered empirically by
trying variants. The DeleteRequest.Force field on gosh now passes
this; doc.go should document it explicitly.

## CCS minimum-TLS-version read

`cloud/server/update_minimum_tls_version.json` writes the value;
no read endpoint exists. To learn the current setting, you must
either:
1. Provision a fresh CCS and observe what TLS versions its
   nginx-proxy actually negotiates (what
   `examples/probe-tls-default/main.go` does), or
2. Track the value in your own state when you set it.

**Open question:** is there a `get_minimum_tls_version` endpoint
or equivalent that surfaces the current value? If not, is one
planned, or should consumers always treat this field as
"set-only / observe-via-handshake"?

---

## Process

When something else surfaces while writing an example or wrapper,
add it here with the section format above. Resolution paths:

- Endpoint exists but isn't in gosh → file a `feat/<wrapper>`
  branch, ship the wrapper, drop the section.
- Endpoint doesn't exist publicly → escalate to whoever owns
  that part of the API; capture the resolution here when known.
- Internal-only knowledge → document the env-var / config
  pattern consumers should use, leave the section as "documented
  workaround."
