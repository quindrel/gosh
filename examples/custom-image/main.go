// Program custom-image walks the full custom-image lifecycle for a
// customer container, end-to-end via gosh:
//
//  1. Generate an ephemeral SSH keypair, register the public half
//     with ssh.key.Create so it can clone the GitLab-hosted custom
//     image repo, and with cloud.ssh.user.Add so we can SFTP into
//     containers for the smoke test.
//  2. Locate (or accept via env) a Cloud Container Server to deploy on.
//  3. Fork the public sitehost-php85-apache image into a new
//     customer image via cloud.image.ForkFromImage.
//  4. Construct the GitLab clone URL via cloud.image.Client.CloneURL
//     and clone the repo using the local git binary
//     (GIT_SSH_COMMAND points at our ephemeral private key).
//  5. Append a PECL install for mailparse + yaml to the Dockerfile.
//     Run cloud.image.LintManifest as a sanity check on the
//     already-present manifest.yml.
//  6. git commit + git push — triggers a build in SiteHost's GitLab CI.
//  7. Poll cloud.image.version.list_all via cloud.image.Client.WaitForBuild
//     until the build reports success or failed. On failure, fetch the
//     build trace via version.GetBuild and exit with the trace.
//  8. Provision a stack on the CCS using the new image code,
//     wait for the scheduler job to complete.
//  9. SFTP a tiny phpinfo.php into the container's
//     /container/application/public/, then curl it and assert that
//     extension_loaded("mailparse") and extension_loaded("yaml")
//     both report true.
// 10. Cleanup: delete the stack, delete the image, deregister both
//     SSH keys.
//
// Required env:
//
//	SH_API_KEY                — your API key
//
// Optional env:
//
//	SH_CLIENT_ID              — sub-account targeting; otherwise discovered.
//	SH_CCS_NAME               — name of an existing CCS to deploy on.
//	                            Mutually exclusive with JOURNEY_PROVISION_CCS.
//	                            If neither set, picks the first CCS from
//	                            cloud.server.List.
//	JOURNEY_PROVISION_CCS=1   — provision a fresh CCS in AKLCITY (zero-cost
//	                            staff region) just for this run, instead of
//	                            using SH_CCS_NAME or auto-pick. Adds ~5min
//	                            to the run; tear-down happens at cleanup
//	                            unless JOURNEY_KEEP_CCS=1.
//	JOURNEY_CCS_IMAGE         — override the CCS provisioning image code
//	                            (default: ubuntu-cc-2404-20260323).
//	JOURNEY_PARENT_IMAGE      — override the custom-image fork parent
//	                            (default: sitehost-php85-apache).
//	JOURNEY_KEEP=1            — leave EVERYTHING (image, stack, SSH keys,
//	                            CCS) in place after the run for inspection.
//	JOURNEY_KEEP_CCS=1        — leave the provisioned CCS but tear down
//	                            stack/image/keys. Used to amortise CCS
//	                            provisioning across iterative runs.
//	JOURNEY_KEEP_IMAGE=1      — leave the built custom image but tear down
//	                            stack/keys. Used together with
//	                            JOURNEY_REUSE_IMAGE on subsequent runs.
//	JOURNEY_REUSE_IMAGE=<code> — skip fork+clone+build entirely; resolve
//	                            an existing image's id and deploy a stack
//	                            from its latest successful build. Pair with
//	                            SH_CCS_NAME to repeat the runtime probe in
//	                            seconds rather than minutes during iteration.
//
// Iteration workflow (after one initial slow run):
//
//	# 1. First run — provision + build + keep both for reuse:
//	JOURNEY_PROVISION_CCS=1 JOURNEY_KEEP_CCS=1 JOURNEY_KEEP_IMAGE=1 ./custom-image
//	# Note the printed CCS name and image code at the end.
//
//	# 2. Iterate — reuse both, just deploy/probe/clean:
//	SH_CCS_NAME=<name> JOURNEY_REUSE_IMAGE=<code> JOURNEY_KEEP_CCS=1 \
//	    JOURNEY_KEEP_IMAGE=1 ./custom-image
//	# Each iteration is ~30-60 seconds.
//
//	# 3. When done, tear down explicitly:
//	# (delete the CCS via gosh or Control Panel; image is auto-cleaned
//	# when the CCS goes since image records aren't shared across CCSes.)
//
// Side effects:
//   - Creates one customer SSH key (ssh.key) and one container SSH user
//     (cloud.ssh.user). Cleaned up by default.
//   - Creates one custom image (cloud.image), one image build (PECL
//     install), and one Docker stack (cloud.stack) on the target CCS.
//     Cleaned up by default.
//   - Pushes one commit to the customer's GitLab project for the new
//     custom image. The image is removed on cleanup, taking the
//     project's history with it.
//
// External requirements:
//   - The local `git` binary in PATH (the helper relies on it for
//     clone + push; gosh deliberately doesn't bundle a git client).
//   - Outbound SSH to gitlab-clients.sitehost.co.nz:22. Reachable
//     from international IPs (validated from a Philippines source);
//     the example uses a single-attempt clone because GitLab's edge
//     enforces per-IP SSH rate limiting that aggressive retry loops
//     reliably trigger. JOURNEY_GIT_PROXY_JUMP routes via a bastion
//     for callers who want to absorb a rate-limit hit via a
//     different source IP, but it's optional. See
//     docs/open-api-questions.md.
//
// This is dogfood: every API interaction goes through gosh. Git
// clone/push is the one external tool — we shell out to git rather
// than pull go-git in as a heavy dep.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"

	"github.com/sitehostnz/gosh/pkg/api"
	cloudImage "github.com/sitehostnz/gosh/pkg/api/cloud/image"
	imgVersion "github.com/sitehostnz/gosh/pkg/api/cloud/image/version"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	cloudSSHUser "github.com/sitehostnz/gosh/pkg/api/cloud/ssh/user"
	"github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	"github.com/sitehostnz/gosh/pkg/api/server"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
	pnet "github.com/sitehostnz/gosh/pkg/net"
)

