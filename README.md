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

All branches are independent (single-squashed-commit off `main`,
disjoint files) and can technically land in any order. The order
below is reviewer-friendly: smallest / lowest-risk first.

### Batch 1 — trivial cleanups (review in minutes)

| Branch | Surface |
|---|---|
| `feat/comment-fixups` | 5 doc-comment endpoint-path typos (image package + models) |
| `feat/cloud-ssh-user-volumes-tag` | One-line `url` tag fix on `cloud/ssh/user.AddRequest.Volumes` |
| `feat/job-message-field` | Adds `Message` field to `models.JobDetails` |

### Batch 2 — small additive PRs

| Branch | Surface |
|---|---|
| `feat/dns-domain-templates` | 1 endpoint, new `pkg/api/dns/template` sub-package |
| `feat/info-discover-client-id` | New helper `info.NewClientWithDiscovery` (+ 4 tests) — bootstrap `*api.Client` from just an API key |

### Batch 3 — existing-package extensions

| Branch | Surface |
|---|---|
| `feat/dns-reads` | 1 new endpoint + `GetZoneResponse` schema fix + 11 backfilled tests |
| `feat/bandwidth-usage` | 5 new endpoints + 1 backfilled test |

### Batch 4 — new small packages

| Branch | Surface |
|---|---|
| `feat/ssl` | 3 endpoints, new `pkg/api/ssl` package |
| `feat/server` | reads + writes on existing `pkg/api/server` |

### Batch 5 — new larger packages

| Branch | Surface |
|---|---|
| `feat/cloud-volume` | 6 endpoints, new sub-package |
| `feat/server-snapshot` | 5 endpoints incl. destructive `Restore` |
| `feat/cloud-stack-writes` | 6 write ops on `pkg/api/cloud/stack` (Update, Delete, Copy, Overwrite, Backup, PurgeCache). Param shapes inferred from API validation messages — sanity-check before merge. |

### Batch 6 — heaviest review surface

| Branch | Surface |
|---|---|
| `feat/mail` | ~17 endpoints (full mail, reads + writes) |
| `feat/srs` | full registry package, ~16 endpoints, .nz lifecycle |

### Aggregated working tree

Every batch above is also cherry-picked onto `integration` for
combined-state testing. SHC's `go.mod` `replace` directive should
pin to a tag on `integration`, never to a branch HEAD. As feat
branches land upstream, drop them from `integration` on the next
rebuild.

### Examples branch (`feat/examples`)

Not for upstream as-is. Contains the SHC-internal `examples/` tree
(`build-a-site`, `full-overview`, `cloud-volume`, `dns`, `mail`,
`server-reads`, `server-snapshot`, `srs`). Its commits depend on
the SDK feat branches above being upstream first. Once batches 1–6
merge, rebase the example commits on upstream `main` and open a
single `feat/examples-tree` PR.

---

## Licence

Gosh is distributed under the terms of the [MIT](./LICENSE.md) licence.
