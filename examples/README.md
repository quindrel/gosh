# Runnable examples — live SDK validation

This directory exists so anyone reviewing a PR (or running gosh
against an unfamiliar account) can **prove the SDK works
end-to-end** in under five minutes. Each subdirectory is a
self-contained Go program that hits the live SiteHost API,
exercises a meaningful slice of one of `gosh`'s package surfaces,
and cleans up after itself.

The mocked unit tests under `pkg/api/<package>/` validate request
shape and response parsing. The programs here validate that the
API and the SDK still agree about reality.

---

## Quick-start

```sh
# Required for every example
export SH_API_KEY=<your api key>
export SH_CLIENT_ID=<your client id>

# Required for some examples — see the per-example list below.
# Cloud-server / test-server names are account-specific; use names you control.
export SH_MAIL_SERVER=sth-mail-air                     # SiteHost's shared mail service
export SH_CLOUD_SERVER=<your-cloud-container-name>     # a cloud container you control
export SH_TEST_SERVER=<a-test-server-name>             # a low-stakes server you control

# Run them
go run ./examples/full-overview     # safest — read-only counts across every namespace
go run ./examples/server-reads      # read-only server diagnostics
go run ./examples/dns               # creates+deletes a DNS zone
go run ./examples/mail              # full mail lifecycle
go run ./examples/cloud-volume      # creates+deletes a volume
go run ./examples/server-snapshot   # takes+deletes a snapshot
go run ./examples/srs               # registers+cancels a .nz domain (needs funds)
```

To run them all in sequence, use the `examples/run-all.sh`
script (it gates each example on its required env vars; missing
env skips that example with a clear note).

---

## Validation matrix

This table shows **which endpoint each example exercises and
what passing the example proves about the SDK**. If you can run
all examples to green, you've validated everything in the table.

### `examples/full-overview/` — every-namespace read-only smoke test

| Endpoint                              | Proves                                                          |
|---------------------------------------|------------------------------------------------------------------|
| `server.List`                         | API auth + pagination wrapper                                    |
| `dns.ListZones`, `dns.ListIPs`        | Top-level DNS reads                                             |
| `mail.ListDomains` (gated)            | Mail-server identifier round-trips (when SH_MAIL_SERVER set)    |
| `srs.ListDomains`, `srs.ListContacts` | SRS pagination + contact-summary parsing                        |
| `ssl.ListCertificates`                | SSL summary list                                                |
| `cloud.stack.List`                    | Cross-server stack inventory (no filter = all)                  |
| `cloud.volume.List`                   | Volume inventory                                                |
| `cloud.image.List`                    | Image catalogue                                                 |
| `cloud.db.List`                       | Database inventory                                              |
| `cloud.server.List`                   | Cloud-server inventory                                          |
| `ssh.key.List`                        | SSH key inventory                                               |
| `bandwidth.ListResources`             | Per-client quota groups                                         |
| `snapshot.List` (first server)        | Per-server snapshot inventory                                   |

One line of output per call, counts only. Useful as a 30-second
"is gosh talking to this account?" check before running a heavier
example. Skips mail and snapshot gracefully when their preconditions
aren't satisfied.

### `examples/server-reads/` — read-only smoke test

| Endpoint                              | Proves                                                          |
|---------------------------------------|------------------------------------------------------------------|
| `server.List`                         | API auth works; pagination wrapper parses                        |
| `server.GetState`                     | `name` param accepted; `rescue` bool, `last_job` nested struct  |
| `server.ListUpgrades`                 | Map-keyed disk options; `extra-disk` (hyphenated key) parses    |
| `server.ListImages`                   | Catalog read returns flat list                                  |
| `server.ListLocations`                | `available_ips`/`ipv4`/`ipv6` numeric, `ipv6` bool              |

### `examples/dns/` — zone + records lifecycle

| Endpoint              | Proves                                                                 |
|----------------------|------------------------------------------------------------------------|
| `srs.Whois`          | Whois pre-flight (collision check before touching DNS)                 |
| `dns.CreateZone`     | POST form encoding; the actual API param is `domain` not `name`        |
| `dns.AddRecord`      | Returns `{return: {id}}` shape; A and TXT both supported               |
| `dns.ListRecords`    | Auto-created NS+SOA + user-added records all parse                     |
| `dns.UpdateRecord`   | Synchronous ack response; record content actually updated              |
| `dns.DeleteRecord`   | Synchronous ack; record gone from subsequent list                      |
| `dns.DeleteZone`     | Cleanup works; zone removable                                          |

### `examples/mail/` — full mail lifecycle

| Endpoint                  | Proves                                                              |
|--------------------------|----------------------------------------------------------------------|
| `dns.CreateZone`         | Required precondition (mail domain needs DNS zone)                  |
| `mail.AddDomain`         | `server_name` is the mapped mail-service identifier from your account |
| `mail.AddAccount`        | Nested `params[password]` etc. via embedded AccountParams           |
| `mail.AddAlias`          | `source` + `destination` (not `email`)                              |
| `mail.AddForward`        | Same shape as alias                                                 |
| `mail.ListAccounts`      | 13-field per-account shape                                          |
| `mail.ListAliases`       | 2-field source/destination shape                                    |
| `mail.ListForwards`      | Same shape as aliases                                               |
| `mail.UpdateAccount`     | Optional params[*] field encoding                                   |
| `mail.SearchAliases`     | Query[*] filter encoding; client_id appears in search shape         |
| `mail.DeleteAlias`       | Both source AND destination required                                |
| `mail.DeleteForward`     | Same dual-required pattern                                          |
| `mail.DeleteAccount`     | Synchronous via job                                                 |
| `mail.DeleteDomain`      | Refuses while child records exist (good — order matters)            |
| `dns.DeleteZone`         | Cleanup chain works                                                 |

