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

## Custom-image GitLab — undocumented per-IP rate limiting

Public docs (`https://kb.sitehost.nz/cloud-containers/custom-images/`)
present `git clone git@gitlab-clients.sitehost.co.nz:g_<id>/<code>.git`
as an ordinary git-over-SSH workflow. Reality, observed during
`examples/custom-image-smoke` and `examples/custom-image` validation
in May 2026 from a Philippines-based developer machine:

- A single, clean `git clone` from any IP (international or NZ-side)
  succeeds. The port-22 endpoint is **not** geo-blocked — earlier
  observations to the contrary turned out to be the rate-limit
  effect described next, not a geo restriction.
- After a small number of SSH attempts in quick succession (the
  smoke's original 6× retry loop reliably triggered it), the source
  IP gets temporarily blocked at the TCP layer — `nc -zv` against
  port 22 returns "connection refused" for several minutes. After a
  cooldown, connectivity returns. Behaviour is consistent with
  fail2ban-style per-IP banning at GitLab's edge.
- Port 443 (HTTPS) stays reachable throughout, suggesting the rate
  limit is SSH-specific rather than a generic per-IP TCP block.

The KB article closest to this concern
(`/cloud-containers/custom-images/access-via-ssh`) only documents
SSH-key-as-account-credential semantics. There is no mention of:

- the per-IP SSH rate limit / fail2ban behaviour,
- the recommended retry strategy (single-attempt, no aggressive
  loops),
- HTTPS-clone-with-PAT as a possible alternative.

**Open questions:**

- What's the actual ban threshold (failed attempts per minute, total
  per hour, etc.) and cooldown duration? Knowing this would let SDKs
  pick a safe retry policy.
- Is HTTPS git clone with a personal access token supported on
  port 443? A PAT-based path would let consumers iterate without
  the SSH-rate-limit risk.

**Mitigations adopted by gosh examples:**

1. **Single-attempt clone** (no retry loop). The smoke and
   custom-image examples take exactly one shot at the clone after a
   5-second wait for GitLab repo provisioning to settle.
2. **`JOURNEY_GIT_PROXY_JUMP`** env var injects `-o ProxyJump=...`
   into `GIT_SSH_COMMAND` so consumers can route git through a
   bastion. Useful for hopping through a SiteHost-network jump host
   when iterating heavily, but **not required for normal
   single-attempt use** from an international IP.
3. Failed-delete jobs are a downstream symptom of the same
   underlying state: if the GitLab project never accepted a first
   push, `cloud.image.Delete` sometimes returns the transient
   "could not delete" error and orphans the metadata record.
   `examples/probe-images` with `CLEANUP_IMAGE_PREFIX=` provides
   the manual recovery path; `cloud.image.DeleteAndWait` absorbs
   the transient automatically.

**Resolution paths:**

- KB / API docs add a "Network access" section to the custom-image
  pages mentioning the rate-limit behaviour and recommending the
  single-attempt pattern.
- If HTTPS clone is supported, document the auth flow (PAT
  generation, URL form) so SDKs can offer an HTTPS path.

---

## Per-CCS write-time resource gate (cause unconfirmed)

`cloud/stack/add.json` against an existing CCS sometimes returns:

```
Unable to update stack, the number of new images required exceeds
the number of available images on this server.
```

Observed in May 2026 against `ch-faraday` (product `905`), which
had 17 stacks deployed at the time. The gate is real and enforced
at write-time; what it *measures* is unclear from the public APIs:

- `cloud.server.List` exposes `images_remaining` and
  `containers_remaining` per CCS — both `0` for ch-faraday — but
  also `images_used=[]` (count 0). The two values don't add up,
  so the displayed quota state isn't a reliable read of the live
  cap.
- `server/list_resources` is account-level (VPS Disk + Memory only)
  — no per-CCS image cap visible. Notably, the account had
  `available_units=-100` for VPS Disk Space at the time of the
  failure, which could equally be the actual gate.

**Open question:** what does the "available images" check actually
measure — a per-CCS image-count cap, host disk space, container
count, or something else? The error message is ambiguous and the
read-side APIs don't surface enough state to disambiguate.

**Workaround used in `examples/custom-image`:** when
`JOURNEY_PROVISION_CCS=1` is set, the example provisions a fresh
CCS in AKLCITY (zero-cost staff region) just for the run, sidestepping
whatever resource cap exists on shared / loaded CCSes. CCS provision
+ teardown adds ~10 minutes total; reuse modes
(`JOURNEY_KEEP_CCS=1`, `JOURNEY_REUSE_IMAGE=...`) let consumers
amortise the cost across iterative runs.

**Resolution paths:**

- API surfaces a clearer error code and a precise per-CCS resource
  view (used vs cap for whatever the gate actually measures).
