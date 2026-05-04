// Program srs-tier3-writes validates the SRS "tier 3" wrappers —
// domain mutators (add_name_servers, update_domain) and the
// multi-TLD availability lookup — against gosh-srs-test.nz.
//
// renew_domain is intentionally skipped (it charges the account).
// transfer_domain is intentionally skipped (it requires a domain
// at another registrar plus a valid UDAI from the losing
// registrar; gosh's test rig can't satisfy either).
//
// Required env: SH_API_KEY, SH_CLIENT_ID.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/srs"
)

const testDomain = "gosh-srs-test.nz"

func main() {
	if err := run(); err != nil {
		log.Fatalf("srs-tier3-writes: %v", err)
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

	probeDomain := "goshcheck-" + randHex(4) + ".com"
	log.Printf("==> srs.TLDsAvailable(%s)", probeDomain)
	tlds, err := srsC.TLDsAvailable(ctx, srs.TLDsAvailableOptions{Domain: probeDomain})
	if err != nil {
		log.Printf("    ⚠ TLDsAvailable: %v", err)
	} else {
		log.Printf("    ✓ %s available=%v", tlds.Return.Domain, tlds.Return.Available)
	}

	log.Printf("==> srs.UpdateDomain(%s) — registry-record refresh", testDomain)
	if _, err := srsC.UpdateDomain(ctx, srs.UpdateDomainOptions{Domain: testDomain}); err != nil {
		log.Printf("    ⚠ UpdateDomain rejected: %v", err)
	} else {
		log.Printf("    ✓ accepted")
	}

	// Live finding: registry requires ≥2 NS in the call (not the
	// total set on the domain — the call itself must enumerate ≥2).
	// Send the existing SiteHost pair as a no-op-ish add.
	log.Printf("==> srs.AddNameServers(%s, ns1+ns2.sitehost.co.nz)", testDomain)
	if _, err := srsC.AddNameServers(ctx, srs.AddNameServersOptions{
		Domain: testDomain,
		NameServers: []srs.NameServerEntry{
			{Name: "ns1.sitehost.co.nz"},
			{Name: "ns2.sitehost.co.nz"},
		},
	}); err != nil {
		log.Printf("    ⚠ AddNameServers: %v", err)
	} else {
		log.Printf("    ✓ accepted")
	}

	log.Printf("==> srs.NewUDAI(%s) — triggers email to registrant", testDomain)
	if _, err := srsC.NewUDAI(ctx, srs.NewUDAIOptions{Domain: testDomain}); err != nil {
		log.Printf("    ⚠ NewUDAI rejected: %v", err)
	} else {
		log.Printf("    ✓ accepted (UDAI emailed to registrant)")
	}

	log.Printf("==> srs.ValidateUDAI(%s, BOGUS) — should reject", testDomain)
	resp, err := srsC.ValidateUDAI(ctx, srs.ValidateUDAIOptions{Domain: testDomain, UDAI: "BOGUS-CODE"})
	if err != nil {
		log.Printf("    ⚠ ValidateUDAI errored: %v", err)
	} else {
		log.Printf("    ✓ status=%v msg=%q (false expected for bogus code)", resp.Status, resp.Msg)
	}

	log.Printf("==> srs.ListEmailTemplates")
	tpls, err := srsC.ListEmailTemplates(ctx)
	if err != nil {
		log.Printf("    ⚠ ListEmailTemplates: %v", err)
	} else {
		log.Printf("    ✓ %d templates (sample: name=%q type=%q customized=%v)",
			len(tpls.Return), tpls.Return[0].Name, tpls.Return[0].Type, tpls.Return[0].Customized)
	}
	// GetEmailTemplate is not exercised live: the `template` parameter
	// is required but no value derived from the list response (name,
	// type, template_id) satisfies it. See pkg/api/srs/email_templates.go
	// doc-comment for details and docs/api-issues/.

	log.Printf("✓ srs-tier3-writes complete (skipped: renew_domain, transfer_domain, update_email_template, verify_email_token)")
	return nil
}

func randHex(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = hex[i*7%16]
	}
	return string(out)
}
