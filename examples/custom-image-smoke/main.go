// Program custom-image-smoke is a minimal round-trip check of the
// custom-image build cycle. It does NOT deploy a stack or probe a
// running container — purely:
//
//  1. Generate an ed25519 SSH keypair, register the public half via
//     ssh.key.Create.
//  2. Fork the parent image (default: sitehost-php85-apache) into a
//     temporary customer image.
//  3. Wait for the create-image job (so the GitLab repo is ready).
//  4. Clone the repo, append a RUN line to the Dockerfile that echoes
//     a unique marker string, commit, push.
//  5. Poll cloud.image.version.list_all via the WaitForBuild helper
//     until the build is success or failed.
//  6. Fetch the build trace via cloud.image.version.GetBuild and
//     assert the marker appears in it.
//  7. Cleanup: delete image (waits for the scheduler delete job)
//     + ssh.key (unless JOURNEY_KEEP=1).
//
// This proves the round-trip mechanically before tackling the
// harder content-specific tests (PECL extensions, runtime probes).
//
// # Network requirements
//
// gitlab-clients.sitehost.co.nz:22 (the SSH endpoint for custom
// image repositories) appears to be geo-restricted: connections
// from outside SiteHost's New Zealand network are blocked at the
// firewall (TCP refused on port 22). For AI agents driving this
// SDK from outside NZ:
//
//   - Easiest: SSH-tunnel via a SiteHost VM by setting
//     JOURNEY_GIT_PROXY_JUMP=user@host (e.g.
//     "ubuntu@45.113.8.110"). The smoke injects -o ProxyJump=…
//     into GIT_SSH_COMMAND so all git+ssh ops route through the
//     bastion transparently.
//   - Alternative: open a SiteHost support ticket to allow the
//     consumer's IP to reach gitlab-clients.sitehost.co.nz:22 directly.
//   - Or: run this example from inside a SiteHost (NZ) VM in the
//     first place.
//
// HTTPS git over port 443 may also be available (the host responds
// on 443) but we haven't validated PAT-based auth via HTTPS.
//
// Required env:
//
//	SH_API_KEY                — your API key.
//
// Optional env:
//
//	SH_CLIENT_ID              — sub-account; otherwise discovered
//	                            via info.NewClientWithDiscovery.
//	JOURNEY_KEEP=1            — leave the image and SSH key in place
//	                            after the run for inspection.
//	JOURNEY_PARENT_IMAGE      — override the parent image code
//	                            (default: sitehost-php85-apache).
//	JOURNEY_GIT_PROXY_JUMP    — SSH ProxyJump spec to route git+ssh
//	                            through a bastion (see above).
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/sitehostnz/gosh/pkg/api"
	cloudImage "github.com/sitehostnz/gosh/pkg/api/cloud/image"
	imgVersion "github.com/sitehostnz/gosh/pkg/api/cloud/image/version"
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
	if err := run(); err != nil {
		log.Fatalf("smoke: %v", err)
	}
}

