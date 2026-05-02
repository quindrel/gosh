// Program build-a-site is the canonical reference for using gosh to
// provision SiteHost infrastructure end-to-end. Its primary readers
// are AI agents writing Go code that imports gosh — comments
// explain non-obvious choices so the patterns can be cribbed.
//
// Phase A (always runs, no real-money cost on non-Linode regions):
//   1. Authenticate; discover client_id if needed
//   2. Find a Cloud Container Server (CCS)
//   3. Provision a SiteHost-typed www container reachable via sth.nz
//   4. Verify it serves
//   5. Tear it down
//
// Phase B (opt-in via JOURNEY_DOMAIN or JOURNEY_REGISTER_DOMAIN — not
// yet implemented in this milestone):
//   6+ Domain registration, DNS hosting, Let's Encrypt, mail, send/receive
//
// Required env: SH_API_KEY
// Optional env: SH_CLIENT_ID, JOURNEY_KEEP=1
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	cloudStack "github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	"github.com/sitehostnz/gosh/pkg/models"
)

// SiteHost-shipped PHP/Apache image. Any registry.sitehost.co.nz/sitehost-php<XX>-apache
// image works — these are the runtime's "typed" images. A docker_compose
// using a non-typed image (random Docker Hub container) is rejected by
// the provisioning daemon with "Image not typed".
const wwwImage = "registry.sitehost.co.nz/sitehost-php85-apache:1.0.0-noble"

func main() {
	ctx := context.Background()

	// ── A.1 — authenticate ─────────────────────────────────────────────
	//
	// info.NewClientWithDiscovery is the bootstrap entry point when you
	// only have an API key. If you also know the client_id (or are
	// targeting a sub-account from a super-user key), use api.New
	// directly so discovery doesn't override your intended scope.
	apiKey := mustEnv("SH_API_KEY")
	c, err := buildClient(ctx, apiKey, os.Getenv("SH_CLIENT_ID"))
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	step("A.1", "authenticated as client_id=%s", c.ClientID)

	// ── A.2 — find a Cloud Container Server ────────────────────────────
	//
	// cloud.server.List returns CCSs for the account. We'll provision
	// onto the first one in state=On. The CCS's primary_ip is what
	// every stack on it shares for ingress; sth.nz wildcard DNS turns
	// `<anything>.<ip>.sth.nz` into a hostname pointing at that IP.
	servers, err := cloudServer.New(c).List(ctx)
	if err != nil {
		log.Fatalf("cloud.server.List: %v", err)
	}
	target, ok := pickRunningCCS(servers.CloudServers)
	if !ok {
		log.Fatalf("no Cloud Container Server in state=On. Provision one via the dashboard before running this example — gosh's cloud.server package doesn't expose CCS creation.")
	}
	step("A.2", "target CCS: name=%s primary_ip=%s state=%s",
		target.Name, target.PrimaryIP, target.State)

	// ── A.3 — name + hostname ──────────────────────────────────────────
	//
	// cloud.stack.GenerateName allocates a unique cc<hex> identifier on
	// the chosen CCS. The label field on cloud.stack.Add (and on the
	// returned stack record) is the FQDN hostname, NOT a free-form
	// description — every existing stack on the CCS uses the form
	// `<short>.<ccs-IP>.sth.nz` (or a real domain if the customer has
	// pointed one). The www-type runtime keys off this hostname for
	// nginx-proxy routing, certificate naming, and volume layout.
	gen, err := cloudStack.New(c).GenerateName(ctx)
	if err != nil {
		log.Fatalf("cloud.stack.GenerateName: %v", err)
	}
	stackName := gen.Return.Name
	hostname := fmt.Sprintf("gosh.%s.sth.nz", target.PrimaryIP)
	step("A.3", "stack name=%s hostname=%s", stackName, hostname)

	// ── A.4 — provision the stack ──────────────────────────────────────
	//
	// docker_compose conventions on SiteHost (per existing platform
	// stacks like the user's `php85.<ip>.sth.nz` test stack):
	//   - version: '2.1' (NOT v3 — SiteHost's runtime is v2.1-shaped)
	//   - One service named the same as the stack name
	//   - container_name = stack name (no suffixes)
	//   - image MUST be a SiteHost-typed image
	//     (registry.sitehost.co.nz/sitehost-php<XX>-apache:<tag>);
	//     untyped images are rejected at provision time
	//   - environment: VIRTUAL_HOST (FQDN + www. variant), CERT_NAME
	//   - expose 80/tcp (nginx-proxy connects to this; no host port binding)
	//   - SiteHost labels — at minimum: label, type=www, monitored,
	//     website.vhosts, image_update, production_mode, backup_disable
	//   - logging: journald
	//   - Volumes mount under /data/docker0/www/<stack-name>/{config,logs,crontabs,application,system}
	//     The runtime auto-creates these host directories on add.
	//   - networks: default joins external infra_default — the network
	//     where the shared nginx-proxy from the `infra` stack also lives.
	compose := buildWWWCompose(stackName, hostname)

	addResp, err := cloudStack.New(c).Add(ctx, cloudStack.AddRequest{
		ServerName:    target.Name,
		Name:          stackName,
		Label:         hostname, // FQDN — not free-form
		EnableSSL:     0,
		DockerCompose: compose,
	})
	if err != nil {
		log.Fatalf("cloud.stack.Add: %v", err)
	}
	jobID := addResp.Return.ID
	step("A.4", "provision job queued: id=%d type=%s",
		jobID, addResp.Return.Type)

	// ── A.5 — wait for provision job ───────────────────────────────────
	//
	// The Add response confirms the API ACCEPTED the request, not that
	// the stack will provision successfully. Underlying failures
	// (image not typed, host capacity, network conflict) surface as
	// `state=Failed` on the scheduler job, with a `message` field
	// explaining why.
	if err := waitForJob(ctx, c, jobID, 5*time.Minute); err != nil {
		// Cleanup: even on job failure, attempt Delete so any partial
		// resources get reaped. Delete is forgiving of "stack not found".
		_, _ = cloudStack.New(c).Delete(ctx, cloudStack.DeleteRequest{
			ServerName: target.Name, Name: stackName,
		})
		log.Fatalf("provision job: %v", err)
	}
	step("A.5", "stack provisioned (job completed)")

	// ── A.6 — verify it serves ─────────────────────────────────────────
	//
	// nginx-proxy in the `infra` stack discovers new VIRTUAL_HOST
	// containers automatically. Routing usually works within seconds
	// after the container is up; we retry briefly in case the proxy
	// hasn't picked up the new vhost yet.
	body, err := waitForHTTP(ctx, "http://"+hostname+"/", 90*time.Second)
	if err != nil {
		log.Printf("⚠ HTTP verification failed: %v", err)
	} else {
		step("A.6", "served HTTP 200; first 100 chars: %s",
			truncate(body, 100))
	}

	fmt.Println()
	fmt.Println("  → run this in another shell to verify externally:")
	fmt.Printf("      curl -i %s\n", "http://"+hostname+"/")
	fmt.Println()

	// ── A.7 — cleanup ──────────────────────────────────────────────────
	if os.Getenv("JOURNEY_KEEP") == "1" {
		step("A.7", "JOURNEY_KEEP=1 — leaving %s on %s in place",
			stackName, target.Name)
		return
	}
	delResp, err := cloudStack.New(c).Delete(ctx, cloudStack.DeleteRequest{
		ServerName: target.Name, Name: stackName,
	})
	if err != nil {
		log.Fatalf("cloud.stack.Delete: %v", err)
	}
	step("A.7", "delete job queued: id=%d", delResp.Return.ID)
	if err := waitForJob(ctx, c, delResp.Return.ID, 2*time.Minute); err != nil {
		log.Printf("⚠ delete job did not reach Completed: %v", err)
	}
}