const (
	defaultParentImage = "sitehost-php85-apache"
	imageBuildTimeout  = 15 * time.Minute
	imageBuildPoll     = 15 * time.Second

	// CCS provisioning constants — used only when
	// JOURNEY_PROVISION_CCS=1 (default OFF; existing CCSes are
	// reused via SH_CCS_NAME or auto-picked otherwise).
	ccsLocation         = "AKLCITY"      // zero-cost staff region (matches probe-tls-default)
	ccsProductCode      = "CLDCON4-P"    // Performance Cloud Container - 4 Core
	ccsDefaultImageCode = "ubuntu-cc-2404-20260323"
	ccsProvisionTimeout = 15 * time.Minute
	jobPollInterval    = 5 * time.Second
	jobTimeout         = 10 * time.Minute
)

func main() {
	if err := mainErr(); err != nil {
		log.Fatalf("custom-image: %v", err)
	}
}

// mainErr owns the lifecycle so deferred cleanup runs even when a
// step fails (log.Fatalf calls os.Exit and skips defers — leaks).
func mainErr() error {
	ctx := context.Background()
	r := &runner{ctx: ctx}

	if err := r.setup(); err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	defer r.cleanup()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"register SSH key", r.registerSSHKey},
		{"provision CCS (or use existing)", r.locateOrProvisionCCS},
		{"fork custom image", r.forkImage},
		{"clone repo + edit Dockerfile", r.cloneAndEdit},
		{"git push (triggers build)", r.commitAndPush},
		{"wait for build", r.waitForBuild},
		{"deploy stack", r.deployStack},
		{"deploy phpinfo + probe extensions", r.probeExtensions},
	}
	for _, s := range steps {
		log.Printf("==> %s", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}

	log.Printf("✓ custom-image example completed successfully")
	log.Printf("  image code:   %s", r.imageCode)
	log.Printf("  stack:        %s on %s", r.stackName, r.serverName)
	log.Printf("  smoke URL:    %s", r.siteURL)
	return nil
}

type runner struct {
	ctx context.Context
	c   *api.Client

	// SSH key state
	keyID    int    // ssh.key id (used for image fork ssh_keys)
	keyName  string // both ssh.key.Update label and cloud.ssh.user username
	keyPath  string // local private key file path
	pubKey   string // OpenSSH-format public key line

	// CCS target
	serverName string

	// Custom image state
	parentImage string
	imageLabel  string
	imageCode   string
	imageID     int

	// Repo workspace
	repoDir string

	// Stack state
	stackName         string
	siteURL           string
	siteHost          string
	siteIP            string
	containerUsername string
	imageVersion      string // e.g. "1.0-26356", from WaitForBuild

	// Cleanup flags
	keep                bool
	keepCCS             bool
	keepImage           bool
	createdSSHKey       bool
	createdContainerKey bool
	createdImage        bool
	createdStack        bool
	provisionedCCS      bool
}

func (r *runner) setup() error {
	apiKey := os.Getenv("SH_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("SH_API_KEY required")
	}
	clientID := os.Getenv("SH_CLIENT_ID")

	r.keep = os.Getenv("JOURNEY_KEEP") == "1"
	r.keepCCS = r.keep || os.Getenv("JOURNEY_KEEP_CCS") == "1"
	r.keepImage = r.keep || os.Getenv("JOURNEY_KEEP_IMAGE") == "1"
	r.parentImage = os.Getenv("JOURNEY_PARENT_IMAGE")
	if r.parentImage == "" {
		r.parentImage = defaultParentImage
	}

	if clientID == "" {
		// Discover the first available client_id via info.list_clients.
		c, err := info.NewClientWithDiscovery(r.ctx, apiKey)
		if err != nil {
			return fmt.Errorf("client discovery: %w", err)
		}
		r.c = c
	} else {
		c, err := api.New(apiKey, clientID)
		if err != nil {
			return fmt.Errorf("api.New: %w", err)
		}
		r.c = c
	}

	suffix := randomSuffix()
	r.keyName = "gosh-customimg-" + suffix
	r.imageLabel = "gosh custom-image " + suffix
	r.imageCode = "gosh-customimg-" + suffix
	r.stackName = "ciexample" + suffix

	// Reuse mode: skip the fork+clone+build cycle entirely and just
	// deploy / probe / clean a stack against an already-built image.
	// Used together with SH_CCS_NAME to repeat the runtime probe
	// quickly while iterating on phpinfo or extension behaviour.
	if reuse := os.Getenv("JOURNEY_REUSE_IMAGE"); reuse != "" {
		r.imageCode = reuse
		log.Printf("JOURNEY_REUSE_IMAGE=%s — skipping fork+build steps", reuse)
	}
	return nil
}

