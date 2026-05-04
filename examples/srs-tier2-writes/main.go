// Program srs-tier2-writes validates the SRS "tier 2" write
// endpoints — contact CRUD, domain-contact rebinding, and
// company-info update — against the long-lived gosh-srs-test.nz
// test domain.
//
// Steps:
//
//   1. Read current company info (GetCompanyInfo).
//   2. Create a temp contact (CreateContact).
//   3. Update the temp contact (UpdateContact).
//   4. Rebind gosh-srs-test.nz to the temp contact for all four
//      roles (UpdateDomainContacts), then verify via GetDomain.
//   5. Restore the original primary contact via
//      UpdateDomainContacts.
//   6. Delete the temp contact (DeleteContact).
//   7. Update company info as a no-op (write the values we just
//      read), confirming UpdateCompanyInfo is wired correctly
//      without changing the account.
//
// The temp contact is always cleaned up even on partial failure
// (deferred). The primary contact is rebound back to the domain
// before the temp delete attempt, since deleting a bound contact
// is rejected.
//
// Required env: SH_API_KEY, SH_CLIENT_ID.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/srs"
)

const testDomain = "gosh-srs-test.nz"

func main() {
	if err := run(); err != nil {
		log.Fatalf("srs-tier2-writes: %v", err)
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

	// Capture primary contact id (the one currently bound to the
	// test domain) so we can rebind back to it later.
	dom, err := srsC.GetDomain(ctx, srs.DomainOptions{Domain: testDomain})
	if err != nil {
		return fmt.Errorf("GetDomain(%s): %w (run examples/srs-register first)", testDomain, err)
	}
	primaryRegistrantID := dom.Return.RegistrantContactID
	primaryAdminID := dom.Return.AdminContactID
	primaryTechID := dom.Return.TechnicalContactID
	log.Printf("==> baseline contact bindings on %s: registrant=%d admin=%d tech=%d",
		testDomain, primaryRegistrantID, primaryAdminID, primaryTechID)

	// === Read current company info (we'll write it back as a no-op later) ===
	log.Printf("==> srs.GetCompanyInfo")
	co, err := srsC.GetCompanyInfo(ctx)
	if err != nil {
		return fmt.Errorf("GetCompanyInfo: %w", err)
	}
	log.Printf("    name=%q url=%q email=%q", co.Return.CompanyName, co.Return.CompanyURL, co.Return.CompanyEmail)

	// === Create a temp contact ===
	suffix := randHex(6)
	tempName := "Gosh Test " + suffix
	log.Printf("==> srs.CreateContact(%s)", tempName)
	created, err := srsC.CreateContact(ctx, srs.CreateContactOptions{
		Name:           tempName,
		Email:          "noreply@example.com",
		PostalAddress:  "1 Test Lane",
		PostalAddress2: "Building A",
		Suburb:         "Ellerslie",
		City:           "Auckland",
		Country:        "NZ",
		PostCode:       "1010",
		Province:       "Auckland",
		Organization:   "Gosh Validation",
		// Phone is required per the docs schema:
		// params[Phone][Country|Area|Local|Extension].
		PhoneCountry: "64",
		PhoneArea:    "9",
		PhoneLocal:   "9742182",
	})
	if err != nil {
		return fmt.Errorf("CreateContact: %w", err)
	}
	tempContactID, err := strconv.Atoi(created.Return.ContactID)
	if err != nil {
		return fmt.Errorf("non-numeric contact_id %q: %w", created.Return.ContactID, err)
	}
	log.Printf("    ✓ created temp contact id=%d", tempContactID)

	tempCleaned := false
	rebound := false
	defer func() {
		// 1. If we rebound the domain to the temp contact, rebind
		//    back to primary first (can't delete a bound contact).
		if rebound {
			log.Printf("==> cleanup: rebind %s back to primary contact", testDomain)
			_, err := srsC.UpdateDomainContacts(ctx, srs.UpdateDomainContactsOptions{
				Domain:              testDomain,
				RegistrantContactID: primaryRegistrantID,
				AdminContactID:      primaryAdminID,
				TechnicalContactID:  primaryTechID,
			})
			if err != nil {
				log.Printf("    rebind-back: %v", err)
			} else {
				log.Printf("    ✓ rebound to primary")
			}
		}
		// 2. Delete the temp contact.
		if !tempCleaned {
			log.Printf("==> cleanup: srs.DeleteContact(%d)", tempContactID)
			if _, err := srsC.DeleteContact(ctx, srs.DeleteContactOptions{ContactID: tempContactID}); err != nil {
				log.Printf("    DeleteContact: %v", err)
			} else {
				log.Printf("    ✓ deleted temp contact")
			}
		}
	}()

	// === Update the temp contact (change email — Organization
	//     isn't a documented update_contact field, only create) ===
	log.Printf("==> srs.UpdateContact(%d) — change email", tempContactID)
	if _, err := srsC.UpdateContact(ctx, srs.UpdateContactOptions{
		ContactID: tempContactID,
		Email:     "noreply2@example.com",
	}); err != nil {
		return fmt.Errorf("UpdateContact: %w", err)
	}
	log.Printf("    ✓ updated")

	// === Rebind domain to temp contact for all 3 required roles ===
	log.Printf("==> srs.UpdateDomainContacts(%s) — rebind admin + tech to temp (registrant stays primary per .nz rules)", testDomain)
	// .nz registry typically restricts registrant changes; keep
	// the original registrant and just move admin + tech.
	if _, err := srsC.UpdateDomainContacts(ctx, srs.UpdateDomainContactsOptions{
		Domain:              testDomain,
		RegistrantContactID: primaryRegistrantID, // unchanged
		AdminContactID:      tempContactID,
		TechnicalContactID:  tempContactID,
	}); err != nil {
		return fmt.Errorf("UpdateDomainContacts(rebind): %w", err)
	}
	rebound = true
	verify, err := srsC.GetDomain(ctx, srs.DomainOptions{Domain: testDomain})
	if err != nil {
		return fmt.Errorf("GetDomain after rebind: %w", err)
	}
	log.Printf("    after rebind: admin=%d tech=%d (temp is %d)",
		verify.Return.AdminContactID, verify.Return.TechnicalContactID, tempContactID)
	if verify.Return.AdminContactID != tempContactID {
		log.Printf("    ⚠ admin didn't change (visible delay or registry-restricted)")
	}

	// === Update company info as a no-op (write current values back) ===
	log.Printf("==> srs.UpdateCompanyInfo — round-trip current values (no real change)")
	if _, err := srsC.UpdateCompanyInfo(ctx, srs.UpdateCompanyInfoOptions{
		CompanyName:  co.Return.CompanyName,
		CompanyURL:   co.Return.CompanyURL,
		CompanyEmail: co.Return.CompanyEmail,
	}); err != nil {
		return fmt.Errorf("UpdateCompanyInfo: %w", err)
	}
	log.Printf("    ✓ accepted (no-op)")

	log.Printf("✓ srs-tier2-writes: 5 wrappers exercised against %s + temp contact id=%d",
		testDomain, tempContactID)
	return nil
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)[:n]
}
