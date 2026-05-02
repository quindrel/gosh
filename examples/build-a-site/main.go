// Program build-a-site is the canonical reference for using gosh to
// provision SiteHost infrastructure end-to-end. Its primary readers
// are AI agents writing Go code that imports gosh — comments
// explain non-obvious choices so the patterns can be cribbed.
//
// Phase A (always runs, no real-money cost on non-Linode regions):
//
//  1. Authenticate (info.NewClientWithDiscovery)
//  2. Find a Cloud Container Server (cloud.server.List)
//  3. Read bandwidth baseline (bandwidth.GetUsageSummary)
//  4. Generate ephemeral SSH keypair; register the public key with
//     ssh.key.Create
//  5. Provision a www-type Cloud Container (cloud.stack.Add)
//  6. Create an application database on the existing MariaDB stack
//     (cloud.db.Add)
//  7. Create a database user with grants (cloud.db.user.Add)
//  8. Create an SSH user scoped to the new container, attaching our
//     SSH key by ID (cloud.ssh.user.Add)
//  9. SFTP into the SSH user, upload an index.php that queries the
//     database — proves web container ↔ DB ↔ deployed code
//  10. Verify HTTP returns the deployed marker
//  11. Read bandwidth again (delta typically zero — the API updates
//     slowly and a fresh stack hasn't accumulated traffic)
//  12. Cleanup, reverse order
//  13. Audit: list every namespace we touched, assert the resources
//     we created are no longer present
//
// Phase B (opt-in via JOURNEY_DOMAIN or JOURNEY_REGISTER_DOMAIN, not
// yet implemented in this milestone): registered domain, DNS hosting,
// Let's Encrypt, mail provisioning, send/receive verification.
//
// # Job convention — IMPORTANT for AI agents reading this as reference
//
// Most gosh write operations are asynchronous: they return a job id
// (Return.ID, type=scheduler) and the actual work happens on the
// SiteHost scheduler. Every write that returns a job MUST be followed
// by a job.Get poll until the job's state is "Completed" before any
// follow-up call that depends on the same resource. This program
// uses waitForJob() after each such write — search for "waitForJob"
// to see the pattern. Skipping the wait causes downstream calls to
// fail with errors like "job already running on this stack" or
// "resource not found", because the resource hasn't actually been
// created yet.
//
// Synchronous writes (e.g. cloud.ssh.user.Add) return without a job;
// the operation is complete on response. Read endpoints are likewise
// synchronous.
//
// Required env: SH_API_KEY
// Optional env: SH_CLIENT_ID (skip discovery), JOURNEY_KEEP=1 (skip cleanup)
package main

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/bandwidth"
	cloudDB "github.com/sitehostnz/gosh/pkg/api/cloud/db"
	cloudDBUser "github.com/sitehostnz/gosh/pkg/api/cloud/db/user"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	cloudSSHUser "github.com/sitehostnz/gosh/pkg/api/cloud/ssh/user"
	cloudStack "github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
	"github.com/sitehostnz/gosh/pkg/models"
)

// SiteHost-shipped PHP/Apache image. Any registry.sitehost.co.nz/sitehost-php<XX>-apache
// image works — these are the runtime's "typed" images. A docker_compose
// using a non-typed image (random Docker Hub container) is rejected by
// the provisioning daemon with state=Failed and message="Image not typed".
const wwwImage = "registry.sitehost.co.nz/sitehost-php85-apache:1.0.0-noble"

// dbHost — the MariaDB/MySQL stack on the CCS that hosts our database.
// cloud.db.* doesn't provision a new DB container; it creates a
// database within an existing one. We use the well-known mariadb1108
// stack present on every standard CCS.
const dbHost = "mariadb1108"

// markerLen — how many random hex chars in the deployed-page marker.
const markerLen = 16

// state holds everything the program creates so cleanup + audit can reach it.
type state struct {
	ccs        models.CloudServer
	hostname   string // FQDN — also used as cloud.stack label
	stackName  string // cc<hex> — also the container_name
	keyID      string // ssh.key id
	dbName     string // app DB name
	dbUser     string // app DB user
	dbPassword string // app DB password (in-memory only)
	sshUser    string // SSH user for code deploy
	sshPriv    ed25519.PrivateKey
	marker     string // unique string we expect to see served
}

func main() {
	// run() returns the exit code; we wrap in main() so deferred
	// cleanup inside run() actually fires before the process exits.
	// log.Fatal and os.Exit both skip defers — so neither is used in
	// the body.
	os.Exit(run())
}