// registerSSHKey generates an ed25519 keypair, writes the private
// half to a temp file (chmod 0600), and registers the public half
// with ssh.key.Create. The same key is also used to log into the
// container as a transient SFTP user later.
func (r *runner) registerSSHKey() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("ed25519.GenerateKey: %w", err)
	}

	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	r.pubKey = strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub)))

	pemBytes, err := openSSHPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}
	r.keyPath = filepath.Join(os.TempDir(), r.keyName+".key")
	if err := os.WriteFile(r.keyPath, pemBytes, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	keyClient := sshKey.New(r.c)
	resp, err := keyClient.Create(r.ctx, sshKey.CreateRequest{
		Label:   r.keyName,
		Content: r.pubKey,
	})
	if err != nil {
		return fmt.Errorf("ssh.key.Create: %w", err)
	}
	id, err := strconv.Atoi(resp.Return.KeyID)
	if err != nil {
		return fmt.Errorf("non-numeric ssh.key id %q: %w", resp.Return.KeyID, err)
	}
	r.keyID = id
	r.createdSSHKey = true
	log.Printf("    ssh.key id=%d label=%s", r.keyID, r.keyName)
	return nil
}

// locateOrProvisionCCS picks the target CCS in priority order:
//
//  1. SH_CCS_NAME env var — explicit override.
//  2. JOURNEY_PROVISION_CCS=1 — provision a fresh CCS in AKL01,
//     used just for this run, deleted at cleanup. Avoids hitting
//     image-count limits on shared test CCSes.
//  3. Auto-pick the first CCS from cloud.server.List (cheap path
//     when an empty / lightly-loaded CCS already exists).
func (r *runner) locateOrProvisionCCS() error {
	if name := os.Getenv("SH_CCS_NAME"); name != "" {
		r.serverName = name
		log.Printf("    using CCS %q from SH_CCS_NAME", name)
		return nil
	}
	if os.Getenv("JOURNEY_PROVISION_CCS") == "1" {
		return r.provisionFreshCCS()
	}
	servers, err := cloudServer.New(r.c).List(r.ctx)
	if err != nil {
		return fmt.Errorf("cloud.server.List: %w", err)
	}
	if len(servers.CloudServers) == 0 {
		return fmt.Errorf("no Cloud Container Servers found; set SH_CCS_NAME or JOURNEY_PROVISION_CCS=1")
	}
	r.serverName = servers.CloudServers[0].Name
	log.Printf("    auto-selected CCS %q", r.serverName)
	return nil
}

// provisionFreshCCS provisions a temporary CCS in AKL01 (zero-cost
// staff region), waits for the scheduler job, and records cleanup
// state. The CCS is torn down by cleanup() with force_delete=1
// (needed because the platform auto-deploys an infra stack on
// every fresh CCS that plain Delete refuses to remove).
func (r *runner) provisionFreshCCS() error {
	imageCode := os.Getenv("JOURNEY_CCS_IMAGE")
	if imageCode == "" {
		imageCode = ccsDefaultImageCode
	}

	ipAddr, err := rawFirstFreeIP(r.c, ccsLocation)
	if err != nil {
		return fmt.Errorf("list IPs in %s: %w", ccsLocation, err)
	}
	log.Printf("    free IP in %s: %s", ccsLocation, ipAddr)

	srvClient := server.New(r.c)
	createResp, err := srvClient.Create(r.ctx, server.CreateRequest{
		Label:       "gosh-customimg-" + randHex(8),
		Location:    ccsLocation,
		ProductCode: ccsProductCode,
		Image:       imageCode,
		Params:      server.ParamsOptions{IPv4: []string{ipAddr}},
	})
	if err != nil {
		return fmt.Errorf("server.Create: %w", err)
	}
	r.serverName = createResp.Return.Name
	r.provisionedCCS = true
	log.Printf("    provisioning CCS %q (job %d, type=%s)",
		r.serverName, createResp.Return.ID, createResp.Return.Type)

	if err := waitForJob(r.ctx, r.c, createResp.Return.ID, createResp.Return.Type, ccsProvisionTimeout); err != nil {
		return fmt.Errorf("provision job: %w", err)
	}
	// Give nginx-proxy + infra stack time to come up before we add
	// our stack on top.
	log.Printf("    CCS provisioned; waiting 30s for infra stack to settle")
	time.Sleep(30 * time.Second)
	return nil
}

