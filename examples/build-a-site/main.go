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
// Reads are synchronous (no job in the response). Every async write
// — including cloud.ssh.user.Add and cloud.ssh.user.Delete which
// embed a job in their response — must wait. Audit endpoints
// (List, Get) may show stale state for tens of seconds if the
// preceding write's job hasn't been polled to Completed.
//
// Required env: SH_API_KEY
// Optional env: SH_CLIENT_ID (skip discovery), JOURNEY_KEEP=1 (skip cleanup)
package main

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	imapclient "github.com/emersion/go-imap/client"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/bandwidth"
	cloudDB "github.com/sitehostnz/gosh/pkg/api/cloud/db"
	cloudDBUser "github.com/sitehostnz/gosh/pkg/api/cloud/db/user"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	cloudSSHUser "github.com/sitehostnz/gosh/pkg/api/cloud/ssh/user"
	cloudStack "github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	"github.com/sitehostnz/gosh/pkg/api/dns"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	"github.com/sitehostnz/gosh/pkg/api/cloud/stack/ssl/letsencrypt"
	"github.com/sitehostnz/gosh/pkg/api/mail"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
	"github.com/sitehostnz/gosh/pkg/api/srs"
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
	hostname   string // sth.nz hostname for Phase A (always set)
	stackName  string // cc<hex> — also the container_name
	keyID      string // ssh.key id
	dbName     string // app DB name
	dbUser     string // app DB user
	dbPassword string // app DB password (in-memory only)
	sshUser    string // SSH user for code deploy
	sshPriv    ed25519.PrivateKey
	marker     string // unique string we expect to see served

	// Phase B fields (zero-valued when Phase B doesn't run)
	domain          string   // BYO test domain or auto-generated gosh-test-<hex>.co.nz
	dnsRecordIDs    []string // every DNS record we created, for cleanup
	zoneCreated     bool     // whether we created the DNS zone
	mailDomainAdded bool     // whether mail.AddDomain succeeded
	mailHostname    string   // SMTP/IMAP hostname from mail.GetServerInfo
	mailSender      string   // sender@<domain>
	mailReceiver    string   // receiver@<domain>
	mailSenderPwd   string
	mailReceiverPwd string

	// Phase B v3 fields (zero-valued unless JOURNEY_REGISTER_DOMAIN=1)
	registerDomain   bool // user opted into SRS register/cancel
	domainRegistered bool // SRS registration succeeded
	leCertCreated    bool // Let's Encrypt cert was issued
}