### `examples/cloud-volume/` — volume lifecycle

| Endpoint              | Proves                                                          |
|----------------------|------------------------------------------------------------------|
| `volume.Add`         | `server_name` + `volume_name` form encoding; job-shape response |
| `volume.List`        | Pagination wrapper; per-volume 13-field shape including `server_owner` (real bool) |
| `volume.Get`         | API quirk: `get` uses `server`/`volume` (NOT `server_name`/`volume_name`) |
| `volume.Delete`      | Same quirky param naming as `Get`; cleanup works                |

### `examples/server-snapshot/` — snapshot lifecycle

| Endpoint                  | Proves                                                          |
|--------------------------|------------------------------------------------------------------|
| `snapshot.Create`        | `name` (server) + `partition` + `lifetime` (hours)              |
| `snapshot.List`          | 19-field per-snapshot shape; mixed bool / string-bool / numeric |
| `snapshot.SetLifetime`   | Param is `snapshot` (NOT `snapshot_id` despite error message)   |
| `snapshot.Delete`        | Same `snapshot=` param; cleanup works                           |

### `examples/srs/` — .nz domain register/cancel

| Endpoint                       | Proves                                                          |
|-------------------------------|------------------------------------------------------------------|
| `srs.ListContacts`            | Pagination at top-level (not inside Return)                     |
| `srs.DomainAvailable`         | Bare-bool Return shape                                          |
| `srs.CreateDomain`            | Inconsistent param names: `registrant_contact` (no `_id`) + `params[AdminContact]` (PascalCase nested) + `params[TechContact]` (NOT "Technical") |
| `srs.GetDomain`               | 30-field PascalCase response with real bools, ints, etc.        |
| `srs.ListNameServers`         | Domain delegation entries                                       |
| `srs.CancelDomain`            | Job-shape response (not just ack); .nz 5-day grace period       |

---

## Required environment per example

All example-specific env vars are pointers to **resources on
your account**. Set them to names you control; the values shown
are placeholders only.

| Example              | Env vars beyond `SH_API_KEY` + `SH_CLIENT_ID`                   |
|---------------------|------------------------------------------------------------------|
| `full-overview`     | `SH_CLIENT_ID` is **optional** here — if unset, the program calls `info.NewClientWithDiscovery` to resolve it from `api/get_info.json`. Optional `SH_MAIL_SERVER` to include the mail-domains read. |
| `server-reads`      | (none — picks the first server returned by `server.List`)       |
| `dns`               | (none — generates a unique zone name; whois-checked first)      |
| `mail`              | `SH_MAIL_SERVER` — mail service identifier (e.g. `sth-mail-air` for SiteHost's shared mail) |
| `cloud-volume`      | `SH_CLOUD_SERVER` — name of a cloud container you don't mind briefly attaching a volume to |
| `server-snapshot`   | `SH_TEST_SERVER` — name of a server you don't mind briefly snapshotting; optional `SH_TEST_PARTITION` (default `scsi0`) |
| `srs`               | (none required; optional `SH_TEST_REGISTRANT/ADMIN/TECH/BILLING` to override contact-ID auto-discovery) — account must hold funds for registration |

---

## Output convention

Every example uses the same step-by-step convention:

- `✓` prefix means a step succeeded as expected
- `⚠` prefix means a step failed but the program is continuing (often used during cleanup so partial failures don't leave debris)
- A bare `Fatal:` line means the program is aborting; the message names which call failed and any cleanup hint

This makes scanning the output for "did everything work" trivial.

---

## Safety notes

- `srs/` registers a real domain. **Funds must be available** in
  the account at registration time. The .nz registry refunds
  within the 5-day grace period if cancellation succeeds before
  then — but the funds need to be there at create time.
- All other examples create short-lived test resources and clean
  up before exit. Storage / processing costs are negligible for
  the brief lifetime.
- If a program crashes mid-way it may leave test resources
  behind. Each program prints what it created so manual cleanup
  is straightforward.
- All test resource names embed `gosh-example-` and a UNIX
  timestamp, so they're trivially identifiable in CP-Admin.

---

## Notes for SDK contributors

These are *not* unit tests. They live in `examples/` rather than
`*_test.go`. The httptest-based tests under `pkg/api/<package>/`
are the unit-level coverage; these programs are the
integration-level demonstration.

The example programs are excluded from `go test ./...` runs by
virtue of being `package main` in their own directories. They
build with `go build ./...` and run with `go run ./examples/<name>`.

When adding a new endpoint to a package, consider whether the
matching example here also wants extending — it's the fastest
place to validate end-to-end against live.