// rawFirstFreeIP — inline call to server/list_ips.json. Same
// pattern as examples/probe-tls-default; swap to gosh's
// server.ListIPs once the wrapper is on this branch.
func rawFirstFreeIP(c *api.Client, loc string) (string, error) {
	req, err := c.NewRequest("GET", "server/list_ips.json", "")
	if err != nil {
		return "", err
	}
	v := req.URL.Query()
	v.Add("location", loc)
	req.URL.RawQuery = pnet.Encode(v, []string{"apikey", "client_id", "location"})
	var resp struct {
		Return []struct {
			IPAddr string `json:"ip_addr"`
		} `json:"return"`
	}
	if err := c.Do(context.Background(), req, &resp); err != nil {
		return "", err
	}
	if len(resp.Return) == 0 {
		return "", fmt.Errorf("no free IPs at %s", loc)
	}
	return resp.Return[0].IPAddr, nil
}

// forkImage uses the cloud.image.ForkFromImage helper, which
// resolves the public parent's numeric id by listing then calls
// cloud.image.Create. We then poll until the create-image job is
// done, since the GitLab repository is provisioned asynchronously
// and we can't clone before that completes.
//
// In JOURNEY_REUSE_IMAGE mode we just resolve the existing image's
// numeric id (needed downstream by deployStack for the version tag)
// and return — no fork, no Create job.
func (r *runner) forkImage() error {
	imgClient := cloudImage.New(r.c)
	if os.Getenv("JOURNEY_REUSE_IMAGE") != "" {
		got, err := imgClient.Get(r.ctx, cloudImage.GetRequest{Code: r.imageCode})
		if err != nil {
			return fmt.Errorf("cloud.image.Get(%s) for reuse: %w", r.imageCode, err)
		}
		id, err := strconv.Atoi(got.Image.ID)
		if err != nil {
			return fmt.Errorf("non-numeric image id %q: %w", got.Image.ID, err)
		}
		r.imageID = id
		log.Printf("    reusing image id=%d code=%s", r.imageID, r.imageCode)
		return nil
	}

	resp, err := imgClient.ForkFromImage(r.ctx,
		r.parentImage, r.imageLabel, r.imageCode, []int{r.keyID})
	if err != nil {
		return fmt.Errorf("ForkFromImage(%s): %w", r.parentImage, err)
	}
	r.createdImage = true

	if resp.Return.ID > 0 {
		log.Printf("    fork job id=%d type=%s", resp.Return.ID, resp.Return.Type)
		if err := waitForJob(r.ctx, r.c, resp.Return.ID, resp.Return.Type, jobTimeout); err != nil {
			return fmt.Errorf("waiting for fork job: %w", err)
		}
	}

	// Get the new image's numeric id (needed for WaitForBuild later).
	got, err := imgClient.Get(r.ctx, cloudImage.GetRequest{Code: r.imageCode})
	if err != nil {
		return fmt.Errorf("cloud.image.Get(%s): %w", r.imageCode, err)
	}
	id, err := strconv.Atoi(got.Image.ID)
	if err != nil {
		return fmt.Errorf("non-numeric image id %q: %w", got.Image.ID, err)
	}
	r.imageID = id
	log.Printf("    new image id=%d code=%s", r.imageID, r.imageCode)
	return nil
}