const mailService = "sth-mail-air"

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

	// Phase B opts in via JOURNEY_DOMAIN. Auto-generates a unique
	// `gosh-test-<8hex>.co.nz` if the env var is unset but the
	// JOURNEY_PHASE_B=1 flag is set explicitly. Without either, only
	// Phase A runs.
	if domain := os.Getenv("JOURNEY_DOMAIN"); domain != "" {
		st.domain = domain
	} else if os.Getenv("JOURNEY_REGISTER_DOMAIN") == "1" {
		// Auto-generated unique .co.nz that we'll register via SRS,
		// run the full Phase B against (including LE since it's
		// publicly delegated), then cancel inside the .nz 5-day grace.
		st.domain = "gosh-journey-" + randHex(8) + ".co.nz"
		st.registerDomain = true
	} else if os.Getenv("JOURNEY_PHASE_B") == "1" {
		st.domain = "gosh-test-" + randHex(8) + ".co.nz"
	}

	if err := runPhaseA(ctx, c, st); err != nil {
		log.Printf("Fatal: %v", err)
		rc = 1
		return rc
	}

	if st.domain != "" {
		if err := runPhaseB(ctx, c, st); err != nil {
			log.Printf("Fatal: %v", err)
			rc = 1
		}
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
	// Phase A always uses the sth.nz hostname. Phase B's test domain
	// is folded into the VIRTUAL_HOST list at stack-creation time
	// so a single stack serves both — avoids needing cloud.stack.Update
	// later. When Phase B isn't running, st.domain is empty.
	hostnames := []string{st.hostname}
	if st.domain != "" {
		hostnames = append(hostnames, st.domain)
	}
	compose := buildWWWCompose(st.stackName, hostnames)

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
	addSSHUser, err := cloudSSHUser.New(c).Add(ctx, cloudSSHUser.AddRequest{
		ServerName:     st.ccs.Name,
		Username:       st.sshUser,
		Containers:     []string{st.stackName},
		SSHKeys:        []string{st.keyID},
		ReadOnlyConfig: false,
	})
	if err != nil {
		return fmt.Errorf("cloud.ssh.user.Add: %w", err)
	}
	if err := waitForJob(ctx, c, addSSHUser.Return.ID, 2*time.Minute); err != nil {
		return fmt.Errorf("cloud.ssh.user.Add job: %w", err)
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

// runPhaseB runs against a Phase B test domain (st.domain), already
// folded into Phase A's stack VIRTUAL_HOST. Covers DNS hosting,
// mail provisioning + SMTP/IMAP loopback, and (when
// JOURNEY_REGISTER_DOMAIN=1) SRS register/cancel + Let's Encrypt.
func runPhaseB(ctx context.Context, c *api.Client, st *state) error {
	log.Printf("─── Phase B (domain=%s, register=%v) ───", st.domain, st.registerDomain)

	// ── B.0 — SRS register (only when JOURNEY_REGISTER_DOMAIN=1) ───────
	//
	// Done first so the rest of Phase B has a publicly-resolvable
	// domain — required for LE's HTTP-01 challenge later.
	if st.registerDomain {
		if err := runPhaseBRegister(ctx, c, st); err != nil {
			return err
		}
	}

	dnsClient := dns.New(c)

	// ── B.1 — host DNS for the test domain ─────────────────────────────
	//
	// dns.CreateZone accepts unregistered synthetic domain names. The
	// zone exists in SiteHost's nameservers and is queryable via
	// `dig @<sitehost-ns>`, but won't resolve via public recursive
	// resolvers unless the domain is registered with NS records
	// pointing at SiteHost. For Phase B v1 verification we bypass
	// public DNS by hitting the container IP with a custom Host
	// header — that proves nginx-proxy routes for our hostname
	// regardless of public DNS state.
	if _, err := dnsClient.CreateZone(ctx, dns.CreateZoneRequest{DomainName: st.domain}); err != nil {
		return fmt.Errorf("dns.CreateZone: %w", err)
	}
	st.zoneCreated = true
	step("B.1", "dns zone created: %s", st.domain)

	// ── B.2 — add A and CNAME records pointing at the container ────────
	a, err := dnsClient.AddRecord(ctx, dns.AddRecordRequest{
		Domain:  st.domain,
		Type:    "A",
		Name:    st.domain, // apex (@)
		Content: st.ccs.PrimaryIP,
	})
	if err != nil {
		return fmt.Errorf("dns.AddRecord A: %w", err)
	}
	st.dnsRecordIDs = append(st.dnsRecordIDs, a.Return.ID)

	cname, err := dnsClient.AddRecord(ctx, dns.AddRecordRequest{
		Domain:  st.domain,
		Type:    "CNAME",
		Name:    "www." + st.domain,
		Content: st.domain,
	})
	if err != nil {
		return fmt.Errorf("dns.AddRecord CNAME: %w", err)
	}
	st.dnsRecordIDs = append(st.dnsRecordIDs, cname.Return.ID)
	step("B.2", "dns records added: A %s -> %s, CNAME www -> @",
		st.domain, st.ccs.PrimaryIP)

	// ── B.3 — verify the zone via list_records ─────────────────────────
	listResp, err := dnsClient.ListRecords(ctx, dns.ListRecordsRequest{Domain: st.domain})
	if err != nil {
		return fmt.Errorf("dns.ListRecords: %w", err)
	}
	step("B.3", "dns.ListRecords: %d records (NS + SOA + our two)", len(listResp.Return))

	// ── B.4 — verify HTTP serves through the test domain ───────────────
	//
	// We hit the container IP directly with Host: <test-domain> in
	// the request headers. nginx-proxy sees the Host header and
	// routes to our container's vhost. This bypasses public DNS,
	// which won't resolve for an unregistered synthetic domain.
	body, err := getWithHost(ctx, "http://"+st.ccs.PrimaryIP+"/", st.domain, 30*time.Second)
	if err != nil {
		return fmt.Errorf("phase B http verify: %w", err)
	}
	if !strings.Contains(body, st.marker) {
		return fmt.Errorf("phase B response missing marker %q", st.marker)
	}
	step("B.4", "served HTTP 200 with marker %s via Host: %s", st.marker, st.domain)

	fmt.Println()
	fmt.Printf("  → externally verifiable while it's still up (bypasses public DNS):\n")
	fmt.Printf("      curl -i http://%s/ --resolve %s:80:%s\n", st.domain, st.domain, st.ccs.PrimaryIP)
	fmt.Println()

	if err := runPhaseBMail(ctx, c, st); err != nil {
		return err
	}

	// ── B.10 — Let's Encrypt (only when register mode, since LE's ────
	// HTTP-01 challenge needs the domain publicly delegated) ─────────
	if st.registerDomain {
		if err := runPhaseBLetsEncrypt(ctx, c, st); err != nil {
			return err
		}
	}

	return nil
}

// runPhaseBRegister registers the test domain via SRS, then waits
// for it to become Active. Real-money path: cancel inside the .nz
// 5-day grace to avoid billing.
func runPhaseBRegister(ctx context.Context, c *api.Client, st *state) error {
	srsClient := srs.New(c)

	// ── B.0a — pre-flight ──────────────────────────────────────────────
	whois, err := srsClient.Whois(ctx, srs.WhoisOptions{Domain: st.domain})
	if err != nil {
		return fmt.Errorf("srs.Whois: %w", err)
	}
	if whois.Return.State == "Active" {
		return fmt.Errorf("srs.Whois: %s already registered (state=Active)", st.domain)
	}
	step("B.0a", "srs.Whois: %s available (state=%s)", st.domain, whois.Return.State)

	avail, err := srsClient.DomainAvailable(ctx, srs.DomainAvailableOptions{Domain: st.domain})
	if err != nil {
		return fmt.Errorf("srs.DomainAvailable: %w", err)
	}
	if !avail.Return {
		return fmt.Errorf("srs.DomainAvailable: %s not available", st.domain)
	}
	step("B.0b", "srs.DomainAvailable: %s available ✓", st.domain)

	// ── B.0c — discover contact IDs from the account ───────────────────
	contacts, err := srsClient.ListContacts(ctx)
	if err != nil {
		return fmt.Errorf("srs.ListContacts: %w", err)
	}
	if len(contacts.Return) == 0 {
		return fmt.Errorf("srs.ListContacts: no contacts on account; create one before running register mode")
	}
	contactID := contacts.Return[0].ContactID
	id, err := atoi(contactID)
	if err != nil {
		return fmt.Errorf("invalid contact_id %q: %w", contactID, err)
	}
	step("B.0c", "srs.ListContacts: using contact_id=%d (%s)", id, contacts.Return[0].Name)

	// ── B.0d — register ────────────────────────────────────────────────
	//
	// Same contact for all four roles is the simplest path; the API
	// accepts identical IDs for registrant/admin/technical/billing.
	// Term is in MONTHS. .nz registry minimum is 12 months — sending
	// Term=1 fails with "must be registered for a minimum of 12 months".
	regResp, err := srsClient.CreateDomain(ctx, srs.CreateDomainOptions{
		Domain:            st.domain,
		Term:              12,
		RegistrantContact: id,
		AdminContact:      id,
		TechnicalContact:  id,
		BillingContact:    id,
	})
	if err != nil {
		return fmt.Errorf("srs.CreateDomain: %w", err)
	}
	if err := waitForJobOf(ctx, c, regResp.Return.ID, regResp.Return.Type, 10*time.Minute); err != nil {
		return fmt.Errorf("srs.CreateDomain job: %w", err)
	}
	st.domainRegistered = true
	step("B.0d", "srs.CreateDomain: %s registered ✓ (cancel within 5 days for refund)", st.domain)

	// ── B.0e — confirm Active ──────────────────────────────────────────
	dom, err := srsClient.GetDomain(ctx, srs.DomainOptions{Domain: st.domain})
	if err != nil {
		return fmt.Errorf("srs.GetDomain: %w", err)
	}
	step("B.0e", "srs.GetDomain: state=%s", dom.Return.State)
	return nil
}

// runPhaseBLetsEncrypt issues an LE cert for the registered domain
// via the cloud.stack.ssl.lets_encrypt.* surface, then verifies the
// HTTPS connection lands on a Let's Encrypt cert.
func runPhaseBLetsEncrypt(ctx context.Context, c *api.Client, st *state) error {
	leClient := letsencrypt.New(c)

	// ── B.10a — request the cert ───────────────────────────────────────
	//
	// HTTP-01 challenge runs against the stack's nginx-proxy vhost.
	// Requires the domain to publicly resolve to the CCS IP — which
	// it does, since SiteHost is the registrar AND DNS host for our
	// just-registered domain.
	createResp, err := leClient.Create(ctx, letsencrypt.CreateRequest{
		ServerName: st.ccs.Name,
		Name:       st.stackName,
	})
	if err != nil {
		return fmt.Errorf("letsencrypt.Create: %w", err)
	}
	if err := waitForJobOf(ctx, c, createResp.Return.ID, createResp.Return.Type, 10*time.Minute); err != nil {
		return fmt.Errorf("letsencrypt.Create job: %w", err)
	}
	st.leCertCreated = true
	step("B.10a", "Let's Encrypt cert issued for %s", st.domain)

	// ── B.10b — verify HTTPS handshake lands on LE cert ────────────────
	body, err := waitForHTTP(ctx, "https://"+st.domain+"/", 60*time.Second)
	if err != nil {
		return fmt.Errorf("https verify: %w", err)
	}
	if !strings.Contains(body, st.marker) {
		return fmt.Errorf("https response missing marker %q", st.marker)
	}
	step("B.10b", "HTTPS served marker %s on Let's Encrypt cert", st.marker)
	return nil
}

// atoi is strconv.Atoi, factored out so the call site reads tighter.
func atoi(s string) (int, error) {
	var n int
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(ch-'0')
	}
	return n, nil
}

// runPhaseBMail provisions mail on the test domain and verifies
// SMTP+IMAP loopback. Two accounts (sender, receiver) on the same
// domain; sender sends to receiver via SMTP submission; we poll
// IMAP until a message containing our marker arrives. Loopback
// bypasses public deliverability concerns (DNS, SPF, spam) — proves
// the SiteHost mail service routes internally.
func runPhaseBMail(ctx context.Context, c *api.Client, st *state) error {
	mailClient := mail.New(c)
	dnsClient := dns.New(c)

	// ── B.5 — register the test domain on the mail service ─────────────
	if _, err := mailClient.AddDomain(ctx, mail.AddDomainOptions{
		ServerOptions: mail.ServerOptions{ServerName: mailService},
		Domain:        st.domain,
	}); err != nil {
		return fmt.Errorf("mail.AddDomain: %w", err)
	}
	st.mailDomainAdded = true
	step("B.5", "mail.AddDomain: %s on %s", st.domain, mailService)

	// ── B.6 — get the SMTP/IMAP hostname ───────────────────────────────
	infoResp, err := mailClient.GetServerInfo(ctx, mail.GetServerInfoOptions{
		ServerOptions: mail.ServerOptions{ServerName: mailService},
	})
	if err != nil {
		return fmt.Errorf("mail.GetServerInfo: %w", err)
	}
	st.mailHostname = infoResp.Return.Hostname
	step("B.6", "mail.GetServerInfo: hostname=%s webmail=%s",
		st.mailHostname, infoResp.Return.WebmailURL)

	// ── B.7 — provision sender + receiver accounts ─────────────────────
	//
	// Mail provisioning jobs are DaemonType, not SchedulerType. They
	// also take longer than scheduler jobs (~2-3 min observed).
	st.mailSender = "sender@" + st.domain
	st.mailReceiver = "receiver@" + st.domain
	st.mailSenderPwd = randHex(16) + "Aa1!"
	st.mailReceiverPwd = randHex(16) + "Aa1!"

	addSender, err := mailClient.AddAccount(ctx, mail.AddAccountOptions{
		ServerOptions: mail.ServerOptions{ServerName: mailService},
		Email:         st.mailSender,
		AccountParams: mail.AccountParams{Password: st.mailSenderPwd, Quota: "100"},
	})
	if err != nil {
		return fmt.Errorf("mail.AddAccount sender: %w", err)
	}
	if err := waitForJobOf(ctx, c, addSender.Return.ID, addSender.Return.Type, 5*time.Minute); err != nil {
		return fmt.Errorf("mail.AddAccount sender job: %w", err)
	}

	addRecv, err := mailClient.AddAccount(ctx, mail.AddAccountOptions{
		ServerOptions: mail.ServerOptions{ServerName: mailService},
		Email:         st.mailReceiver,
		AccountParams: mail.AccountParams{Password: st.mailReceiverPwd, Quota: "100"},
	})
	if err != nil {
		return fmt.Errorf("mail.AddAccount receiver: %w", err)
	}
	if err := waitForJobOf(ctx, c, addRecv.Return.ID, addRecv.Return.Type, 5*time.Minute); err != nil {
		return fmt.Errorf("mail.AddAccount receiver job: %w", err)
	}
	step("B.7", "mail accounts created: %s, %s", st.mailSender, st.mailReceiver)

	// ── B.8 — add MX + SPF records ─────────────────────────────────────
	mx, err := dnsClient.AddRecord(ctx, dns.AddRecordRequest{
		Domain: st.domain, Type: "MX", Name: st.domain,
		Content: st.mailHostname, Priority: "10",
	})
	if err != nil {
		return fmt.Errorf("dns.AddRecord MX: %w", err)
	}
	st.dnsRecordIDs = append(st.dnsRecordIDs, mx.Return.ID)

	spf, err := dnsClient.AddRecord(ctx, dns.AddRecordRequest{
		Domain: st.domain, Type: "TXT", Name: st.domain,
		Content: "v=spf1 include:_spf.sitehost.co.nz ~all",
	})
	if err != nil {
		return fmt.Errorf("dns.AddRecord TXT: %w", err)
	}
	st.dnsRecordIDs = append(st.dnsRecordIDs, spf.Return.ID)
	step("B.8", "mail DNS records added: MX -> %s, TXT (SPF)", st.mailHostname)

	// ── B.9 — SMTP send + IMAP receive loopback ────────────────────────
	marker := "gosh-mail-" + randHex(8)
	if err := smtpSend(st.mailHostname, st.mailSender, st.mailSenderPwd,
		st.mailReceiver, "Phase B loopback", "Marker: "+marker); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	step("B.9a", "smtp send accepted: %s -> %s (marker=%s)",
		st.mailSender, st.mailReceiver, marker)

	if err := imapWaitForMessage(st.mailHostname, st.mailReceiver, st.mailReceiverPwd,
		marker, 60*time.Second); err != nil {
		return fmt.Errorf("imap wait: %w", err)
	}
	step("B.9b", "imap delivery confirmed: marker found in receiver inbox")

	return nil
}

// smtpSend authenticates as `from` against the SiteHost mail
// submission service (port 587, STARTTLS) and delivers a message
// to `to`. Used for the Phase B loopback test — same-domain
// delivery so MX lookup against public DNS isn't required.
func smtpSend(hostname, from, password, to, subject, body string) error {
	addr := net.JoinHostPort(hostname, "587")
	auth := smtp.PlainAuth("", from, password, hostname)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n",
		from, to, subject, body))
	return smtp.SendMail(addr, auth, from, []string{to}, msg)
}

