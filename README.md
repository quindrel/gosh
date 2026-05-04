# Gosh

Gosh is a Go client library for accessing the [SiteHost v1.5 API](https://docs.sitehost.nz/api/v1.5/).

## Installation

```sh
go get -u https://github.com/sitehostnz/gosh
```

## Example

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/server"
)

func main() {
	apiKey := os.Getenv("SH_API_KEY")
	clientId := os.Getenv("SH_CLIENT_ID")

	client := api.NewClient(apiKey, clientId)
	ctx := context.Background()

	instance := server.New(client)

	opts := server.CreateRequest{
		Label:       "goshserver",
		Location:    "AKLCITY",
		ProductCode: "XENLIT",
		Image:       "ubuntu-jammy-pvh.amd64",
		Params: server.ParamsOptions{
			SSHKeys: []string{"ssh-rsa ..."},
		},
	}

	server, err := instance.Create(ctx, opts)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%v", server)
}
```

## Documentation

The structure of this library closely mirrors that of our API, so the
[API documentation](https://docs.sitehost.nz/api/v1.5/) should be your first
point of reference.

## Contributing

If you're interested in contributing to our project:
- Start by reading our [style guide](https://github.com/sitehostnz/go-style-guide/blob/master/style.md) and [contributing guide](/docs/CONTRIBUTING.md).
- Explore our [issues](https://github.com/sitehostnz/gosh/issues).
- Or send us feature PRs.

## Currently in flight

This section is **transient** — it lists feat branches awaiting
upstream review and the recommended merge order. Drop the section
once everything below is merged.

Each branch is a single-squashed-commit off `main`. Where multiple
branches edited the same package, they've been **consolidated into
per-namespace branches** (e.g. all the cloud/* work lives on
`feat/cloud`; all the server/* work on `feat/server`) so reviewers
see one PR per package surface instead of fragmented per-feature
PRs.

The order below is reviewer-friendly: smallest / lowest-risk first.

### Batch 1 — trivial cleanups (review in minutes)

| Branch | LoC | Surface |
|---|---|---|
| `feat/comment-fixups` | 5 | Doc-comment endpoint-path typos (image package + models) |
| `feat/job-message-field` | 10 | Adds `Message` field to `models.JobDetails` |

### Batch 2 — small additive PRs (review in 5–15 min)

| Branch | LoC | Surface |
|---|---|---|
| `feat/info-discover-client-id` | 154 | New helper `info.NewClientWithDiscovery` — bootstrap `*api.Client` from just an API key |
| `feat/accounts` | 248 | New top-level namespace; 1 endpoint (`accounts/client/list_sub_accounts`) — sub-account discovery for resellers |
| `feat/redirect` | 277 | New top-level namespace; 1 endpoint (`redirect/list_redirects`) with custom UnmarshalJSON to tolerate the API's empty-result shape (`[]` not `{}`) |
| `feat/misc-fillins` | 290 | Small filler set across existing packages |

### Batch 3 — single-package additions

| Branch | LoC | Surface |
|---|---|---|
| `feat/ssl` | 413 | 3 read endpoints, new `pkg/api/ssl` package (writes deferred) |
| `feat/bandwidth-usage` | 526 | 5 read endpoints (per-day/month/year/summary + IP list), backfilled tests |

### Batch 4 — consolidated per-namespace surfaces

These branches each fold the full per-namespace work — typically
several earlier feat branches' worth — into one squash commit.
Larger review surface, but each one lands a coherent package end
to end.

| Branch | LoC | Surface |
|---|---|---|
| `feat/dns` | 2,104 | Reads + writes + records + zones + domain_templates sub-package (14 endpoints there alone) + reverse-DNS + SOA. Folds 4 prior feat branches. |
| `feat/server` | 2,307 | Full server lifecycle (Create / Get / List / Update / Delete with `force_delete=1` support / Upgrade / UpgradeComponents / state ops) + snapshots sub-package + firewall sub-package + IP allocation paths documented (`auto` / specific / staff-allocated). Folds 4 prior feat branches. |
| `feat/mail` | 2,528 | Full mail surface — domains / aliases / accounts / forwards / list_all union view + per-domain catch-all caveats. Folds 2 prior feat branches. |
| `feat/cloud` | 3,520 | Full cloud namespace — stack writes, db + grants, ssh.user, server config, image management with helpers (`ForkFromImage`, `WaitForBuild`, `LintManifest`, `DeleteAndWait`), volume CRUD, ssl/letsencrypt sub-package. Folds 6 prior feat branches. |

### Batch 5 — registry (heaviest review surface)

| Branch | LoC | Surface |
|---|---|---|
| `feat/srs` | 3,069 | Full registry namespace — 37 endpoints (reads + .nz domain lifecycle + contact CRUD + nameserver / UDAI / transfer / email-template writes). Live-validated against gosh-srs-test.nz. Folds the prior tier-1/tier-2 SRS branches. |

### Aggregated working tree (`feat/examples`)

`feat/examples` is the **integration branch** — it carries every
feat branch above plus the SHC-internal `examples/` tree
(`build-a-site`, `full-overview`, `cloud-volume`, `custom-image*`,
`cloud-db-compare`, `dns*`, `mail*`, `server-reads`,
`server-upgrade-components`, `vps-*`, `srs`, `accounts`,
`redirect`, etc.) and the live-validation findings captured in
`docs/api-issues/`.

This is the branch consumers of gosh-from-our-fork should pin
their `go.mod` `replace` directive against. As per-namespace
branches above land upstream, the equivalent commits will drop off
`feat/examples` on the next rebuild.

Build status: clean. Tests: passing. Validated live across all
re-run examples this session.

---

## Licence

Gosh is distributed under the terms of the [MIT](./LICENSE.md) licence.