// cloneAndEdit clones the GitLab repo using the helper-constructed
// URL, lints the existing manifest.yml, and appends a PECL install
// directive to the Dockerfile. Skipped in JOURNEY_REUSE_IMAGE mode
// since the image is already built.
func (r *runner) cloneAndEdit() error {
	if os.Getenv("JOURNEY_REUSE_IMAGE") != "" {
		log.Printf("    JOURNEY_REUSE_IMAGE — skipping clone+edit")
		return nil
	}
	cloneURL := cloudImage.New(r.c).CloneURL(r.imageCode)
	log.Printf("    clone URL: %s", cloneURL)

	dir, err := os.MkdirTemp("", "gosh-customimg-*")
	if err != nil {
		return fmt.Errorf("mktmp: %w", err)
	}
	r.repoDir = dir

	// Single attempt only. gitlab-clients.sitehost.co.nz runs aggressive
	// per-IP rate limiting; retry loops trigger TCP-level "connection
	// refused" against the source IP for several minutes. Wait briefly
	// for the GitLab repo to settle, then take one shot.
	time.Sleep(5 * time.Second)
	if err := r.runGit(dir, "clone", cloneURL, "."); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	// Lint the manifest.yml that came in from the parent image.
	manifestPath := filepath.Join(dir, "manifest.yml")
	if mb, err := os.ReadFile(manifestPath); err == nil {
		if lerr := cloudImage.LintManifest(mb); lerr != nil {
			return fmt.Errorf("inherited manifest.yml fails lint: %w", lerr)
		}
		log.Printf("    manifest.yml lint OK")
	} else {
		return fmt.Errorf("read manifest.yml: %w", err)
	}

	// Append the PECL install RUN line to Dockerfile. Idempotent on
	// re-runs in the unlikely case JOURNEY_KEEP=1 left a worktree.
	dockerfilePath := filepath.Join(dir, "Dockerfile")
	current, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return fmt.Errorf("read Dockerfile: %w", err)
	}
	if bytes.Contains(current, []byte("# gosh:custom-image marker")) {
		log.Printf("    Dockerfile already patched; skipping append")
		return nil
	}
	// Install mailparse + yaml extensions via PECL (compiled against
	// the running PHP at build time — works on any sitehost-php*
	// base image regardless of the PHP version's apt-package
	// availability).
	//
	// PECL on the SiteHost base writes the .so directly to
	// /usr/local/lib/php/extensions/<name>.so (no api-version
	// subdirectory). We then enable each extension by writing an
	// .ini file with the absolute .so path into
	// default-data/config/php/conf.d/ in the repo (handled in Go
	// below) — that path becomes /container/config/php/conf.d/ at
	// runtime via the standard SiteHost volume mount.
	//
	// Why hard-code the absolute path: the SiteHost PHP base
	// image's extension_dir is /lib/php/extensions, but PECL
	// installs to /usr/local/lib/php/extensions. Rather than
	// copying .so files between paths (mismatched API versions can
	// silently break runtime loading), the .ini just references
	// the install location directly.
	//
	// Discovered via runtime phpinfo() probe — see
	// docs/open-api-questions.md "PECL extension installs don't
	// auto-enable on sitehost-php*-apache".
	patch := `

# gosh:custom-image marker — added by examples/custom-image
# Compile mailparse + yaml against the running PHP via PECL.
# (libyaml-dev is the build-time dep for the yaml extension.)
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libyaml-dev php-pear php-dev \
    && pecl install mailparse yaml \
    && rm -rf /var/lib/apt/lists/*
`
	combined := append(current, []byte(patch)...)
	if err := os.WriteFile(dockerfilePath, combined, 0o644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	log.Printf("    Dockerfile patched (+%d bytes)", len(patch))

	// Drop extension .ini files into default-data/config/php/conf.d/
	// so they're mounted at /container/config/php/conf.d/ at runtime
	// (per the runtime phpinfo probe finding above). Each .ini just
	// declares "extension=<name>.so" — the .so itself is placed by
	// the Dockerfile RUN above.
	confDir := filepath.Join(dir, "default-data", "config", "php", "conf.d")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return fmt.Errorf("mkdir conf.d: %w", err)
	}
	// Reference the .so by absolute path. PECL on this base image
	// writes to /usr/local/lib/php/extensions/<name>.so directly
	// (verified via build-trace inspection in custom-image-smoke
	// pecl mode). PHP accepts an absolute path in `extension=`.
	for _, ext := range []string{"mailparse", "yaml"} {
		iniPath := filepath.Join(confDir, ext+".ini")
		body := fmt.Sprintf("extension=/usr/local/lib/php/extensions/%s.so\n", ext)
		if err := os.WriteFile(iniPath, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s.ini: %w", ext, err)
		}
		log.Printf("    wrote %s.ini", ext)
	}
	return nil
}