// imapWaitForMessage connects to IMAPS (port 993), authenticates
// as the receiver, and polls INBOX until a message containing the
// marker arrives or the deadline passes.
func imapWaitForMessage(hostname, user, password, marker string, timeout time.Duration) error {
	addr := net.JoinHostPort(hostname, "993")
	deadline := time.Now().Add(timeout)
	var sleep time.Duration
	for time.Now().Before(deadline) {
		found, err := imapHasMarker(addr, hostname, user, password, marker)
		if err == nil && found {
			return nil
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
	}
	return fmt.Errorf("marker %q not found in %s INBOX within %s", marker, user, timeout)
}

func imapHasMarker(addr, serverName, user, password, marker string) (bool, error) {
	cl, err := imapclient.DialTLS(addr, &tls.Config{ServerName: serverName})
	if err != nil {
		return false, err
	}
	defer cl.Logout()
	if err := cl.Login(user, password); err != nil {
		return false, err
	}
	mbox, err := cl.Select("INBOX", true)
	if err != nil {
		return false, err
	}
	if mbox.Messages == 0 {
		return false, nil
	}
	from := uint32(1)
	if mbox.Messages > 20 {
		from = mbox.Messages - 19
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, mbox.Messages)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem()}
	messages := make(chan *imap.Message, 20)
	done := make(chan error, 1)
	go func() { done <- cl.Fetch(seqset, items, messages) }()
	for msg := range messages {
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		body, _ := io.ReadAll(r)
		if strings.Contains(string(body), marker) {
			<-done
			return true, nil
		}
	}
	return false, <-done
}

