// Program srs demonstrates the .nz domain lifecycle:
//
//   1. Generate a unique candidate domain (random suffix +
//      timestamp; collision-resistant by construction)
//   2. Run safety checks: srs.DomainAvailable +
//      srs.DomainInsideGracePeriod + look at our own
//      srs.ListDomains for any name collision
//   3. Register with srs.CreateDomain
//   4. Inspect via srs.GetDomain, srs.ListNameServers
//   5. Cancel with srs.CancelDomain (within the .nz 5-day
//      grace period — no billing if completed in time)
//
// Required environment:
//
//	SH_API_KEY    — your SiteHost API key
//	SH_CLIENT_ID  — your client ID
//
// Optional environment:
//
//	SH_TEST_REGISTRANT, SH_TEST_ADMIN, SH_TEST_TECH, SH_TEST_BILLING
//	  — contact IDs to use for the registration. If unset, the
//	    example fetches the first contact from srs.ListContacts
//	    and uses its ID for all four roles.
//
// Account must hold sufficient funds to cover the registration.
// The .nz registry refunds within the 5-day grace period if the
// cancellation succeeds before then.
//
// Why all the safety checks: the .nz "register and cancel within
// 5 days = no billing" rule applies once per domain per
// calendar month. Registering and cancelling the same domain
// twice would result in a charge on the second cycle. The random
// suffix makes accidental collisions vanishingly unlikely; the
// pre-flight checks catch the edge case where it does happen.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/srs"
)

func main() {
	apiKey := mustEnv("SH_API_KEY")
	clientID := mustEnv("SH_CLIENT_ID")

	c, err := api.New(apiKey, clientID)
	if err != nil {
		log.Fatalf("api.New: %v", err)
	}
	s := srs.New(c)
	ctx := context.Background()

	registrant, admin, tech, billing := resolveContacts(ctx, s)

	domain, err := generateUniqueDomain(ctx, s)
	if err != nil {
		log.Fatalf("generating candidate: %v", err)
	}
	log.Printf("✓ Cleared safety checks; using test domain: %s", domain)

	created, err := s.CreateDomain(ctx, srs.CreateDomainOptions{
		Domain:            domain,
		Term:              12,
		RegistrantContact: registrant,
		AdminContact:      admin,
		TechnicalContact:  tech,
		BillingContact:    billing,
	})
	if err != nil {
		log.Fatalf("CreateDomain: %v", err)
	}
	log.Printf("✓ Registered %s (job %d)", domain, created.Return.Job.ID)

	// Give the registry a moment to settle before inspection.
	time.Sleep(2 * time.Second)

	detail, err := s.GetDomain(ctx, srs.DomainOptions{Domain: domain})
	if err != nil {
		log.Printf("⚠ GetDomain (registration may still be pending): %v", err)
	} else {
		log.Printf("✓ Domain detail: state=%s, autorenew_term=%d, registrant_contact=%d",
			detail.Return.State, detail.Return.AutorenewTerm, detail.Return.RegistrantContactID)
	}

	ns, err := s.ListNameServers(ctx, srs.DomainOptions{Domain: domain})
	if err != nil {
		log.Printf("⚠ ListNameServers: %v", err)
	} else {
		log.Printf("✓ Nameservers: %d delegation entries", len(ns.Return))
	}

	cancelled, err := s.CancelDomain(ctx, srs.DomainOptions{Domain: domain})
	if err != nil {
		log.Fatalf("CancelDomain: %v (you may need to cancel %s manually before the .nz grace period expires)", err, domain)
	}
	log.Printf("✓ Cancelled %s (job %d) — no billing if completed within .nz 5-day grace period", domain, cancelled.Return.Job.ID)
}

// generateUniqueDomain produces a candidate .nz name that is
// collision-resistant by construction (random hex + timestamp),
// then verifies via three independent API calls that it's safe
// to register: not currently available-with-someone-else, not in
// any account's grace period, and not already on this client's
// own list.
func generateUniqueDomain(ctx context.Context, s *srs.Client) (string, error) {
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("random: %w", err)
		}
		domain := fmt.Sprintf("gosh-example-%s-%d.nz", hex.EncodeToString(b[:]), time.Now().Unix())

		// 1. Whois-level availability check.
		avail, err := s.DomainAvailable(ctx, srs.DomainAvailableOptions{Domain: domain})
		if err != nil {
			return "", fmt.Errorf("DomainAvailable: %w", err)
		}
		if !avail.Return {
			log.Printf("attempt %d: %s not available, retrying", attempt, domain)
			continue
		}

		// 2. Grace-period check — if the name was recently
		// cancelled, the registry may still consider it ours
		// for a few days. Skip if so.
		grace, err := s.DomainInsideGracePeriod(ctx, srs.DomainOptions{Domain: domain})
		if err == nil && grace.Return {
			log.Printf("attempt %d: %s is in another grace period, retrying", attempt, domain)
			continue
		}

		// 3. Local check — make sure this client's own list
		// doesn't already include the name (in any state).
		mine, err := s.ListDomains(ctx, &srs.ListDomainsOptions{PageSize: 200})
		if err != nil {
			return "", fmt.Errorf("ListDomains: %w", err)
		}
		clash := false
		for _, d := range mine.Return.Data {
			if d.Domain == domain {
				log.Printf("attempt %d: %s already on this account (state=%s), retrying", attempt, domain, d.State)
				clash = true
				break
			}
		}
		if clash {
			continue
		}

		return domain, nil
	}
	return "", fmt.Errorf("could not find a safe candidate after %d attempts", maxAttempts)
}

// resolveContacts uses explicit env-var contact IDs if all four
// are set; otherwise falls back to the first contact from the
// account's contact list.
func resolveContacts(ctx context.Context, s *srs.Client) (reg, admin, tech, bill int) {
	if rs := os.Getenv("SH_TEST_REGISTRANT"); rs != "" {
		reg, _ = strconv.Atoi(rs)
		admin, _ = strconv.Atoi(os.Getenv("SH_TEST_ADMIN"))
		tech, _ = strconv.Atoi(os.Getenv("SH_TEST_TECH"))
		bill, _ = strconv.Atoi(os.Getenv("SH_TEST_BILLING"))
		if reg != 0 && admin != 0 && tech != 0 && bill != 0 {
			return
		}
	}
	contacts, err := s.ListContacts(ctx)
	if err != nil {
		log.Fatalf("ListContacts: %v", err)
	}
	if len(contacts.Return) == 0 {
		log.Fatalf("no contacts on the account — set SH_TEST_REGISTRANT etc. or create a contact first")
	}
	id, _ := strconv.Atoi(contacts.Return[0].ContactID)
	log.Printf("Using contact %d for all four roles (set SH_TEST_* env vars to override)", id)
	return id, id, id, id
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("required env var %s is not set", name)
	}
	return v
}
