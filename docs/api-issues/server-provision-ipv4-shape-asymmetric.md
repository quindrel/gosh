# `server.Create` — `params[ipv4]` shape differs between legacy Xen and modern KVM products

**Filed:** 2026-05-03 (during examples/vps-compare validation)
**Status:** open

## Summary

The `params[ipv4]` field on `/server/provision.json` accepts the
sentinel string `"auto"` for platform-allocated IPs — but the
**form** the field takes differs between product families:

- **Modern KVM products (LHPVS family, etc.)** require the array
  form `params[ipv4][0]=auto`. The scalar form
  `params[ipv4]=auto` is rejected.
- **Legacy Xen products (XENPRO, etc.)** require the scalar form
  `params[ipv4]=auto`. The array forms
  `params[ipv4][0]=auto` and `params[ipv4][]=auto` are rejected
  with "The ip address is invalid, please specify a valid ip
  address."

Same client_id, same location (AKLCITY), same image
(ubuntu-noble-pvh.amd64) — only the product code changes, and the
required wire shape flips.

## Reproduction

Against client_id=979387, May 2026:

```
# Modern KVM — array form works:
POST /server/provision.json
  product_code=LHPVS1
  params[ipv4][0]=auto
→ status:true

# Legacy Xen — scalar form works:
POST /server/provision.json
  product_code=XENPRO
  params[ipv4]=auto
→ status:true

# Crossing the wires fails for both:
POST /server/provision.json
  product_code=XENPRO
  params[ipv4][0]=auto
→ "Error: The ip address is invalid, please specify a valid ip address"

POST /server/provision.json
  product_code=LHPVS1
  params[ipv4]=auto
→ (untested but presumed to fail symmetrically)
```

## Why this matters

1. **SDK ergonomics.** A typed wrapper (`ParamsOptions.IPv4
   []string`) naturally serialises as an array — the modern shape
   — and silently breaks against legacy products. Consumers
   calling the wrapper for a XENPRO provision get an unhelpful "ip
   address is invalid" error and have no way to fix it without
   bypassing the wrapper.
2. **Inconsistent contract.** Same parameter name, same semantic
   value (`"auto"`), but two different wire shapes depending on
   the product code. The product-code-aware behaviour isn't
   documented.

## Workaround in gosh

`pkg/api/server.ParamsOptions.IPv4` is `[]string` and always
serialises as the array form. Consumers needing legacy-Xen
provisions currently bypass the wrapper with a raw POST that
includes `params[ipv4]=auto`. See
`examples/vps-compare` (and its companions
`examples/vps-standard` / `examples/vps-high-performance`) for
the empirical observation; the wrapper change is pending.

## Possible wrapper fixes (not yet implemented)

1. **Detect at serialisation time.** If `len(IPv4) == 1 &&
   IPv4[0] == "auto"`, drop the `[0]` index. Side-effect:
   modern products accepting a scalar `auto` (if any) would now
   receive scalar; need to verify modern still accepts.
2. **New ParamsOptions.LegacyIPv4Auto bool.** Explicit toggle the
   caller flips for legacy products. Slightly verbose, no
   behavioural ambiguity.
3. **New ProductFamily field.** Caller passes a known family
   value; wrapper picks the right shape. More structured but
   requires keeping a list of legacy product codes.

## Open questions for the API team

1. **Why does the wire shape differ?** Both behaviours look
   intentional (each product family rejects the other shape with
   a clear error) but the asymmetry isn't documented and forces
   SDKs into product-code-aware serialisation.
2. **Could legacy products be taught to accept the array form?**
   Even just supporting both shapes on every product would let
   SDKs use one canonical form everywhere.
3. **Could the public docs include the per-family shape table?**
   The current example shows `params[ipv4][0]=x` only.
4. **Is the legacy-Xen family being deprecated** such that this
   asymmetry will resolve itself by attrition? If so, capping
   the wrapper at modern KVM may be acceptable.