- If the gate is in fact image-count, the `cloud.server.List` view's
  `images_used` / `images_remaining` semantics get fixed so the
  values are usable for capacity planning.

---

## `cloud.image.Delete` rejects fresh / just-built images

`cloud/image/delete.json` returns success at the API layer
(`status:true`) but its scheduler job can come back as `Failed` with
the verbatim message:

```
We could not delete your custom image right now. Please contact
support@sitehost.co.nz
```

Empirically this is *transient*: the same delete call succeeds a
few seconds later. It appears to fire when a delete is issued too
soon after a build/push completes — e.g. immediately after a
fresh `cloud.image.Create` or right after a CI job ends. Likely a
GC / lock window on the platform side; the customer-facing error
points at support but no contact is actually needed.

**Operational impact:** Without retry, every fresh-image cleanup
fails on the first attempt and orphans the metadata record. AI
agents driving the SDK accumulate these orphans run-over-run with
no indication that the cleanup didn't actually take.

**Discoverability gap:** the error message is misleading — it
implies a permanent / support-needed condition, when in practice
a 10-second backoff and retry succeeds. The KB does not document
this behaviour; the API docs don't either.

**Mitigation in gosh:** `cloud.image.DeleteAndWait` (helper) wraps
`Delete` + scheduler-job polling, recognises the transient by
substring match on the job's Message field, and retries with
backoff up to N attempts. Examples should always cleanup via the
helper rather than bare `Delete`.

Tracked as a sharper actionable bug report at
[`docs/api-issues/cloud-image-delete-transient.md`](api-issues/cloud-image-delete-transient.md).

---

## PECL extension installs don't auto-enable on `sitehost-php*-apache`

When forking `sitehost-php85-apache` (and likely the other apache
PHP base images) and adding a PECL extension via the standard
incantation:

```dockerfile
RUN apt-get install -y libyaml-dev php-pear php-dev \
    && pecl install mailparse yaml \
    && phpenmod -v ALL mailparse yaml
```

…the `pecl install` step succeeds (`install ok: channel://
pecl.php.net/{mailparse,yaml}` appears in the build trace) and the
`.so` files land in `/usr/local/lib/php/extensions/`, but PECL
emits a warning the trace makes plain:

```
configuration option "php_ini" is not set to php.ini location
You should add "extension=mailparse.so" to php.ini
```

`phpenmod`'s own output is silent (no `Extension … enabled` line
appears) — suggesting it didn't find anything to enable, because
PECL installed under `/usr/local/...` rather than the system path
`phpenmod` scans (`/etc/php/<v>/mods-available/`). The end result
is that extension `.so` files are **present but not loaded at
runtime**.

**Operational impact:** A purely build-time check (e.g. trace
assertion that PECL install lines appear) would show a green
build, but `extension_loaded('mailparse')` at runtime would
return false. The customer / AI agent only finds out when their
application throws at runtime.

**Discoverability gap:** The KB's custom-images section explains
how to author a Dockerfile but doesn't document:

- Where the parent image's PHP install actually lives
  (`/usr/local` vs system `/etc/php/<v>/`).
- The right way to drop in an extension `.ini` so it's picked up
  by Apache + mod_php on first start.
- That `phpenmod -v ALL` is a no-op for PECL-installed extensions
  on these base images.

**Workaround (not yet validated by gosh, listed as candidates):**

- Write the `extension=<name>.so` line directly into the path PHP
  reads — likely `/usr/local/etc/php/conf.d/<name>.ini` (or wherever
  this base image expects per-extension config). A short `RUN echo
  "extension=mailparse.so" > <path>/mailparse.ini` after the PECL
  step, but the *correct* path depends on the parent image's
  layout.
- `pecl install -d php_ini=<path>` to point PECL at the right
  php.ini at install time so it auto-appends.
- A SiteHost-published Dockerfile snippet for adding extensions to
  each `sitehost-php*-apache` base, mirroring the pattern in the
  rest of the cloud-containers docs.

**Open questions:**

- Where exactly does PHP look for extension `.ini` files inside
  `sitehost-php85-apache:1.0.1-noble`? Confirming once would let
  the SDK / docs prescribe the canonical install pattern.
- Is there a SiteHost-supplied helper script inside the base image
  for installing PECL extensions correctly (analogous to the
  official PHP image's `docker-php-ext-enable`)?

**Validation status:** unverified at runtime. To confirm/refute,
deploy a stack from the smoke-built image and check `phpinfo()`
or `php -m` against the running container — the smoke's pecl mode
asserts only build-time install, not runtime loading. This is
exactly the gap that `examples/custom-image` (Phase 2: stack
deploy + phpinfo probe) is intended to close.

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