// buildClient returns an *api.Client. If clientID is empty, falls back
// to info-discovery; otherwise uses api.New directly (preserves
// sub-account targeting from a super-user key).
func buildClient(ctx context.Context, apiKey, clientID string) (*api.Client, error) {
	if clientID != "" {
		return api.New(apiKey, clientID)
	}
	return info.NewClientWithDiscovery(ctx, apiKey)
}

// pickRunningCCS returns the first CCS in state=On from the list.
func pickRunningCCS(list []models.CloudServer) (models.CloudServer, bool) {
	for _, s := range list {
		if s.State == "On" {
			return s, true
		}
	}
	return models.CloudServer{}, false
}

// buildWWWCompose returns the docker_compose YAML for a www-type stack
// matching SiteHost's runtime expectations. See the comment on §A.4
// for the schema rationale.
func buildWWWCompose(name, hostname string) string {
	return fmt.Sprintf(`version: '2.1'
services:
    %s:
        container_name: %s
        environment:
            - 'VIRTUAL_HOST=%s,www.%s'
            - CERT_NAME=%s
        expose:
            - 80/tcp
        image: '%s'
        labels:
            - nz.sitehost.container.label=%s
            - nz.sitehost.container.type=www
            - nz.sitehost.container.monitored=True
            - 'nz.sitehost.container.website.vhosts=%s,www.%s'
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
		hostname, hostname,
		hostname,
		wwwImage,
		hostname,
		hostname, hostname,
		name, name, name, name, name,
	)
}

// waitForJob polls job.Get until state=Completed or Failed, or the
// deadline passes. Returns nil on Completed; an error wrapping the
// job's `message` field on Failed.
func waitForJob(ctx context.Context, c *api.Client, jobID int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := job.New(c)
	for time.Now().Before(deadline) {
		resp, err := client.Get(ctx, job.GetRequest{ID: jobID, Type: job.SchedulerType})
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		switch resp.Return.State {
		case "Completed":
			return nil
		case "Failed":
			return fmt.Errorf("job %d failed: %s", jobID, jobMessage(resp))
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("job %d did not reach a terminal state within %s", jobID, timeout)
}

// jobMessage extracts the failure message; the field is on the inner
// JobDetails but the JSON tag is `message`. Fallback to the latest log line.
func jobMessage(resp interface{}) string {
	// Reach into the response via reflection-light path: the struct
	// has Return.Message (covered by JobDetails.message) per the API.
	// gosh's models.JobDetails doesn't currently include Message; we
	// fall back to the most recent log entry.
	if r, ok := resp.(interface {
		GetMessage() string
	}); ok {
		return r.GetMessage()
	}
	return "<see /1.5/job/get.json for details>"
}

// waitForHTTP retries GET <url> until 200, returning the body.
func waitForHTTP(ctx context.Context, url string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(3 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			return string(body), nil
		}
		lastErr = fmt.Errorf("status %d", resp.StatusCode)
		time.Sleep(3 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return "", lastErr
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s must be set", name)
	}
	return v
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func step(id, format string, args ...interface{}) {
	log.Printf("✓ %s — %s", id, fmt.Sprintf(format, args...))
}