// commitAndPush stages, commits, and pushes the Dockerfile change.
// The push triggers SiteHost's GitLab CI to build the image.
// Skipped in JOURNEY_REUSE_IMAGE mode.
func (r *runner) commitAndPush() error {
	if os.Getenv("JOURNEY_REUSE_IMAGE") != "" {
		return nil
	}
	if err := r.runGit(r.repoDir, "config", "user.email", "gosh-example@sitehost.nz"); err != nil {
		return fmt.Errorf("git config email: %w", err)
	}
	if err := r.runGit(r.repoDir, "config", "user.name", "gosh custom-image example"); err != nil {
		return fmt.Errorf("git config name: %w", err)
	}
	if err := r.runGit(r.repoDir, "add", "Dockerfile", "default-data"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := r.runGit(r.repoDir, "commit", "-m", "Install mailparse + yaml PECL extensions"); err != nil {
		// "nothing to commit" surfaces here on idempotent re-runs.
		log.Printf("    git commit (continuing): %v", err)
	}
	if err := r.runGit(r.repoDir, "push", "origin", "HEAD"); err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	log.Printf("    pushed; CI build queued")
	return nil
}

// waitForBuild uses the helper to poll cloud.image.version.list_all
// until the latest version reports a terminal status. On failure we
// fetch the full build trace via GetBuild and surface it.
//
// In JOURNEY_REUSE_IMAGE mode, fetch the latest already-built
// version directly via ListAll (no waiting) so the version tag is
// available to deployStack.
func (r *runner) waitForBuild() error {
	if os.Getenv("JOURNEY_REUSE_IMAGE") != "" {
		resp, err := imgVersion.New(r.c).ListAll(r.ctx, imgVersion.ListAllRequest{
			ImageID: r.imageID, SortBy: "date_added", SortDir: "DESC", PageSize: 1,
		})
		if err != nil {
			return fmt.Errorf("version.ListAll for reuse: %w", err)
		}
		if len(resp.Return.Versions) == 0 {
			return fmt.Errorf("reuse image %s has no built versions", r.imageCode)
		}
		v := resp.Return.Versions[0]
		r.imageVersion = v.Version
		log.Printf("    reusing latest build %s status=%s version=%s", v.BuildID, v.BuildStatus, v.Version)
		if v.BuildStatus != cloudImage.BuildStatusSuccess {
			return fmt.Errorf("latest build is %s, not success", v.BuildStatus)
		}
		return nil
	}

	v, err := cloudImage.New(r.c).WaitForBuild(r.ctx, r.imageID, imageBuildTimeout, imageBuildPoll)
	if err != nil {
		return fmt.Errorf("WaitForBuild: %w", err)
	}
	r.imageVersion = v.Version
	log.Printf("    build %s: status=%s version=%s", v.BuildID, v.BuildStatus, v.Version)
	if v.BuildStatus != cloudImage.BuildStatusSuccess {
		// Fetch and surface the trace so the consumer (or AI agent)
		// can diagnose without leaving the program.
		trace, terr := imgVersion.New(r.c).GetBuild(r.ctx, imgVersion.GetBuildRequest{
			Code: r.imageCode, BuildID: v.BuildID,
		})
		if terr != nil {
			return fmt.Errorf("build %s and trace fetch failed: %w", v.BuildStatus, terr)
		}
		return fmt.Errorf("build %s — trace:\n%s", v.BuildStatus, trace.Return.BuildTrace)
	}
	return nil
}

// deployStack provisions a www stack on the target CCS using the
// freshly-built custom image. The compose body matches the
// SiteHost convention: nginx-proxy routes by VIRTUAL_HOST.
func (r *runner) deployStack() error {
	// Synthesize a sth.nz wildcard hostname against the CCS's primary IP.
	// cloud.server has no Get(name) wrapper; iterate List to resolve.
	servers, err := cloudServer.New(r.c).List(r.ctx)
	if err != nil {
		return fmt.Errorf("cloud.server.List: %w", err)
	}
	for _, s := range servers.CloudServers {
		if s.Name == r.serverName {
			r.siteIP = s.PrimaryIP
			break
		}
	}
	if r.siteIP == "" {
		return fmt.Errorf("CCS %s not found or has no primary IP", r.serverName)
	}
	// Stack names must come from cloud.stack.GenerateName (the API
	// rejects custom names with "Unable to add stack, the hostname
	// is invalid"). Override the synthetic name we set during setup.
	gen, err := stack.New(r.c).GenerateName(r.ctx)
	if err != nil {
		return fmt.Errorf("cloud.stack.GenerateName: %w", err)
	}
	r.stackName = gen.Return.Name

	// sth.nz wildcard DNS form: <subdomain>.<IP>.sth.nz.
	r.siteHost = fmt.Sprintf("gosh.%s.sth.nz", r.siteIP)
	r.siteURL = "http://" + r.siteHost

	// docker-compose body matches build-a-site's working www-stack
	// shape: nginx-proxy routes by VIRTUAL_HOST, the standard
	// SiteHost volumes are mounted, and the image points at our
	// freshly-built custom image.
	// Image reference must include a version tag (the API rejects
	// untagged refs with "There was no image version provided").
	// imageVersion comes from WaitForBuild, e.g. "1.0-26356".
	composeYAML := buildWWWCompose(r.stackName, r.siteHost,
		fmt.Sprintf("registry-clients.sitehost.co.nz/g_%s/%s:%s",
			r.c.ClientID, r.imageCode, r.imageVersion))

	stackClient := stack.New(r.c)
	resp, err := stackClient.Add(r.ctx, stack.AddRequest{
		ServerName: r.serverName,
		Name:       r.stackName,
		// Label MUST be the FQDN — the API rejects non-FQDN labels
		// with "Error: Unable to add stack, the hostname is invalid".
		Label:         r.siteHost,
		DockerCompose: composeYAML,
	})
	if err != nil {
		return fmt.Errorf("cloud.stack.Add: %w", err)
	}
	r.createdStack = true
	if resp.Return.ID > 0 {
		if err := waitForJob(r.ctx, r.c, resp.Return.ID, resp.Return.Type, jobTimeout); err != nil {
			return fmt.Errorf("waiting for stack add: %w", err)
		}
	}
	log.Printf("    stack %q deployed; URL %s", r.stackName, r.siteURL)
	return nil
}

// probeExtensions auto-provisions a container SSH user via
// cloud.ssh.user.Add (same ssh.key authorized for it), SFTP-deploys
// a phpinfo probe, HTTP-fetches it, and asserts both extensions are
// runtime-loaded.
func (r *runner) probeExtensions() error {
	// 1. Provision a transient container SSH user scoped to our stack,
	//    using the same ssh.key.Create-registered key.
	r.containerUsername = "g" + randHex(7) // SSH usernames are length-limited
	addResp, err := cloudSSHUser.New(r.c).Add(r.ctx, cloudSSHUser.AddRequest{
		ServerName: r.serverName,
		Username:   r.containerUsername,
		Containers: []string{r.stackName},
		SSHKeys:    []string{strconv.Itoa(r.keyID)},
	})
	if err != nil {
		return fmt.Errorf("cloud.ssh.user.Add: %w", err)
	}
	r.createdContainerKey = true
	if addResp.Return.ID > 0 {
		if err := waitForJob(r.ctx, r.c, addResp.Return.ID, addResp.Return.Type, jobTimeout); err != nil {
			return fmt.Errorf("cloud.ssh.user.Add job: %w", err)
		}
	}
	log.Printf("    container ssh user %q scoped to stack %q", r.containerUsername, r.stackName)

	// 2. SFTP a phpinfo probe into /container/application/public/.
	const phpinfo = `<?php
$mp = extension_loaded('mailparse');
$yaml = extension_loaded('yaml');
header('Content-Type: text/plain');
echo "mailparse=" . ($mp ? "true" : "false") . "\n";
echo "yaml=" . ($yaml ? "true" : "false") . "\n";
echo "php_version=" . PHP_VERSION . "\n";
echo "extension_dir=" . ini_get('extension_dir') . "\n";
echo "loaded_inis=" . php_ini_loaded_file() . "\n";
echo "scanned_inis=" . php_ini_scanned_files() . "\n";
if (!$mp || !$yaml) { http_response_code(500); }
`

	if err := sftpDeploy(r.siteIP, r.containerUsername, r.keyPath, "/container/application/public/phpinfo.php", []byte(phpinfo)); err != nil {
		return fmt.Errorf("sftp deploy: %w", err)
	}

	// Give nginx-proxy a moment to map the new VIRTUAL_HOST.
	time.Sleep(5 * time.Second)
	body, err := httpGet(r.siteURL + "/phpinfo.php")
	if err != nil {
		return fmt.Errorf("http GET: %w", err)
	}
	log.Printf("    phpinfo body:\n%s", body)
	if !strings.Contains(body, "mailparse=true") || !strings.Contains(body, "yaml=true") {
		return fmt.Errorf("extensions not loaded — body: %s", body)
	}
	log.Printf("    ✓ both extensions loaded at runtime")
	return nil
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}

// buildWWWCompose constructs the docker-compose body for a www-type
// stack with the standard SiteHost volume mounts and nginx-proxy
// routing labels. Mirrors the working pattern from
// examples/build-a-site/buildWWWCompose, simplified to a single
// hostname (no LE / multi-vhost handling).
func buildWWWCompose(name, hostname, image string) string {
	vhosts := hostname + ",www." + hostname
	return fmt.Sprintf(`version: '2.1'
services:
    %s:
        container_name: %s
        environment:
            - 'VIRTUAL_HOST=%s'
            - CERT_NAME=%s
        expose:
            - 80/tcp
        image: '%s'
        labels:
            - nz.sitehost.container.label=%s
            - nz.sitehost.container.type=www
            - nz.sitehost.container.monitored=True
            - 'nz.sitehost.container.website.vhosts=%s'
            - nz.sitehost.container.image_update=True
            - nz.sitehost.container.production_mode=False
            - nz.sitehost.container.backup_disable=False
        restart: unless-stopped
        volumes:
            - '/data/docker0/www/%s/config:/container/config:ro'
            - '/data/docker0/www/%s/logs:/container/logs:rw'
            - '/data/docker0/www/%s/crontabs:/cron:ro'
            - '/data/docker0/www/%s/application:/container/application:rw'
            - '/data/docker0/www/%s/system:/container/system:rw'
networks:
    default:
        external:
            name: infra_default
`,
		name, name,
		vhosts,
		hostname,
		image,
		hostname,
		vhosts,
		name, name, name, name, name,
	)
}

func (r *runner) cleanup() {
	if r.keep {
		log.Printf("JOURNEY_KEEP=1 — leaving image, stack, and SSH keys in place")
		return
	}
	log.Printf("==> cleanup")

	// Two-phase ssh-user delete: first call clears scoping, second
	// removes the user. The first call queues a job; if we hit the
	// second too quickly we get "job already running on this user".
	// 10s between phases is enough for the first job to settle.
	if r.createdContainerKey {
		for i := 0; i < 2; i++ {
			if _, err := cloudSSHUser.New(r.c).Delete(r.ctx, cloudSSHUser.DeleteRequest{
				ServerName: r.serverName, Username: r.containerUsername,
			}); err != nil {
				log.Printf("    cloud.ssh.user.Delete (phase %d): %v", i+1, err)
				break
			}
			if i == 0 {
				time.Sleep(10 * time.Second)
			}
		}
		log.Printf("    deleted container ssh user %s", r.containerUsername)
	}
	if r.createdStack {
		stackResp, err := stack.New(r.c).Delete(r.ctx, stack.DeleteRequest{
			ServerName: r.serverName, Name: r.stackName,
		})
		if err != nil {
			log.Printf("    cloud.stack.Delete: %v", err)
		} else {
			if stackResp.Return.ID > 0 {
				if err := waitForJob(r.ctx, r.c, stackResp.Return.ID, stackResp.Return.Type, jobTimeout); err != nil {
					log.Printf("    cloud.stack.Delete job: %v", err)
				}
			}
			log.Printf("    deleted stack %s", r.stackName)
		}
	}
	if r.createdImage && !r.keepImage {
		// DeleteAndWait absorbs the "could not delete your custom
		// image right now" transient.
		if err := cloudImage.New(r.c).DeleteAndWait(r.ctx, r.imageCode, 5, jobTimeout, 10*time.Second); err != nil {
			log.Printf("    cloud.image.DeleteAndWait: %v", err)
		} else {
			log.Printf("    deleted image %s", r.imageCode)
		}
	} else if r.createdImage && r.keepImage {
		log.Printf("    JOURNEY_KEEP_IMAGE=1 — leaving image %s", r.imageCode)
	}
	if r.provisionedCCS && !r.keepCCS {
		// Force=true adds force_delete=1, required because the
		// platform auto-deploys an infra stack on every fresh CCS
		// that plain Delete refuses to remove. (See
		// docs/open-api-questions.md "Server.Delete force option".)
		delResp, err := server.New(r.c).Delete(r.ctx, server.DeleteRequest{
			Name: r.serverName, Force: true,
		})
		if err != nil {
			log.Printf("    server.Delete: %v", err)
		} else {
			if delResp.Return.ID > 0 {
				if werr := waitForJob(r.ctx, r.c, delResp.Return.ID, delResp.Return.Type, ccsProvisionTimeout); werr != nil {
					log.Printf("    server.Delete job: %v", werr)
				}
			}
			log.Printf("    deprovisioned CCS %s", r.serverName)
		}
	} else if r.provisionedCCS && r.keepCCS {
		log.Printf("    JOURNEY_KEEP_CCS=1 — leaving CCS %s", r.serverName)
	}
	if r.createdSSHKey {
		_, err := sshKey.New(r.c).Delete(r.ctx, sshKey.DeleteRequest{ID: strconv.Itoa(r.keyID)})
		if err != nil {
			log.Printf("    ssh.key.Delete: %v", err)
		} else {
			log.Printf("    deleted ssh.key %d", r.keyID)
		}
	}
	if r.repoDir != "" {
		_ = os.RemoveAll(r.repoDir)
	}
	if r.keyPath != "" {
		_ = os.Remove(r.keyPath)
	}
}

// runGit invokes git in the given working dir with GIT_SSH_COMMAND
// pointed at our ephemeral private key. Optionally injects -o
// ProxyJump=<spec> when JOURNEY_GIT_PROXY_JUMP is set, so consumers
// outside the SiteHost / NZ network can route through a bastion
// without changing the helper-constructed clone URL.
func (r *runner) runGit(dir string, args ...string) error {
	sshCmd := fmt.Sprintf("ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", r.keyPath)
	if pj := os.Getenv("JOURNEY_GIT_PROXY_JUMP"); pj != "" {
		sshCmd += " -o ProxyJump=" + pj
	}
	cmd := exec.CommandContext(r.ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(out))
	}
	return nil
}