func run() int {
	ctx := context.Background()
	apiKey := mustEnv("SH_API_KEY")

	c, err := buildClient(ctx, apiKey, os.Getenv("SH_CLIENT_ID"))
	if err != nil {
		log.Printf("Fatal: auth: %v", err)
		return 1
	}
	step("A.1", "authenticated as client_id=%s", c.ClientID)

	st := &state{}
	rc := 0

	defer func() {
		if r := recover(); r != nil {
			log.Printf("⚠ panic: %v — running cleanup anyway", r)
			rc = 1
		}
		if os.Getenv("JOURNEY_KEEP") == "1" {
			log.Printf("- cleanup skipped (JOURNEY_KEEP=1)")
			st.printSummary()
			return
		}
		st.cleanup(ctx, c)
		st.audit(ctx, c)
	}()

	if err := runPhaseA(ctx, c, st); err != nil {
		log.Printf("Fatal: %v", err)
		rc = 1
	}
	return rc
}

func runPhaseA(ctx context.Context, c *api.Client, st *state) error {
	servers, err := cloudServer.New(c).List(ctx)
	if err != nil {
		return fmt.Errorf("cloud.server.List: %w", err)
	}
	for _, s := range servers.CloudServers {
		if s.State == "On" {
			st.ccs = s
			break
		}
	}
	if st.ccs.Name == "" {
		return fmt.Errorf("no Cloud Container Server in state=On")
	}
	step("A.2", "target CCS: name=%s primary_ip=%s state=%s",
		st.ccs.Name, st.ccs.PrimaryIP, st.ccs.State)

	if _, err := bandwidth.New(c).GetUsageSummary(ctx); err != nil {
		log.Printf("⚠ bandwidth.GetUsageSummary (baseline): %v", err)
	} else {
		step("A.3", "bandwidth baseline read (delta updates infrequently)")
	}

	// Ephemeral keypair. Generated in-memory only; the public key
	// gets registered via ssh.key.Create so cloud.ssh.user.Add can
	// reference it by ID. The private key never leaves this process.
	pub, priv, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return fmt.Errorf("ed25519.GenerateKey: %w", err)
	}
	st.sshPriv = priv
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return fmt.Errorf("ssh.NewPublicKey: %w", err)
	}
	pubLine := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(sshPub)))

	keyResp, err := sshKey.New(c).Create(ctx, sshKey.CreateRequest{
		Label:             "gosh-build-a-site-" + tsString(),
		Content:           pubLine,
		CustomImageAccess: false,
	})
	if err != nil {
		return fmt.Errorf("ssh.key.Create: %w", err)
	}
	st.keyID = keyResp.Return.KeyID
	step("A.4", "registered ephemeral ed25519 ssh key id=%s", st.keyID)

	// docker_compose — see comment on buildWWWCompose for the schema
	// rationale. cloud.stack.Add's Label MUST be the FQDN; the API
	// rejects non-FQDN labels with "Error: Unable to add stack, the
	// hostname is invalid."
	gen, err := cloudStack.New(c).GenerateName(ctx)
	if err != nil {
		return fmt.Errorf("cloud.stack.GenerateName: %w", err)
	}
	st.stackName = gen.Return.Name
	st.hostname = fmt.Sprintf("gosh.%s.sth.nz", st.ccs.PrimaryIP)
	compose := buildWWWCompose(st.stackName, st.hostname)

	addStack, err := cloudStack.New(c).Add(ctx, cloudStack.AddRequest{
		ServerName:    st.ccs.Name,
		Name:          st.stackName,
		Label:         st.hostname,
		EnableSSL:     0,
		DockerCompose: compose,
	})
	if err != nil {
		return fmt.Errorf("cloud.stack.Add: %w", err)
	}
	// CONVENTION: every write that returns a job must be followed by
	// a job.Get poll before the next dependent call. The Add response
	// confirms the API ACCEPTED the request; the underlying provision
	// can still fail (e.g. "Image not typed") and only surfaces as
	// state=Failed on the scheduler job. See top-of-file comment.
	if err := waitForJob(ctx, c, addStack.Return.ID, 5*time.Minute); err != nil {
		return fmt.Errorf("cloud.stack.Add job: %w", err)
	}
	step("A.5", "stack provisioned: name=%s hostname=%s", st.stackName, st.hostname)

	// cloud.db.Add does NOT provision a new DB container — it creates a
	// database WITHIN an existing one. We use the well-known shared
	// mariadb1108 stack. Container ties this DB to our web stack.
	st.dbName = st.stackName
	addDB, err := cloudDB.New(c).Add(ctx, cloudDB.AddRequest{
		ServerName: st.ccs.Name,
		MySQLHost:  dbHost,
		Database:   st.dbName,
		Container:  st.stackName,
	})
	if err != nil {
		return fmt.Errorf("cloud.db.Add: %w", err)
	}
	if err := waitForJob(ctx, c, addDB.Return.ID, 2*time.Minute); err != nil {
		return fmt.Errorf("cloud.db.Add job: %w", err)
	}
	step("A.6", "database created: %s on %s", st.dbName, dbHost)

	st.dbUser = st.stackName
	st.dbPassword = randHex(16)
	addDBUser, err := cloudDBUser.New(c).Add(ctx, cloudDBUser.AddRequest{
		ServerName: st.ccs.Name,
		MySQLHost:  dbHost,
		Username:   st.dbUser,
		Password:   st.dbPassword,
		Database:   st.dbName,
		// Canonical "app user" grant set, mirroring what SiteHost's
		// own customer-facing tooling provisions (lowercased; includes
		// compound grants like "create temporary tables"). Read off
		// an existing healthy user in the API.
		Grants: []string{
			"select", "insert", "update", "delete",
			"create", "drop", "index", "alter",
			"create temporary tables", "lock tables",
			"create view", "show view",
		},
	})
	if err != nil {
		return fmt.Errorf("cloud.db.user.Add: %w", err)
	}
	if err := waitForJob(ctx, c, addDBUser.Return.ID, 2*time.Minute); err != nil {
		return fmt.Errorf("cloud.db.user.Add job: %w", err)
	}
	step("A.7", "database user created: %s (password held in-memory)", st.dbUser)

	// SSHKeys is a list of key IDs (from ssh.key.Create), not literal
	// public-key content. Containers scopes the SSH user; SSH login
	// lands them inside our www container's volume mounts.
	st.sshUser = "g" + randHex(7) // SSH username length-limited
	if _, err := cloudSSHUser.New(c).Add(ctx, cloudSSHUser.AddRequest{
		ServerName:     st.ccs.Name,
		Username:       st.sshUser,
		Containers:     []string{st.stackName},
		SSHKeys:        []string{st.keyID},
		ReadOnlyConfig: false,
	}); err != nil {
		return fmt.Errorf("cloud.ssh.user.Add: %w", err)
	}
	step("A.8", "ssh user created: %s scoped to container=%s", st.sshUser, st.stackName)

	// Web containers serve from /container/application; the SSH user
	// lands there via the volume mount in the compose body.
	// SiteHost's www-type containers serve from
	// /container/application/public/ — Laravel-style DocumentRoot.
	// The image ships an index.html placeholder ("Your Website Is
	// Almost Ready"); we remove it so Apache's DirectoryIndex picks
	// our index.php, then upload our PHP that queries the DB.
	st.marker = randHex(markerLen)
	indexPHP := buildIndexPHP(st)
	if err := sshExecRun(st.ccs.PrimaryIP, st.sshUser, priv,
		"rm -f /container/application/public/index.html"); err != nil {
		return fmt.Errorf("ssh exec rm placeholder: %w", err)
	}
	if err := sftpUpload(st.ccs.PrimaryIP, st.sshUser, priv,
		"/container/application/public/index.php", indexPHP); err != nil {
		return fmt.Errorf("sftp upload: %w", err)
	}
	step("A.9", "deployed index.php (marker=%s) to /container/application/public/", st.marker)

	// The response body must contain the marker (proves our deployed
	// code ran) AND "db_ok" (proves the PDO connection from inside
	// the web container worked). That's the full chain.
	body, err := waitForHTTP(ctx, "http://"+st.hostname+"/", 90*time.Second)
	if err != nil {
		return fmt.Errorf("http verify: %w", err)
	}
	if !strings.Contains(body, st.marker) {
		return fmt.Errorf("response missing marker %q; first 300 chars: %s",
			st.marker, truncate(body, 300))
	}
	if !strings.Contains(body, "db_ok") {
		return fmt.Errorf("response missing db_ok; first 300 chars: %s",
			truncate(body, 300))
	}
	step("A.10", "served HTTP 200 with marker %s and db_ok", st.marker)

	if _, err := bandwidth.New(c).GetUsageSummary(ctx); err != nil {
		log.Printf("⚠ bandwidth.GetUsageSummary (end): %v", err)
	} else {
		step("A.11", "bandwidth end-state read (deltas update slowly; not asserting)")
	}

	fmt.Println()
	fmt.Println("  → externally verifiable while it's still up:")
	fmt.Printf("      curl -i http://%s/\n", st.hostname)
	fmt.Println()
	return nil
}

