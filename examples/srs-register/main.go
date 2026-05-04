// Program srs-register one-shot-registers gosh-srs-test.nz with
// SiteHost's registry and leaves it in place.
//
// **Idempotent.** Reads srs.ListDomains first; if the test domain
// is already registered, prints its details and exits cleanly. Only
// registers when missing.
//
// **Does NOT cancel.** Unlike examples/srs (which exercises the full
// register → inspect → cancel-within-grace cycle), this example
// keeps the domain alive so subsequent SRS-write examples (lock,
// unlock, privacy, contact updates, etc.) have a real registered
// domain to target. Cancel testing happens in a separate, late
// example after the user signals everything else is validated.
//
// Per .nz registry rules and SiteHost policy, registrations
// shouldn't happen more than once per calendar month per
// domain — this example's idempotent check guards against that.
//
// Required env: SH_API_KEY, SH_CLIENT_ID.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/job"
	"github.com/sitehostnz/gosh/pkg/api/srs"
)

const (
	testDomain = "gosh-srs-test.nz"
	termMonths = 12 // .nz registry minimum
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("srs-register: %v", err)
	}
}

func run() error {
	apiKey := os.Getenv("SH_API_KEY")
	clientID := os.Getenv("SH_CLIENT_ID")
	if apiKey == "" || clientID == "" {
		return fmt.Errorf("SH_API_KEY and SH_CLIENT_ID required")
	}
	c, err := api.New(apiKey, clientID)
	if err != nil {
		return fmt.Errorf("api.New: %w", err)
	}
	srsC := srs.New(c)
	ctx := context.Background()

	// 1. Idempotency check: is the domain already registered to us?
	log.Printf("==> srs.ListDomains — checking if %s is already ours", testDomain)
	listed, err := srsC.ListDomains(ctx, nil)
	if err != nil {
		return fmt.Errorf("ListDomains: %w", err)
	}
	for _, d := range listed.Return.Data {
		if d.Domain == testDomain {
			log.Printf("✓ %s already registered (state=%s registrant=%s reg_id=%s)",
				d.Domain, d.State, d.RegistrantName, d.RegID)
			log.Printf("  no action taken")
			return nil
		}
	}
	log.Printf("    not present in our domains list — proceeding to register")

	// 2. Sanity: is it actually available at the registry?
	avail, err := srsC.DomainAvailable(ctx, srs.DomainAvailableOptions{Domain: testDomain})
	if err != nil {
		return fmt.Errorf("DomainAvailable: %w", err)
	}
	if !avail.Return {
		return fmt.Errorf("%s reports as not available — someone else may have it, or it was registered+cancelled within a grace window", testDomain)
	}
	log.Printf("    ✓ available at the registry")

	// 3. Pick a contact. The account already has 4 contacts; first
	//    one is fine for all four roles.
	contacts, err := srsC.ListContacts(ctx)
	if err != nil {
		return fmt.Errorf("ListContacts: %w", err)
	}
	if len(contacts.Return) == 0 {
		return fmt.Errorf("no contacts on the account — create one via srs.CreateContact first")
	}
	contactID, err := strconv.Atoi(contacts.Return[0].ContactID)
	if err != nil {
		return fmt.Errorf("non-numeric contact_id %q: %w", contacts.Return[0].ContactID, err)
	}
	log.Printf("    using contact_id=%d (%s) for all 4 roles",
		contactID, contacts.Return[0].Name)

	// 4. Pricing sanity check (so we don't surprise ourselves).
	price, err := srsC.GetDomainPrice(ctx, srs.DomainOptions{Domain: testDomain})
	if err != nil {
		log.Printf("    GetDomainPrice: %v (continuing anyway)", err)
	} else {
		log.Printf("    price: NZD %.2f for %d months (premium=%v)",
			price.Return.TotalPrice, termMonths, price.Return.Premium)
	}

	// 5. Register.
	log.Printf("==> srs.CreateDomain(%s, term=%d months)", testDomain, termMonths)
	created, err := srsC.CreateDomain(ctx, srs.CreateDomainOptions{
		Domain:            testDomain,
		Term:              termMonths,
		RegistrantContact: contactID,
		AdminContact:      contactID,
		TechnicalContact:  contactID,
		BillingContact:    contactID,
	})
	if err != nil {
		return fmt.Errorf("CreateDomain: %w", err)
	}
	log.Printf("    job id=%d type=%s", created.Return.ID, created.Return.Type)

	// 6. Wait for completion (mail-server-style daemon job).
	if err := waitForJob(ctx, c, created.Return.ID, created.Return.Type, 5*time.Minute); err != nil {
		return fmt.Errorf("CreateDomain job: %w", err)
	}
	log.Printf("✓ %s registered", testDomain)

	// 7. Confirm via Get.
	got, err := srsC.GetDomain(ctx, srs.DomainOptions{Domain: testDomain})
	if err != nil {
		return fmt.Errorf("GetDomain: %w", err)
	}
	log.Printf("  state=%s registered=%s billed_until=%s",
		got.Return.State, got.Return.DateRegistered, got.Return.DateBilledUntil)
	return nil
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
		case <-time.After(5 * time.Second):
		}
	}
}