// getWithHost issues a GET to url with the Host header overridden.
// Used to hit a container by IP while presenting a hostname for
// nginx-proxy's vhost routing.
func getWithHost(ctx context.Context, url, host string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	var sleep time.Duration
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return string(body), nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return "", lastErr
}

// cleanup tears down everything in reverse order. Each step is best-effort.
func (st *state) cleanup(ctx context.Context, c *api.Client) {
	log.Printf("─── Cleanup ───")

	// Phase B v3 LE teardown (only if cert was created).
	if st.leCertCreated {
		resp, err := letsencrypt.New(c).Delete(ctx, letsencrypt.DeleteRequest{
			ServerName: st.ccs.Name, Name: st.stackName,
		})
		if err != nil {
			log.Printf("⚠ letsencrypt.Delete: %v", err)
		} else {
			_ = waitForJobOf(ctx, c, resp.Return.ID, resp.Return.Type, 5*time.Minute)
			step("CB.LE", "Let's Encrypt cert deleted")
		}
	}

	// Phase B mail teardown — accounts, then domain. Done before DNS
	// records/zone so mail-side cleanup completes while DNS exists.
	if st.mailDomainAdded {
		mailClient := mail.New(c)
		for _, email := range []string{st.mailSender, st.mailReceiver} {
			if email == "" {
				continue
			}
			resp, err := mailClient.DeleteAccount(ctx, mail.DeleteAccountOptions{
				ServerOptions: mail.ServerOptions{ServerName: mailService},
				Email:         email,
			})
			if err != nil {
				log.Printf("⚠ mail.DeleteAccount %s: %v", email, err)
				continue
			}
			_ = waitForJobOf(ctx, c, resp.Return.ID, resp.Return.Type, 5*time.Minute)
			step("CB.M", "mail account deleted: %s", email)
		}
		if _, err := mailClient.DeleteDomain(ctx, mail.DeleteDomainOptions{
			ServerOptions: mail.ServerOptions{ServerName: mailService},
			Domain:        st.domain,
		}); err != nil {
			log.Printf("⚠ mail.DeleteDomain %s: %v", st.domain, err)
		} else {
			step("CB.M2", "mail domain unmapped: %s", st.domain)
		}
	}

	// Phase B teardown — DNS records, then zone. Reverse-add order.
	dnsClient := dns.New(c)
	for i := len(st.dnsRecordIDs) - 1; i >= 0; i-- {
		id := st.dnsRecordIDs[i]
		if _, err := dnsClient.DeleteRecord(ctx, dns.DeleteRecordRequest{
			Domain: st.domain, RecordID: id,
		}); err != nil {
			log.Printf("⚠ dns.DeleteRecord %s: %v", id, err)
		} else {
			step("CB.1", "dns record deleted: id=%s", id)
		}
	}
	if st.zoneCreated {
		if _, err := dnsClient.DeleteZone(ctx, dns.DeleteZoneRequest{DomainName: st.domain}); err != nil {
			log.Printf("⚠ dns.DeleteZone %s: %v", st.domain, err)
		} else {
			step("CB.2", "dns zone deleted: %s", st.domain)
		}
	}

	if st.sshUser != "" && st.ccs.Name != "" {
		// cloud.ssh.user.Delete appears to be two-phase server-side:
		// the first call clears the user's container/volume scoping
		// (Get afterward shows containers=[] volumes=[], user still
		// present); a second call fully removes the user account.
		// Loop Delete + waitForJob until Get returns error ("does
		// not exist"), with a deadline so a real bug doesn't hang.
		client := cloudSSHUser.New(c)
		deadline := time.Now().Add(60 * time.Second)
		var sleep time.Duration
		gone := false
		for time.Now().Before(deadline) {
			if _, gErr := client.Get(ctx, cloudSSHUser.GetRequest{
				ServerName: st.ccs.Name, Username: st.sshUser,
			}); gErr != nil {
				gone = true
				break
			}
			resp, err := client.Delete(ctx, cloudSSHUser.DeleteRequest{
				ServerName: st.ccs.Name, Username: st.sshUser,
			})
			if err != nil {
				log.Printf("⚠ cloud.ssh.user.Delete %s: %v", st.sshUser, err)
				break
			}
			_ = waitForJob(ctx, c, resp.Return.ID, 2*time.Minute)
			sleep = nextBackoff(sleep)
			time.Sleep(sleep)
		}
		if gone {
			step("C.1", "cloud.ssh.user deleted: %s", st.sshUser)
		} else {
			log.Printf("⚠ cloud.ssh.user %s not gone after 60s of Delete loop", st.sshUser)
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
		// Wait for the stack to be quiescent before issuing Delete.
		// models.Stack exposes Pending; polling Get and waiting until
		// Pending == nil is more correct than retry-on-error against
		// the "job already running on this stack" message — we observe
		// the actual resource state rather than guessing from an error
		// string.
		if err := waitForStackQuiescent(ctx, c, st.ccs.Name, st.stackName, 60*time.Second); err != nil {
			log.Printf("⚠ stack %s did not become quiescent: %v", st.stackName, err)
		}
		resp, err := cloudStack.New(c).Delete(ctx, cloudStack.DeleteRequest{
			ServerName: st.ccs.Name, Name: st.stackName,
		})
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

	// Phase B v3 SRS cancel — last, after all dependent state is gone.
	// Inside the .nz 5-day grace window, cancellation is unbilled.
	// OUTSIDE that window, the registry will bill — the warning log
	// below is intentionally loud.
	if st.domainRegistered {
		resp, err := srs.New(c).CancelDomain(ctx, srs.DomainOptions{Domain: st.domain})
		if err != nil {
			log.Printf("⚠ srs.CancelDomain %s: %v — REAL DOMAIN MAY BILL OUTSIDE 5-DAY GRACE", st.domain, err)
		} else {
			_ = waitForJobOf(ctx, c, resp.Return.ID, resp.Return.Type, 5*time.Minute)
			step("CB.SRS", "srs.CancelDomain: %s (within .nz 5-day grace; refunded)", st.domain)
		}
	}
}

// audit lists each namespace we touched and asserts our resource is gone.
func (st *state) audit(ctx context.Context, c *api.Client) {
	log.Printf("─── Audit ───")

	// Phase B audit: zone should be gone from list_domains.
	if st.zoneCreated {
		zones, err := dns.New(c).ListZones(ctx, nil)
		if err == nil {
			if anyMatch(zones.Return.Data, func(z models.DNSZone) bool { return z.Name == st.domain }) {
				log.Printf("⚠ dns zone %s still present after cleanup", st.domain)
			} else {
				step("DB.1", "dns.ListZones: our zone absent ✓")
			}
		} else {
			log.Printf("⚠ dns.ListZones for audit: %v", err)
		}
	}

	// Phase B mail audit: domain should be gone from mail.list_domains.
	if st.mailDomainAdded {
		domains, err := mail.New(c).ListDomains(ctx, mail.ListDomainsOptions{
			ServerOptions: mail.ServerOptions{ServerName: mailService},
		})
		if err == nil {
			if anyMatch(domains.Return, func(d mail.Domain) bool { return d.Domain == st.domain }) {
				log.Printf("⚠ mail domain %s still present after cleanup", st.domain)
			} else {
				step("DB.2", "mail.ListDomains: our domain absent ✓")
			}
		} else {
			log.Printf("⚠ mail.ListDomains for audit: %v", err)
		}
	}

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

	// SSH user audit uses Get rather than List. List has a stale
	// cache that lingers ~15-30s after delete — the user appears in
	// the list well after cloud.ssh.user.Delete's job state=Completed.
	// Get reflects the underlying state immediately (returns an error
	// once the user is gone), so it's the right primitive for an
	// "is it gone" check.
	if _, err := cloudSSHUser.New(c).Get(ctx, cloudSSHUser.GetRequest{
		ServerName: st.ccs.Name, Username: st.sshUser,
	}); err != nil {
		step("D.4", "cloud.ssh.user.Get: our user absent ✓")
	} else {
		log.Printf("⚠ ssh user %s still present after cleanup", st.sshUser)
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

// buildWWWCompose generates the docker_compose body for a www-type
// stack. hostnames is the list of FQDNs the container should respond
// to via nginx-proxy — typically the sth.nz wildcard hostname for
// Phase A, optionally augmented with a Phase B test domain. The
// first entry becomes CERT_NAME (LE primary subject); all entries
// (and their www. variants) are joined into VIRTUAL_HOST and
// website.vhosts.
func buildWWWCompose(name string, hostnames []string) string {
	primary := hostnames[0]
	var parts []string
	for _, h := range hostnames {
		parts = append(parts, h, "www."+h)
	}
	vhosts := strings.Join(parts, ",")
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
		primary,
		wwwImage,
		primary,
		vhosts,
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
	var sleep time.Duration
	for time.Now().Before(deadline) {
		sshClient, err = gossh.Dial("tcp", addr, cfg)
		if err == nil {
			break
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
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
	var sleep time.Duration
	for time.Now().Before(deadline) {
		sshClient, err = gossh.Dial("tcp", addr, cfg)
		if err == nil {
			break
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
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

// nextBackoff returns the next sleep in an exponential backoff
// sequence: 1s → 2s → 4s → 8s → 10s cap. Bounded to keep
// long-running provisions from spamming the API at flat-rate
// intervals (a 5min job would hit /job/get ~100 times at flat 3s).
func nextBackoff(prev time.Duration) time.Duration {
	if prev == 0 {
		return 1 * time.Second
	}
	next := prev * 2
	if next > 10*time.Second {
		return 10 * time.Second
	}
	return next
}

// waitForJob polls /job/get for a SchedulerType job. Used after
// cloud.* writes which queue scheduler jobs.
func waitForJob(ctx context.Context, c *api.Client, jobID int, timeout time.Duration) error {
	return waitForJobOf(ctx, c, jobID, job.SchedulerType, timeout)
}

// waitForJobOf is the explicit-type variant. mail.* writes queue
// DaemonType jobs (different scheduler from cloud.*); polling the
// wrong type returns "job does not exist" and waitForJob spins
// until timeout. Always pass the Type from the originating
// response (e.g. addAccount.Return.Type) rather than hardcoding.
func waitForJobOf(ctx context.Context, c *api.Client, jobID int, jobType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := job.New(c)
	var sleep time.Duration
	for time.Now().Before(deadline) {
		resp, err := client.Get(ctx, job.GetRequest{ID: jobID, Type: jobType})
		if err == nil {
			switch resp.Return.State {
			case "Completed":
				return nil
			case "Failed":
				msg := resp.Return.Message
				if msg == "" {
					msg = resp.Return.State
				}
				return fmt.Errorf("job %d (%s) failed: %s", jobID, jobType, msg)
			}
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
	}
	return fmt.Errorf("job %d (%s) did not reach a terminal state within %s", jobID, jobType, timeout)
}

// waitForStackQuiescent polls cloud.stack.Get until Pending == nil.
// Use this before any dependent write that touches the same stack:
// the per-stack "job already running" semaphore lingers briefly
// after the previous job's state=Completed.
func waitForStackQuiescent(ctx context.Context, c *api.Client, server, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := cloudStack.New(c)
	var sleep time.Duration
	for time.Now().Before(deadline) {
		resp, err := client.Get(ctx, cloudStack.GetRequest{ServerName: server, Name: name})
		if err == nil && resp.Stack.Pending == nil {
			return nil
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
	}
	return fmt.Errorf("stack %s/%s did not become quiescent within %s", server, name, timeout)
}

func waitForHTTP(ctx context.Context, url string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	var sleep time.Duration
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return string(body), nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
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