// cleanup tears down everything in reverse order. Each step is best-effort.
func (st *state) cleanup(ctx context.Context, c *api.Client) {
	log.Printf("─── Cleanup ───")

	if st.sshUser != "" && st.ccs.Name != "" {
		if _, err := cloudSSHUser.New(c).Delete(ctx, cloudSSHUser.DeleteRequest{
			ServerName: st.ccs.Name, Username: st.sshUser,
		}); err != nil {
			log.Printf("⚠ cloud.ssh.user.Delete %s: %v", st.sshUser, err)
		} else {
			step("C.1", "cloud.ssh.user deleted: %s", st.sshUser)
		}
	}

	if st.dbUser != "" && st.ccs.Name != "" {
		resp, err := cloudDBUser.New(c).Delete(ctx, cloudDBUser.DeleteRequest{
			ServerName: st.ccs.Name, MySQLHost: dbHost, Username: st.dbUser,
		})
		if err != nil {
			log.Printf("⚠ cloud.db.user.Delete %s: %v", st.dbUser, err)
		} else {
			_ = waitForJob(ctx, c, resp.Return.ID, 2*time.Minute)
			step("C.2", "cloud.db.user deleted: %s", st.dbUser)
		}
	}

	if st.dbName != "" && st.ccs.Name != "" {
		resp, err := cloudDB.New(c).Delete(ctx, cloudDB.DeleteRequest{
			ServerName: st.ccs.Name, MySQLHost: dbHost, Database: st.dbName,
		})
		if err != nil {
			log.Printf("⚠ cloud.db.Delete %s: %v", st.dbName, err)
		} else {
			_ = waitForJob(ctx, c, resp.Return.ID, 2*time.Minute)
			step("C.3", "cloud.db deleted: %s", st.dbName)
		}
	}

	if st.stackName != "" && st.ccs.Name != "" {
		// Stacks can have a per-stack "job already running" lock that
		// lingers briefly after the previous job's state=Completed.
		// gosh's models.Stack doesn't expose the `pending` field that
		// would let us poll for quiescence directly, so retry briefly
		// on the specific error instead.
		var resp cloudStack.JobResponse
		var err error
		deadline := time.Now().Add(60 * time.Second)
		for {
			resp, err = cloudStack.New(c).Delete(ctx, cloudStack.DeleteRequest{
				ServerName: st.ccs.Name, Name: st.stackName,
			})
			if err == nil || !strings.Contains(err.Error(), "job already running on this stack") {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			log.Printf("⚠ cloud.stack.Delete %s: %v", st.stackName, err)
		} else {
			_ = waitForJob(ctx, c, resp.Return.ID, 2*time.Minute)
			step("C.4", "cloud.stack deleted: %s", st.stackName)
		}
	}

	if st.keyID != "" {
		if _, err := sshKey.New(c).Delete(ctx, sshKey.DeleteRequest{ID: st.keyID}); err != nil {
			log.Printf("⚠ ssh.key.Delete %s: %v", st.keyID, err)
		} else {
			step("C.5", "ssh.key deleted: id=%s", st.keyID)
		}
	}
}

// audit lists each namespace we touched and asserts our resource is gone.
func (st *state) audit(ctx context.Context, c *api.Client) {
	log.Printf("─── Audit ───")

	if stacks, err := cloudStack.New(c).List(ctx, cloudStack.ListRequest{ServerName: st.ccs.Name}); err == nil {
		if anyMatch(stacks.Return.Stacks, func(s models.Stack) bool { return s.Name == st.stackName }) {
			log.Printf("⚠ stack %s still present after cleanup", st.stackName)
		} else {
			step("D.1", "cloud.stack.List: our stack absent ✓")
		}
	} else {
		log.Printf("⚠ cloud.stack.List for audit: %v", err)
	}

	if dbs, err := cloudDB.New(c).List(ctx, cloudDB.ListOptions{ServerName: st.ccs.Name, MySQLHost: dbHost}); err == nil {
		if anyMatch(dbs.Return.Databases, func(d models.Database) bool { return d.DBName == st.dbName }) {
			log.Printf("⚠ database %s still present after cleanup", st.dbName)
		} else {
			step("D.2", "cloud.db.List: our database absent ✓")
		}
	} else {
		log.Printf("⚠ cloud.db.List for audit: %v", err)
	}

	if users, err := cloudDBUser.New(c).List(ctx, cloudDBUser.ListOptions{ServerName: st.ccs.Name, MySQLHost: dbHost}); err == nil {
		if anyMatch(users.Return.Users, func(u models.DatabaseUser) bool { return u.Username == st.dbUser }) {
			log.Printf("⚠ db user %s still present after cleanup", st.dbUser)
		} else {
			step("D.3", "cloud.db.user.List: our user absent ✓")
		}
	} else {
		log.Printf("⚠ cloud.db.user.List for audit: %v", err)
	}

	if users, err := cloudSSHUser.New(c).List(ctx, cloudSSHUser.ListOptions{ServerName: st.ccs.Name}); err == nil {
		if anyMatch(users.Return.Users, func(u models.User) bool { return u.Username == st.sshUser }) {
			log.Printf("⚠ ssh user %s still present after cleanup", st.sshUser)
		} else {
			step("D.4", "cloud.ssh.user.List: our user absent ✓")
		}
	} else {
		log.Printf("⚠ cloud.ssh.user.List for audit: %v", err)
	}

	if keys, err := sshKey.New(c).List(ctx); err == nil {
		if anyMatch(keys.Return.SSHKeys, func(k models.SSHKey) bool { return k.ID == st.keyID }) {
			log.Printf("⚠ ssh key id=%s still present after cleanup", st.keyID)
		} else {
			step("D.5", "ssh.key.List: our key absent ✓")
		}
	} else {
		log.Printf("⚠ ssh.key.List for audit: %v", err)
	}
}

func (st *state) printSummary() {
	log.Printf("─── Resources left in place (JOURNEY_KEEP=1) ───")
	log.Printf("  hostname:  http://%s/", st.hostname)
	log.Printf("  stack:     %s on %s", st.stackName, st.ccs.Name)
	log.Printf("  database:  %s on %s", st.dbName, dbHost)
	log.Printf("  db user:   %s", st.dbUser)
	log.Printf("  ssh user:  %s", st.sshUser)
	log.Printf("  ssh key:   id=%s", st.keyID)
}

func buildClient(ctx context.Context, apiKey, clientID string) (*api.Client, error) {
	if clientID != "" {
		return api.New(apiKey, clientID)
	}
	return info.NewClientWithDiscovery(ctx, apiKey)
}

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

func buildIndexPHP(st *state) string {
	return fmt.Sprintf(`<?php
$marker = %q;
try {
    $pdo = new PDO("mysql:host=%s;dbname=%s", %q, %q,
        [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
    $pdo->query("SELECT 1");
    echo "marker=$marker db_ok";
} catch (Exception $e) {
    http_response_code(500);
    echo "marker=$marker db_fail: " . $e->getMessage();
}
`, st.marker, dbHost, st.dbName, st.dbUser, st.dbPassword)
}

// sshExecRun runs cmd on the remote host and ignores stdout/stderr,
// returning only the error (or nil on success).
func sshExecRun(host, user string, priv ed25519.PrivateKey, cmd string) error {
	_, err := sshExec(host, user, priv, cmd)
	return err
}

// sshExec runs cmd on the remote host as the given user via SSH and
// returns the combined stdout. Diagnostic helper.
func sshExec(host, user string, priv ed25519.PrivateKey, cmd string) (string, error) {
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		return "", err
	}
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(host, "22")
	var sshClient *gossh.Client
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		sshClient, err = gossh.Dial("tcp", addr, cfg)
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return "", err
	}
	defer sshClient.Close()
	sess, err := sshClient.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(cmd)
	return string(out), err
}

