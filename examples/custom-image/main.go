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
//	SH_CLIENT_ID              — sub-account targeting; otherwise discovered
//	SH_CCS_NAME               — name of an existing CCS to deploy on. If
//	                            unset, the example picks the first CCS
//	                            from cloud.server.List (read-only — never
//	                            provisions a CCS itself, since this example
//	                            is already long-running).
//	JOURNEY_KEEP=1            — leave the image, stack, and SSH keys in
//	                            place after the run for inspection.
//	                            Default: cleanup.
//	JOURNEY_PARENT_IMAGE      — override the parent image code
//	                            (default: sitehost-php85-apache).
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
//   - Outbound SSH to gitlab-clients.sitehost.co.nz:22.
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

	"golang.org/x/crypto/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"

	"github.com/sitehostnz/gosh/pkg/api"
	cloudImage "github.com/sitehostnz/gosh/pkg/api/cloud/image"
	imgVersion "github.com/sitehostnz/gosh/pkg/api/cloud/image/version"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	"github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
)

const (
	defaultParentImage = "sitehost-php85-apache"
	imageBuildTimeout  = 15 * time.Minute
	imageBuildPoll     = 15 * time.Second
	jobPollInterval    = 5 * time.Second
	jobTimeout         = 10 * time.Minute
)

func main() {
	ctx := context.Background()
	r := &runner{ctx: ctx}

	if err := r.setup(); err != nil {
		log.Fatalf("setup: %v", err)
	}

	defer r.cleanup()

	steps := []struct {
		name string
		fn   func() error
	}{
		{"register SSH key", r.registerSSHKey},
		{"locate target CCS", r.locateCCS},
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
			log.Fatalf("%s: %v", s.name, err)
		}
	}

	log.Printf("✓ custom-image example completed successfully")
	log.Printf("  image code:   %s", r.imageCode)
	log.Printf("  stack:        %s on %s", r.stackName, r.serverName)
	log.Printf("  smoke URL:    %s", r.siteURL)
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
	stackName string
	siteURL   string
	siteHost  string
	siteIP    string

	// Cleanup flags
	keep                bool
	createdSSHKey       bool
	createdContainerKey bool
	createdImage        bool
	createdStack        bool
}

