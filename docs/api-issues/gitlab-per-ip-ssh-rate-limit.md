# `gitlab-clients.sitehost.co.nz:22` — undocumented per-IP SSH rate limiting

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open, mitigated in gosh

## Summary

The SSH endpoint for custom-image GitLab repositories
(`gitlab-clients.sitehost.co.nz:22`) is reachable from
international IPs and works for ordinary single-attempt git+ssh
clones. However, **a small number of retried SSH attempts from
the same source IP gets that IP TCP-refused on port 22 for
several minutes** (fail2ban-style banning at GitLab's edge).

Public docs don't mention this. The KB article closest to the
concern (`/cloud-containers/custom-images/access-via-ssh`) only
covers SSH-key-as-account-credential semantics, not source-IP
behaviour.

## Reproduction

From a Philippines source IP, May 2026:

```
$ nc -zv gitlab-clients.sitehost.co.nz 22
Connection ... succeeded!

$ # Now run something that does ~6 SSH attempts in <60s
$ # (e.g. an aggressive git-clone retry loop)

$ nc -zv gitlab-clients.sitehost.co.nz 22
Connection ... refused
$ # Stays refused for several minutes, then clears.
```

Port 443 (HTTPS) stays reachable throughout, suggesting the rate
limit is SSH-specific.

## Why this matters

1. **Misleading first failure.** A consumer hitting a transient
   SSH issue (DNS hiccup, momentary network blip) and reflexively
   retrying is the exact pattern that triggers the ban — turning
   a recoverable hiccup into a several-minutes outage.
2. **Mid-debugging confusion.** During this session the
   rate-limit hangover was initially misread as geo-blocking
   ("can't reach from outside NZ"), driving a chunk of wasted
   investigation into bastion hops that turned out to be
   unnecessary.
3. **No documented recovery.** Consumers who do trigger it have
   no way to know the cooldown is happening — `nc` just says
   "connection refused" indefinitely. There's no error code or
   header to distinguish "your IP is banned" from "the host is
   down".

## Workaround in gosh

`examples/custom-image-smoke` and `examples/custom-image` take
**exactly one shot at the clone** after a 5-second wait for the
GitLab repo to settle. No retry. Documented prominently in both
package comments.

`JOURNEY_GIT_PROXY_JUMP=user@host` env var injects
`-o ProxyJump=...` into `GIT_SSH_COMMAND` for consumers who get
banned and want to keep iterating from a bastion's IP rather than
waiting out the cooldown. Optional, not required for normal use.

## Open questions for the API team

1. **What's the actual ban threshold?** Failed attempts per
   minute, total per hour, etc.? Knowing this would let SDKs pick
   a safe automatic retry policy instead of the conservative
   "no retry ever" stance.
2. **What's the cooldown duration?** Empirically several minutes,
   but never measured precisely.
3. **Could GitLab return a distinct error** (HTTP code, banner
   text) when banning rather than silent TCP refused? Even just
   sending a TCP RST with a specific Connection: close pattern
   would let SDKs detect the state.
4. **Is HTTPS git clone with a personal access token supported?**
   Port 443 stays reachable; if HTTPS+PAT clone is supported,
   that's a path that bypasses the SSH rate limit entirely. Not
   documented in the KB.
5. Could the KB add a "Network access" section to the
   custom-image pages mentioning the rate-limit behaviour and
   recommending the single-attempt retry pattern?

Happy to test rate-limit thresholds against a sandbox if anyone on
the API/ops side wants concrete numbers.