// sftpUpload connects to host:22 as user (ed25519 private-key auth)
// and writes content to remotePath. Retries the dial briefly because
// the SSH server may not accept the new user immediately after
// cloud.ssh.user.Add.
func sftpUpload(host, user string, priv ed25519.PrivateKey, remotePath, content string) error {
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		return fmt.Errorf("ssh.NewSignerFromKey: %w", err)
	}
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(), // first-contact convenience for the example
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(host, "22")
	var sshClient *gossh.Client
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		sshClient, err = gossh.Dial("tcp", addr, cfg)
		if err == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("ssh.Dial %s as %s: %w", addr, user, err)
	}
	defer sshClient.Close()

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("sftp.NewClient: %w", err)
	}
	defer sftpClient.Close()

	f, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp.Create %s: %w", remotePath, err)
	}
	defer f.Close()
	if _, err := f.Write([]byte(content)); err != nil {
		return fmt.Errorf("sftp.Write: %w", err)
	}
	return nil
}

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
			return fmt.Errorf("job %d failed (state=%s)", jobID, resp.Return.State)
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("job %d did not reach a terminal state within %s", jobID, timeout)
}

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

func anyMatch[T any](xs []T, pred func(T) bool) bool {
	for _, x := range xs {
		if pred(x) {
			return true
		}
	}
	return false
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("%s must be set", name)
	}
	return v
}

func tsString() string { return fmt.Sprintf("%d", time.Now().Unix()) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)[:n]
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