// run owns the full lifecycle. Using a return-error pattern (rather
// than log.Fatalf throughout) keeps deferred cleanup honest:
// log.Fatalf calls os.Exit which skips defers, leaking resources.
func run() error {
	ctx := context.Background()
	apiKey := os.Getenv("SH_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("SH_API_KEY required")
	}
	keep := os.Getenv("JOURNEY_KEEP") == "1"
	parentImage := os.Getenv("JOURNEY_PARENT_IMAGE")
	if parentImage == "" {
		parentImage = defaultParentImage
	}

	var c *api.Client
	if cid := os.Getenv("SH_CLIENT_ID"); cid != "" {
		var err error
		c, err = api.New(apiKey, cid)
		if err != nil {
			return fmt.Errorf("api.New: %w", err)
		}
	} else {
		var err error
		c, err = info.NewClientWithDiscovery(ctx, apiKey)
		if err != nil {
			return fmt.Errorf("client discovery: %w", err)
		}
	}
	log.Printf("client_id=%s", c.ClientID)

	suffix := randomSuffix()
	keyName := "gosh-smoke-" + suffix
	imageCode := "gosh-smoke-" + suffix
	imageLabel := "gosh smoke " + suffix
	marker := fmt.Sprintf("Hello world from gosh smoke %s", suffix)

	var (
		keyID         int
		keyPath       string
		createdKey    bool
		createdImage  bool
	)

	cleanup := func() {
		if keep {
			log.Printf("JOURNEY_KEEP=1 — leaving image and ssh.key in place")
			return
		}
		log.Printf("==> cleanup")
		if createdImage {
			// cloud.image.Delete returns a scheduler job; the record
			// remains visible until that job completes. Without the
			// wait, repeated runs accumulate orphan images.
			resp, err := cloudImage.New(c).Delete(ctx, cloudImage.DeleteRequest{Code: imageCode})
			if err != nil {
				log.Printf("    cloud.image.Delete: %v", err)
			} else if resp.Return.ID > 0 {
				if werr := waitForJob(ctx, c, resp.Return.ID, resp.Return.Type, jobTimeout); werr != nil {
					log.Printf("    wait for image delete: %v", werr)
				} else {
					log.Printf("    deleted image %s", imageCode)
				}
			}
		}
		if createdKey {
			if _, err := sshKey.New(c).Delete(ctx, sshKey.DeleteRequest{ID: strconv.Itoa(keyID)}); err != nil {
				log.Printf("    ssh.key.Delete: %v", err)
			} else {
				log.Printf("    deleted ssh.key %d", keyID)
			}
		}
		if keyPath != "" {
			_ = os.Remove(keyPath)
		}
	}
	defer cleanup()

	// 1. SSH key
	log.Printf("==> register SSH key (%s)", keyName)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("ed25519: %w", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	pubLine := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub)))
	pemBytes, err := openSSHPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	keyPath = filepath.Join(os.TempDir(), keyName+".key")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	keyResp, err := sshKey.New(c).Create(ctx, sshKey.CreateRequest{
		Label: keyName, Content: pubLine,
	})
	if err != nil {
		return fmt.Errorf("ssh.key.Create: %w", err)
	}
	keyID, err = strconv.Atoi(keyResp.Return.KeyID)
	if err != nil {
		return fmt.Errorf("non-numeric ssh.key id %q: %w", keyResp.Return.KeyID, err)
	}
	createdKey = true
	log.Printf("    ssh.key id=%d", keyID)

	// 2. Fork
	log.Printf("==> fork %s", parentImage)
	imgClient := cloudImage.New(c)
	forkResp, err := imgClient.ForkFromImage(ctx, parentImage, imageLabel, imageCode, []int{keyID})
	if err != nil {
		return fmt.Errorf("ForkFromImage: %w", err)
	}
	createdImage = true
	if forkResp.Return.ID > 0 {
		log.Printf("    fork job id=%d type=%s", forkResp.Return.ID, forkResp.Return.Type)
		if err := waitForJob(ctx, c, forkResp.Return.ID, forkResp.Return.Type, jobTimeout); err != nil {
			return fmt.Errorf("fork job: %w", err)
		}
	}

	// Resolve numeric image id for WaitForBuild
	got, err := imgClient.Get(ctx, cloudImage.GetRequest{Code: imageCode})
	if err != nil {
		return fmt.Errorf("cloud.image.Get: %w", err)
	}
	imageID, err := strconv.Atoi(got.Image.ID)
	if err != nil {
		return fmt.Errorf("non-numeric image id %q: %w", got.Image.ID, err)
	}
	log.Printf("    image id=%d code=%s", imageID, imageCode)

	// 3. Clone
	cloneURL := imgClient.CloneURL(imageCode)
	log.Printf("==> clone %s", cloneURL)
	repoDir, err := os.MkdirTemp("", "gosh-smoke-*")
	if err != nil {
		return fmt.Errorf("mktmp: %w", err)
	}
	defer os.RemoveAll(repoDir)

	sshCmd := "ssh -i " + keyPath + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new"
	if pj := os.Getenv("JOURNEY_GIT_PROXY_JUMP"); pj != "" {
		// Route git+ssh through a bastion (e.g. ubuntu@45.113.8.110)
		// for callers reaching the firewalled gitlab-clients host
		// from outside the SiteHost / NZ network.
		sshCmd += " -o ProxyJump=" + pj
		log.Printf("    using ProxyJump %s", pj)
	}
	gitEnv := append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd)

	var cloneErr error
	for i := 0; i < 6; i++ {
		cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, ".")
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err == nil {
			cloneErr = nil
			break
		}
		cloneErr = fmt.Errorf("%w\n%s", err, string(out))
		log.Printf("    clone attempt %d failed; retrying in 10s", i+1)
		time.Sleep(10 * time.Second)
	}
	if cloneErr != nil {
		return fmt.Errorf("git clone: %w", cloneErr)
	}

	// 4. Patch Dockerfile
	dockerfilePath := filepath.Join(repoDir, "Dockerfile")
	current, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return fmt.Errorf("read Dockerfile: %w", err)
	}
	patch := fmt.Sprintf("\n# gosh-smoke marker\nRUN echo %q\n", marker)
	if err := os.WriteFile(dockerfilePath, append(current, []byte(patch)...), 0o644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	log.Printf("    appended marker: %s", marker)

	// 5. Commit + push
	log.Printf("==> commit + push")
	for _, args := range [][]string{
		{"config", "user.email", "gosh-smoke@sitehost.nz"},
		{"config", "user.name", "gosh smoke"},
		{"add", "Dockerfile"},
		{"commit", "-m", "gosh smoke: echo marker"},
		{"push", "origin", "HEAD"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		log.Printf("    git %s\n%s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		if err != nil && args[0] != "commit" { // ignore "nothing to commit"
			return fmt.Errorf("git %s: %w", args[0], err)
		}
	}

	// 6. WaitForBuild
	log.Printf("==> WaitForBuild (timeout %s)", imageBuildTimeout)
	v, err := imgClient.WaitForBuild(ctx, imageID, imageBuildTimeout, imageBuildPoll)
	if err != nil {
		return fmt.Errorf("WaitForBuild: %w", err)
	}
	log.Printf("    build_id=%s status=%s version=%s", v.BuildID, v.BuildStatus, v.Version)

	// 7. Fetch trace + assert marker
	log.Printf("==> GetBuild trace")
	trace, err := imgVersion.New(c).GetBuild(ctx, imgVersion.GetBuildRequest{
		Code: imageCode, BuildID: v.BuildID,
	})
	if err != nil {
		return fmt.Errorf("GetBuild: %w", err)
	}
	log.Printf("--- build trace (status=%s) ---", trace.Return.BuildStatus)
	log.Printf("%s", trace.Return.BuildTrace)
	log.Printf("--- end trace ---")

	if v.BuildStatus != cloudImage.BuildStatusSuccess {
		return fmt.Errorf("build did not succeed: %s", v.BuildStatus)
	}
	if !strings.Contains(trace.Return.BuildTrace, marker) {
		return fmt.Errorf("marker %q NOT found in build trace", marker)
	}
	log.Printf("✓ marker %q found in build trace — full round trip verified", marker)
	return nil
}

func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x", b)
}

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

// openSSHPrivateKey encodes an ed25519 private key in OpenSSH PEM
// format.
func openSSHPrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	const magic = "openssh-key-v1\x00"
	pub := priv.Public().(ed25519.PublicKey)
	pubW := gossh.Marshal(struct {
		KeyType string
		Pub     []byte
	}{"ssh-ed25519", pub})
	checkint := uint32(time.Now().UnixNano())
	privW := gossh.Marshal(struct {
		Check1, Check2 uint32
		Keytype        string
		Pub            []byte
		Priv           []byte
		Comment        string
	}{checkint, checkint, "ssh-ed25519", pub, []byte(priv), "gosh-smoke"})
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
	body := append([]byte(magic), gossh.Marshal(header)...)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "OPENSSH PRIVATE KEY",
		Bytes: body,
	}), nil
}
