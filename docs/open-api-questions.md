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

## Volume name length limit

`cloud/volume/add.json` rejects volume names with the message
*"Please specify a volume name with only letters, numbers, hyphens
and underscores, with at least 3 characters"* — but this also
fires for names that DO contain only those characters and are well
above 3 characters. The actual constraint appears to be an
undocumented max-length (around ~16 chars empirically). The error
message is misleading.

**Open question:** what is the documented max length for volume
names? Should the API's error message include it explicitly? The
current "at least 3" wording leads consumers to assume there's no
max.

`examples/cloud-volume` works around with `goshv<8hex>` (13 chars).

## Mail forward destination domain validation

`mail/add_forward.json` rejects `.example` and `.test` TLD destinations
with *"Please specify a valid destination email address"* — even
though RFC 2606 reserves these explicitly for documentation/test use.
`example.com` is accepted (also reserved for documentation but on
allowlists). `examples/mail` uses `example.com` as a workaround.

**Open question:** should the API allowlist the RFC 2606 reserved
TLDs (`.example`, `.test`, `.invalid`, `.localhost`) since these are
specifically for examples?

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

## Deferred wrappers (intentionally not implemented)

Endpoints we've deliberately not wrapped because their value to
gosh consumers is unclear from the public docs. Listed here so the
gap is visible — if a use case surfaces, promote to a `feat/`
branch.

### `server/generate_vnc_token`

Returns `{"return": {"token": "xxx..."}}` given `client_id`,
`name`, `remote_ip`. The docs do not state **where to connect** —
no VNC host, no port, no websockify/noVNC URL, no protocol
guidance. A token without a connection target isn't actionable
from an SDK consumer's perspective.

**Resolution path:** if the connection target is documented (or a
companion endpoint exists that returns it), wrap both together.
Until then, deferred.

---

## Custom-image GitLab — undocumented network restrictions

Public docs (`https://kb.sitehost.nz/cloud-containers/custom-images/`)
present `git clone git@gitlab-clients.sitehost.co.nz:g_<id>/<code>.git`
as an ordinary git-over-SSH workflow. Reality, observed during
`examples/custom-image-smoke` validation in May 2026:

- Direct connections from international IPs (Philippines source, in
  this case) to `gitlab-clients.sitehost.co.nz:22` get TCP "connection
  refused" — the port is not reachable at all from outside the
  SiteHost / NZ network.
- From a SiteHost cloud container in NZ (verified against both
  `45.113.8.110` and `223.165.71.164`), `nc -zv` to port 22 succeeds
  on the *first* probe but subsequent SSH attempts return "connection
  refused" at the TCP layer for several minutes. Behaviour is
  consistent with per-source-IP rate limiting / fail2ban-style
  banning at GitLab's edge.
- Port 443 (HTTPS) stays reachable from international IPs throughout.
- The git-over-SSH retry loop (e.g. `examples/custom-image-smoke`
  trying 6× with 10s spacing) appears to *cause* the ban rather than
  recover from it; first-attempt failures should not be retried
  aggressively.

The KB article that's closest to this concern
(`/cloud-containers/custom-images/access-via-ssh`) only documents
SSH-key-as-account-credential semantics. There is no mention of:

- source-IP allow-listing requirements,
- rate-limiting / fail2ban behaviour,
- HTTPS-clone-with-PAT as a possible alternative,
- or that running from outside NZ may need operator support.

**Open question:** what is the supported way to reach
`gitlab-clients.sitehost.co.nz:22` from an external developer
machine or CI runner that doesn't live in the SiteHost network?
Is HTTPS clone with a personal access token supported on port 443?

**Operational impact for AI agents:** this is the #1 blocker for
SDK consumers driving `cloud.image` workflows from outside the NZ
network. Until clarified, examples should:

1. Document the constraint prominently (see
   `examples/custom-image-smoke` package comment).
2. Default the retry loop to single-attempt with long backoff
   rather than aggressive retries that worsen the problem.
3. Surface a `JOURNEY_GIT_PROXY_JUMP` env var so consumers with
   bastion access can route through it without SDK changes.
4. Flag failed-delete jobs as a downstream symptom: if the GitLab
   project never accepted a first push, `cloud.image.Delete`
   sometimes returns `"We could not delete your custom image right
   now"` and orphans the metadata record. `examples/probe-images`
   with `CLEANUP_IMAGE_PREFIX=` provides the manual recovery path.

**Resolution paths:**

- SiteHost ops whitelists the consumer's IP / IP range against the
  GitLab edge firewall (best for individual consumers).
- KB / API docs add a "Network access" section to the custom-image
  pages capturing whichever option above is supported.
- If HTTPS clone is supported, document the auth flow (PAT
  generation, header format) so SDKs can offer an HTTPS path that
  doesn't depend on SSH allow-listing.

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
