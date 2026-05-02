// Program full-overview is a read-only smoke test that exercises
// every public-facing namespace gosh wraps. One line of output per
// namespace; counts only.
//
// The other example programs prove individual namespaces work
// end-to-end (lifecycle, write paths). This program proves
// "gosh can talk to a real account" in under thirty seconds with
// zero setup beyond credentials.
//
// Required environment:
//
//	SH_API_KEY    — your SiteHost API key
//
// Optional environment:
//
//	SH_CLIENT_ID    — your client ID. If unset, the program calls
//	                  info.NewClientWithDiscovery to resolve it from
//	                  api/get_info.json. Set explicitly when targeting
//	                  a sub-account from a super-user key.
//	SH_MAIL_SERVER  — mail service identifier; enables the mail-domains read.
//
// 100% read-only — safe to run on any account.
package main

import (
	"context"
	"log"
	"os"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/bandwidth"
	cloudDB "github.com/sitehostnz/gosh/pkg/api/cloud/db"
	cloudImage "github.com/sitehostnz/gosh/pkg/api/cloud/image"
	cloudServer "github.com/sitehostnz/gosh/pkg/api/cloud/server"
	cloudStack "github.com/sitehostnz/gosh/pkg/api/cloud/stack"
	cloudVolume "github.com/sitehostnz/gosh/pkg/api/cloud/volume"
	"github.com/sitehostnz/gosh/pkg/api/dns"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/mail"
	"github.com/sitehostnz/gosh/pkg/api/server"
	"github.com/sitehostnz/gosh/pkg/api/server/snapshot"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
	"github.com/sitehostnz/gosh/pkg/api/srs"
	"github.com/sitehostnz/gosh/pkg/api/ssl"
)

func main() {
	apiKey := os.Getenv("SH_API_KEY")
	if apiKey == "" {
		log.Fatalf("SH_API_KEY must be set")
	}
	ctx := context.Background()

	var c *api.Client
	var err error
	if clientID := os.Getenv("SH_CLIENT_ID"); clientID != "" {
		c, err = api.New(apiKey, clientID)
		if err != nil {
			log.Fatalf("api.New: %v", err)
		}
	} else {
		c, err = info.NewClientWithDiscovery(ctx, apiKey)
		if err != nil {
			log.Fatalf("info.NewClientWithDiscovery: %v", err)
		}
		log.Printf("✓ discovered client_id=%s via api/get_info.json", c.ClientID)
	}

	// Track first-server name for the snapshot read, which requires it.
	var firstServer string

	// 1. server reads
	if servers, err := server.New(c).List(ctx); err != nil {
		log.Printf("⚠ server.List: %v", err)
	} else {
		n := len(servers.Return.Servers)
		log.Printf("✓ server.List: %d server(s)", n)
		if n > 0 {
			firstServer = servers.Return.Servers[0].Name
		}
	}

	// 2. dns zones + ips
	d := dns.New(c)
	if zones, err := d.ListZones(ctx, nil); err != nil {
		log.Printf("⚠ dns.ListZones: %v", err)
	} else {
		log.Printf("✓ dns.ListZones: %d zone(s)", len(zones.Return.Data))
	}
	if ips, err := d.ListIPs(ctx); err != nil {
		log.Printf("⚠ dns.ListIPs: %v", err)
	} else {
		log.Printf("✓ dns.ListIPs: %d IP(s)", len(ips.Return))
	}

	// 3. mail (gated on SH_MAIL_SERVER)
	if mailServer := os.Getenv("SH_MAIL_SERVER"); mailServer != "" {
		m := mail.New(c)
		opt := mail.ListDomainsOptions{ServerOptions: mail.ServerOptions{ServerName: mailServer}}
		if domains, err := m.ListDomains(ctx, opt); err != nil {
			log.Printf("⚠ mail.ListDomains[%s]: %v", mailServer, err)
		} else {
			log.Printf("✓ mail.ListDomains[%s]: %d domain(s)", mailServer, len(domains.Return))
		}
	} else {
		log.Printf("- mail.ListDomains: skipped (SH_MAIL_SERVER unset)")
	}

	// 4. srs domains + contacts
	r := srs.New(c)
	if domains, err := r.ListDomains(ctx, nil); err != nil {
		log.Printf("⚠ srs.ListDomains: %v", err)
	} else {
		log.Printf("✓ srs.ListDomains: %d domain(s)", len(domains.Return.Data))
	}
	if contacts, err := r.ListContacts(ctx); err != nil {
		log.Printf("⚠ srs.ListContacts: %v", err)
	} else {
		log.Printf("✓ srs.ListContacts: %d contact(s)", len(contacts.Return))
	}

	// 5. ssl
	if certs, err := ssl.New(c).ListCertificates(ctx); err != nil {
		log.Printf("⚠ ssl.ListCertificates: %v", err)
	} else {
		log.Printf("✓ ssl.ListCertificates: %d cert(s)", len(certs.Return))
	}

	// 6. cloud namespaces
	if stacks, err := cloudStack.New(c).List(ctx, cloudStack.ListRequest{}); err != nil {
		log.Printf("⚠ cloud.stack.List: %v", err)
	} else {
		log.Printf("✓ cloud.stack.List: %d stack(s)", len(stacks.Return.Stacks))
	}
	if volumes, err := cloudVolume.New(c).List(ctx, nil); err != nil {
		log.Printf("⚠ cloud.volume.List: %v", err)
	} else {
		log.Printf("✓ cloud.volume.List: %d volume(s)", len(volumes.Return.Data))
	}
	if images, err := cloudImage.New(c).List(ctx); err != nil {
		log.Printf("⚠ cloud.image.List: %v", err)
	} else {
		log.Printf("✓ cloud.image.List: %d image(s)", len(images.Return.Images))
	}
	if dbs, err := cloudDB.New(c).List(ctx, cloudDB.ListOptions{}); err != nil {
		log.Printf("⚠ cloud.db.List: %v", err)
	} else {
		log.Printf("✓ cloud.db.List: %d database(s)", len(dbs.Return.Databases))
	}
	if cs, err := cloudServer.New(c).List(ctx); err != nil {
		log.Printf("⚠ cloud.server.List: %v", err)
	} else {
		log.Printf("✓ cloud.server.List: %d cloud server(s)", len(cs.CloudServers))
	}

	// 7. ssh keys
	if keys, err := sshKey.New(c).List(ctx); err != nil {
		log.Printf("⚠ ssh.key.List: %v", err)
	} else {
		log.Printf("✓ ssh.key.List: %d key(s)", len(keys.Return.SSHKeys))
	}

	// 8. bandwidth resources
	if res, err := bandwidth.New(c).ListResources(ctx); err != nil {
		log.Printf("⚠ bandwidth.ListResources: %v", err)
	} else {
		log.Printf("✓ bandwidth.ListResources: %d quota group(s)", len(res.Return))
	}

	// 9. snapshot for first server (gated on a server existing)
	if firstServer != "" {
		opt := snapshot.ListOptions{Name: firstServer}
		if snaps, err := snapshot.New(c).List(ctx, opt); err != nil {
			log.Printf("⚠ snapshot.List[%s]: %v", firstServer, err)
		} else {
			log.Printf("✓ snapshot.List[%s]: %d snapshot(s)", firstServer, len(snaps.Return))
		}
	} else {
		log.Printf("- snapshot.List: skipped (no servers on account)")
	}
}