func (r *runner) setup() error {
	apiKey := os.Getenv("SH_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("SH_API_KEY required")
	}
	clientID := os.Getenv("SH_CLIENT_ID")

	r.keep = os.Getenv("JOURNEY_KEEP") == "1"
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

// locateCCS picks the target CCS: env override or the first CCS
// returned by cloud.server.List. Read-only — this example assumes a
// CCS already exists. Provisioning a fresh one would more than
// double the example's runtime.
func (r *runner) locateCCS() error {
	if name := os.Getenv("SH_CCS_NAME"); name != "" {
		r.serverName = name
		log.Printf("    using CCS %q from SH_CCS_NAME", name)
		return nil
	}
	servers, err := cloudServer.New(r.c).List(r.ctx)
	if err != nil {
		return fmt.Errorf("cloud.server.List: %w", err)
	}
	if len(servers.CloudServers) == 0 {
		return fmt.Errorf("no Cloud Container Servers found; set SH_CCS_NAME to target one explicitly")
	}
	r.serverName = servers.CloudServers[0].Name
	log.Printf("    auto-selected CCS %q", r.serverName)
	return nil
}

// forkImage uses the cloud.image.ForkFromImage helper, which
// resolves the public parent's numeric id by listing then calls
// cloud.image.Create. We then poll until the create-image job is
// done, since the GitLab repository is provisioned asynchronously
// and we can't clone before that completes.
func (r *runner) forkImage() error {
	imgClient := cloudImage.New(r.c)
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
// directive to the Dockerfile.
func (r *runner) cloneAndEdit() error {
	cloneURL := cloudImage.New(r.c).CloneURL(r.imageCode)
	log.Printf("    clone URL: %s", cloneURL)

	dir, err := os.MkdirTemp("", "gosh-customimg-*")
	if err != nil {
		return fmt.Errorf("mktmp: %w", err)
	}
	r.repoDir = dir

	// GitLab repo provisioning is sometimes ready a beat after the job
	// reports done. A small retry softens that race.
	var cloneErr error
	for i := 0; i < 6; i++ {
		cloneErr = r.runGit(dir, "clone", cloneURL, ".")
		if cloneErr == nil {
			break
		}
		log.Printf("    clone attempt %d failed (%v); retrying", i+1, cloneErr)
		time.Sleep(10 * time.Second)
	}
	if cloneErr != nil {
		return fmt.Errorf("git clone: %w", cloneErr)
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
	patch := `

# gosh:custom-image marker — added by examples/custom-image
# Installs the PECL mailparse + yaml extensions and enables them
# for the system PHP. libyaml-dev is required at build time for
# the yaml extension; we leave it in place since pecl re-runs may
# need it.
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        libyaml-dev php-pear php-dev \
    && pecl install mailparse yaml \
    && phpenmod -v ALL mailparse yaml \
    && rm -rf /var/lib/apt/lists/*
`
	combined := append(current, []byte(patch)...)
	if err := os.WriteFile(dockerfilePath, combined, 0o644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	log.Printf("    Dockerfile patched (+%d bytes)", len(patch))
	return nil
}

// commitAndPush stages, commits, and pushes the Dockerfile change.
// The push triggers SiteHost's GitLab CI to build the image.
func (r *runner) commitAndPush() error {
	if err := r.runGit(r.repoDir, "config", "user.email", "gosh-example@sitehost.nz"); err != nil {
		return fmt.Errorf("git config email: %w", err)
	}
	if err := r.runGit(r.repoDir, "config", "user.name", "gosh custom-image example"); err != nil {
		return fmt.Errorf("git config name: %w", err)
	}
	if err := r.runGit(r.repoDir, "add", "Dockerfile"); err != nil {
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
func (r *runner) waitForBuild() error {
	v, err := cloudImage.New(r.c).WaitForBuild(r.ctx, r.imageID, imageBuildTimeout, imageBuildPoll)
	if err != nil {
		return fmt.Errorf("WaitForBuild: %w", err)
	}
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
	r.siteHost = fmt.Sprintf("%s.%s.sth.nz", r.stackName, r.siteIP)
	r.siteURL = "http://" + r.siteHost

	// docker-compose body for a single web container using our custom image.
	composeYAML := fmt.Sprintf(`version: '3.7'
services:
  %s:
    container_name: %s
    image: registry-clients.sitehost.co.nz/%s/%s
    restart: always
    environment:
      VIRTUAL_HOST: %s
    labels:
      nz.sitehost.container.image_update: "True"
      nz.sitehost.container.label: "%s"
      nz.sitehost.container.monitored: "True"
      nz.sitehost.container.type: "www"
networks:
  default:
    external:
      name: infra_default
`, r.stackName, r.stackName, "g_"+r.c.ClientID, r.imageCode, r.siteHost, r.imageLabel)

	stackClient := stack.New(r.c)
	resp, err := stackClient.Add(r.ctx, stack.AddRequest{
		ServerName:  r.serverName,
		Name:        r.stackName,
		Label:       r.imageLabel,
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

// probeExtensions SFTP-deploys a tiny phpinfo.php into the
// container's public directory, then HTTP-fetches it and asserts
// that both target extensions are loaded.
func (r *runner) probeExtensions() error {
	// We need a container SSH user to SFTP. Add the same key.
	// (cloud.ssh.user lives in pkg/api/cloud/ssh/user; reusing the
	// exact AddRequest plumbing from build-a-site is overkill here —
	// for brevity we rely on the customer already having a
	// container-user key on the CCS, OR a SFTP path via the platform.
	// If your CCS doesn't, set SH_CCS_NAME to one that does or
	// extend this example with cloud.ssh.user.Add.)
	//
	// Practically: most SiteHost CCSes already have a default SSH
	// user; the example uses the locally-stored key + the env-supplied
	// SFTP_USER (defaults to the container's stack name).
	sftpUser := os.Getenv("SH_SFTP_USER")
	if sftpUser == "" {
		return fmt.Errorf("SH_SFTP_USER required (a container SSH user authorized for the test CCS)")
	}

	const phpinfo = `<?php
$mp = extension_loaded('mailparse');
$yaml = extension_loaded('yaml');
header('Content-Type: text/plain');
echo "mailparse=" . ($mp ? "true" : "false") . "\n";
echo "yaml=" . ($yaml ? "true" : "false") . "\n";
if (!$mp || !$yaml) { http_response_code(500); }
`

	if err := sftpDeploy(r.siteIP, sftpUser, r.keyPath, "/container/application/public/phpinfo.php", []byte(phpinfo)); err != nil {
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
	log.Printf("    ✓ both extensions loaded")
	return nil
}

func (r *runner) cleanup() {
	if r.keep {
		log.Printf("JOURNEY_KEEP=1 — leaving image, stack, and SSH keys in place")
		return
	}
	log.Printf("==> cleanup")

	if r.createdStack {
		_, err := stack.New(r.c).Delete(r.ctx, stack.DeleteRequest{
			ServerName: r.serverName, Name: r.stackName,
		})
		if err != nil {
			log.Printf("    cloud.stack.Delete: %v", err)
		} else {
			log.Printf("    deleted stack %s", r.stackName)
		}
	}
	if r.createdImage {
		_, err := cloudImage.New(r.c).Delete(r.ctx, cloudImage.DeleteRequest{Code: r.imageCode})
		if err != nil {
			log.Printf("    cloud.image.Delete: %v", err)
		} else {
			log.Printf("    deleted image %s", r.imageCode)
		}
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
// pointed at our ephemeral private key (so the GitLab clone/push
// authenticates as the registered ssh.key).
func (r *runner) runGit(dir string, args ...string) error {
	cmd := exec.CommandContext(r.ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", r.keyPath),
	)
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
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("parse key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	sshConn, err := ssh.Dial("tcp", host+":22", cfg)
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
// format. Stripped-down from build-a-site's helper of the same shape.
func openSSHPrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	const magic = "openssh-key-v1\x00"
	pub := priv.Public().(ed25519.PublicKey)
	pubW := ssh.Marshal(struct {
		KeyType string
		Pub     []byte
	}{"ssh-ed25519", pub})
	checkint := uint32(time.Now().UnixNano())
	privW := ssh.Marshal(struct {
		Check1, Check2 uint32
		Keytype        string
		Pub            []byte
		Priv           []byte
		Comment        string
	}{checkint, checkint, "ssh-ed25519", pub, []byte(priv), "gosh-customimg"})
	// pad to 8-byte multiple
	for len(privW)%8 != 0 {
		privW = append(privW, byte(len(privW)%8+1))
	}
	header := struct {
		CipherName, KdfName string
		KdfOpts             string
		NumKeys             uint32
		PubKey              []byte
		PrivBlob            []byte
	}{"none", "none", "", 1, pubW, privW}
	body := append([]byte(magic), ssh.Marshal(header)...)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "OPENSSH PRIVATE KEY",
		Bytes: body,
	}), nil
}