// waitForJob polls cloud/job/get until the named job completes.
// Bare-bones reimplementation; the more elaborate exp-backoff and
// "Pending" handling lives in build-a-site.
func waitForJob(ctx context.Context, c *api.Client, id int, jobType string, timeout time.Duration) error {
	jc := job.New(c)
	deadline := time.Now().Add(timeout)
	for {
		resp, err := jc.Get(ctx, job.GetRequest{ID: id, Type: jobType})
		if err != nil {
			return err
		}
		switch resp.Return.State {
		case "Completed":
			return nil
		case "Failed":
			return fmt.Errorf("job %d failed: %s", id, resp.Return.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("job %d timed out (state=%s)", id, resp.Return.State)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jobPollInterval):
		}
	}
}

func httpGet(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func sftpDeploy(host, user, keyPath, remotePath string, data []byte) error {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read key: %w", err)
	}
	signer, err := gossh.ParsePrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	sshConn, err := gossh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer sshConn.Close()
	sftpClient, err := sftp.NewClient(sshConn)
	if err != nil {
		return fmt.Errorf("sftp.NewClient: %w", err)
	}
	defer sftpClient.Close()
	f, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp.Create %s: %w", remotePath, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("sftp.Write: %w", err)
	}
	return nil
}

func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x", b)
}

// openSSHPrivateKey encodes an ed25519 private key in OpenSSH PEM
// format using the upstream ssh.MarshalPrivateKey helper. (Earlier
// hand-rolled binary serialisation produced files that
// /usr/bin/ssh rejected as "invalid format".)
func openSSHPrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	block, err := gossh.MarshalPrivateKey(priv, "gosh-customimg")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

