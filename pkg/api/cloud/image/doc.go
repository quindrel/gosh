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
// # Network notes for git+ssh access
//
// gitlab-clients.sitehost.co.nz:22 is reachable from international
// IPs — a single, clean `git clone` works fine from non-NZ
// sources, validated end-to-end. There is, however, **per-IP SSH
// rate limiting / fail2ban at GitLab's edge**: a small number of
// retried SSH attempts from the same source IP gets that IP
// temporarily TCP-refused on port 22 for several minutes. Take
// **single-attempt clones**, not retry loops — the loops are the
// trigger, not the recovery. Earlier "blocked from international
// IPs" framing in this doc was the rate-limit hangover misread as
// a geofence.
//
// Optional escape hatch: examples/custom-image-smoke and
// examples/custom-image both honour a JOURNEY_GIT_PROXY_JUMP
// env var that injects `-o ProxyJump=...` into GIT_SSH_COMMAND,
// useful for callers iterating heavily who'd rather route through
// a bastion's IP. Not required for normal use. See
// docs/api-issues/gitlab-per-ip-ssh-rate-limit.md.
//
// # Runtime gotcha: PECL extensions don't auto-load on `sitehost-php*-apache`
//
// When forking `sitehost-php85-apache` (and likely sibling apache
// PHP base images) and adding a PECL extension via the obvious
// pattern:
//
//	RUN apt-get install -y libyaml-dev php-pear php-dev \
//	    && pecl install mailparse yaml \
//	    && phpenmod -v ALL mailparse yaml
//
// the build trace goes green ("install ok: channel://..."), but
// `extension_loaded()` at runtime returns false. The reason:
//
//   - SiteHost's PHP base image's extension_dir is
//     /lib/php/extensions (not the Debian default
//     /usr/lib/php/<api>/).
//   - PECL writes the .so to /usr/local/lib/php/extensions/<name>.so.
//   - The loaded ini path is /container/config/php/php.ini, with
//     scanned ini directory /container/config/php/conf.d/ (a
//     volume-mounted path populated from default-data/config/php/
//     conf.d/ in the image repo).
//   - phpenmod scans /etc/php/<v>/mods-available/ and silently
//     no-ops because PECL didn't put anything there.
//
// Working pattern (validated live, see examples/custom-image):
//
//   - Dockerfile: `apt-get install -y libyaml-dev php-pear php-dev
//     && pecl install <ext>` — install only, no enable step.
//   - Repo: write
//     default-data/config/php/conf.d/<ext>.ini containing
//     `extension=/usr/local/lib/php/extensions/<ext>.so` (absolute
//     path).
//
// See docs/api-issues/pecl-extensions-not-auto-enabled.md for the
// full empirical breakdown.
package image
