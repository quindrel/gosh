# `sitehost-php*-apache` images: PECL extensions install but don't auto-enable

**Filed:** 2026-05-03 (during examples/custom-image validation)
**Status:** open, workaround documented

## Summary

The standard pattern for adding a PHP extension to a SiteHost
custom image (forked from `sitehost-php85-apache:1.0.1-noble` and
likely sibling apache PHP base images):

```dockerfile
RUN apt-get install -y libyaml-dev php-pear php-dev \
    && pecl install mailparse yaml \
    && phpenmod -v ALL mailparse yaml
```

…produces a **green build trace** (`install ok: channel://
pecl.php.net/{mailparse,yaml}` lines appear) but **the extensions
do not load at runtime**. `phpinfo()` in a deployed container
reports `extension_loaded('mailparse') === false`.

The customer / AI agent only finds out by deploying a stack and
probing — the build success is misleading.

## Root cause (empirically derived)

The SiteHost `sitehost-php*-apache` base images ship a custom PHP
build with a non-standard layout:

- `extension_dir = /lib/php/extensions` (not the Debian default
  `/usr/lib/php/<api>/`)
- Scanned `.ini` directory: `/container/config/php/conf.d/` (a
  *volume-mounted* path, populated from `default-data/config/
  php/conf.d/` in the image's GitLab repo)

Meanwhile:

- PECL writes the `.so` to `/usr/local/lib/php/extensions/<name>.so`
  (no API-version subdirectory on this image)
- `phpenmod` scans `/etc/php/<v>/mods-available/` (the Debian
  system PHP path), not `/usr/local/...`. So it silently no-ops
  for PECL-installed extensions.

The result: the `.so` is present but the system PHP doesn't know
to load it.

## Reproduction

`examples/custom-image-smoke` with `JOURNEY_MODE=pecl` proves the
build-time install succeeds. `examples/custom-image` deploys a
stack and reports `extension_loaded() === false` at runtime.

## Why this matters

1. **Silent runtime failure after green CI.** Consumers shipping
   custom images may believe they've added an extension when in
   fact production code calling `mailparse_*()` will throw a
   "Call to undefined function" at runtime.
2. **No documentation of the right pattern.** The KB's
   custom-image section explains how to author a Dockerfile in
   general, but doesn't document:
   - The image's actual `extension_dir`.
   - The `.ini` discovery path being volume-mounted from
     `default-data/`.
   - That `phpenmod -v ALL` is a no-op for PECL extensions on
     these images.
3. **Pattern brittleness across PHP versions.** Without a
   SiteHost-supplied helper, every customer / AI agent has to
   independently rediscover the layout for each PHP version
   they target.

## Workaround in gosh

`examples/custom-image` uses the working pattern:

```dockerfile
RUN apt-get install -y libyaml-dev php-pear php-dev \
    && pecl install mailparse yaml
```

…then writes `default-data/config/php/conf.d/<ext>.ini` files in
the image repo with **absolute paths**:

```ini
extension=/usr/local/lib/php/extensions/mailparse.so
```

Validated end-to-end: both extensions runtime-loaded after stack
deploy.

## Open questions for the API team / image team

1. **Could the SiteHost PHP base images ship a helper script**
   analogous to the official `php:apache` image's
   `docker-php-ext-enable`, that knows about the custom layout
   and DTRT for PECL-installed extensions?
2. **Could the KB custom-images section** add a "PHP Extensions"
   page documenting:
   - The `extension_dir` for each `sitehost-php*-apache`
     variant.
   - The `default-data/config/php/conf.d/` mount path.
   - A canonical Dockerfile snippet for PECL extension install.
3. **Is the layout consistent across `sitehost-php*-apache`
   versions** (php7, php8, php85)? If yes, the pattern can be
   written once and reused; if not, the KB needs per-version
   guidance.
4. **Are there plans to align the base image's PHP layout** with
   Debian conventions (so `phpenmod` works as expected, and
   apt-installed `php-X-Y` packages load cleanly)?

Happy to validate any candidate documentation or helper script
against the same example pipeline.
