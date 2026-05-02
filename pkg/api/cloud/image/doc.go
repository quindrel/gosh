// Package image provides access to the /cloud/image API endpoints
// for managing customer "custom images" — Docker images built in
// SiteHost's GitLab CI from a customer-owned repository, then
// deployable as Cloud Container stacks.
//
// # The custom-image workflow
//
// A custom image lives in two places: a metadata record (managed
// here, via this package) and a backing GitLab repository
// (gitlab-clients.sitehost.co.nz, accessed via git+ssh). The full
// authoring loop is:
//
//  1. cloud.image.Create (or the ForkFromImage helper) — registers
//     the image and provisions its GitLab repository. Asynchronous;
//     poll the returned scheduler job before cloning.
//  2. git clone <CloneURL> — pull the auto-generated repository.
//     The repository contains a Dockerfile, a manifest.yml, a
//     .gitlab-ci.yml (locked; commits modifying it are rejected),
//     and a default-data/ directory.
//  3. Edit Dockerfile / manifest.yml / default-data/ locally.
//  4. git push — triggers a build in SiteHost's GitLab CI.
//  5. cloud.image.version.ListAll / WaitForBuild — poll for the
//     latest build. On failure, GetBuild returns the full CI trace.
//  6. The latest *successful* build is what cloud.stack.Add uses
//     when you reference the image's code.
//
// # What this package wraps vs what it doesn't
//
// API endpoints (1-1 wrappers):
//   - Create, Delete, Get, GetChangelog, List
//   - Sub-package /cloud/image/version: ListAll, GetBuild, Delete
//
// Helpers (composite operations or local-only utilities):
//   - ForkFromImage: resolves a public parent's id from
//     /cloud/stack/image/list_all (NOT /cloud/image/list_all,
//     which only returns customer-owned images), then calls Create.
//   - CloneURL: constructs the git@host:g_<client>/<code>.git URL
//     from the api.Client's CustomImageGitHost (default
//     "gitlab-clients.sitehost.co.nz") + ClientID + image code.
//     The URL is *not* exposed by any API endpoint.
//   - WaitForBuild: polls version.ListAll until the latest version
//     reports terminal status.
//   - LintManifest: local YAML schema check against the documented
//     minimum manifest.yml fields.
//
// Git operations (clone, commit, push) are *not* wrapped — gosh
// deliberately doesn't bundle a git client. Consumers shell out to
// /usr/bin/git or use go-git as a separate dep.
//
// # Discovery gaps to be aware of
//
// Two pieces of information that consumers need but the API does
// not surface:
//
//  1. The GitLab clone URL itself. cloud.image.Get returns the
//     Docker registry_url, not the git repository URL. CloneURL
//     constructs it deterministically; if the GitLab host ever
//     changes, override via api.SetCustomImageGitHost.
//  2. The numeric id of a public parent image (needed for
//     /cloud/image/create's params[fork_id]). This lives in
//     /cloud/stack/image/list_all, not /cloud/image/list_all.
//     ForkFromImage does the lookup automatically.
//
// # Network requirements (consumers running outside the SiteHost network)
//
// gitlab-clients.sitehost.co.nz:22 appears geo-restricted: TCP
// connections from outside SiteHost's New Zealand network may be
// firewalled. AI agents driving this from outside NZ typically need
// either a SiteHost-network bastion (use SSH ProxyJump in
// GIT_SSH_COMMAND) or an explicit firewall exception via SiteHost
// support. HTTPS port 443 is reachable from international IPs but
// authentication via HTTPS git operations hasn't been validated by
// this SDK's authors.
package image
