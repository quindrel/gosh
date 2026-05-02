// Program mail demonstrates the full mail lifecycle:
//
//   1. Create a temporary DNS zone (mail domains require a
//      managed zone)
//   2. Add the zone as a mail domain (mail.AddDomain)
//   3. Create an account, alias, and forwarder
//   4. List each (verifying counts and shapes)
//   5. Update the account's label
//   6. Search for the alias
//   7. Cleanup: delete alias + forwarder + account, then mail
//      domain, then DNS zone
//
// Required environment:
//
//	SH_API_KEY      — your SiteHost API key
//	SH_CLIENT_ID    — your client ID
//	SH_MAIL_SERVER  — name of your mapped mail service (e.g.
//	                  "sth-mail-air" for SiteHost's shared mail
//	                  service). Must be discovered out-of-band —
//	                  the public API doesn't currently expose
//	                  enumeration.
//
// Creates and deletes real mail / DNS resources. No billing
// implications for short-lived test data.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/dns"
	"github.com/sitehostnz/gosh/pkg/api/mail"
)

func main() {
	apiKey := mustEnv("SH_API_KEY")
	clientID := mustEnv("SH_CLIENT_ID")
	mailServer := mustEnv("SH_MAIL_SERVER")

	c, err := api.New(apiKey, clientID)
	if err != nil {
		log.Fatalf("api.New: %v", err)
	}
	d := dns.New(c)
	m := mail.NewForServer(c, mailServer) // captures server_name as default
	ctx := context.Background()

	var rb [4]byte
	_, _ = rand.Read(rb[:])
	zone := fmt.Sprintf("gosh-example-%s-%d.co.nz", hex.EncodeToString(rb[:]), time.Now().Unix())
	acct := "alice@" + zone
	aliasSource := "info@" + zone
	forwardSource := "external@" + zone
	forwardDest := "remote@elsewhere.example"

	log.Printf("Using test zone: %s", zone)

	// 1) DNS zone (precondition for mail domain)
	if _, err := d.CreateZone(ctx, dns.CreateZoneRequest{DomainName: zone}); err != nil {
		log.Fatalf("CreateZone: %v", err)
	}
	log.Printf("✓ DNS zone created")

	// 2) Mail domain
	if _, err := m.AddDomain(ctx, mail.AddDomainOptions{Domain: zone}); err != nil {
		log.Fatalf("mail.AddDomain: %v", err)
	}
	log.Printf("✓ Mail domain added")

	// 3) Account, alias, forwarder
	if _, err := m.AddAccount(ctx, mail.AddAccountOptions{
		Email:         acct,
		AccountParams: mail.AccountParams{Password: "Sup3rS3cret!Pass", Label: "Alice"},
	}); err != nil {
		log.Fatalf("mail.AddAccount: %v", err)
	}
	log.Printf("✓ Account %s created", acct)

	if _, err := m.AddAlias(ctx, mail.AddAliasOptions{Source: aliasSource, Destination: acct}); err != nil {
		log.Fatalf("mail.AddAlias: %v", err)
	}
	log.Printf("✓ Alias %s → %s", aliasSource, acct)

	if _, err := m.AddForward(ctx, mail.AddForwardOptions{Source: forwardSource, Destination: forwardDest}); err != nil {
		log.Fatalf("mail.AddForward: %v", err)
	}
	log.Printf("✓ Forwarder %s → %s", forwardSource, forwardDest)

	// 4) Verify via list endpoints
	if accounts, err := m.ListAccounts(ctx, mail.ListAccountsOptions{Domain: zone}); err != nil {
		log.Printf("⚠ ListAccounts: %v", err)
	} else {
		log.Printf("✓ %d account(s) on the domain", len(accounts.Return))
	}
	if aliases, err := m.ListAliases(ctx, mail.ListAliasesOptions{Domain: zone}); err != nil {
		log.Printf("⚠ ListAliases: %v", err)
	} else {
		log.Printf("✓ %d alias(es)", len(aliases.Return))
	}
	if forwards, err := m.ListForwards(ctx, mail.ListForwardsOptions{Domain: zone}); err != nil {
		log.Printf("⚠ ListForwards: %v", err)
	} else {
		log.Printf("✓ %d forwarder(s)", len(forwards.Return))
	}

	// 5) Update account label
	if _, err := m.UpdateAccount(ctx, mail.UpdateAccountOptions{
		Email:         acct,
		AccountParams: mail.AccountParams{Label: "Alice (renamed)"},
	}); err != nil {
		log.Printf("⚠ UpdateAccount: %v", err)
	} else {
		log.Printf("✓ Account label updated")
	}

	// 6) Search for the alias by exact source
	if results, err := m.SearchAliases(ctx, mail.SearchAliasesOptions{Source: aliasSource}); err != nil {
		log.Printf("⚠ SearchAliases: %v", err)
	} else {
		log.Printf("✓ SearchAliases returned %d result(s)", len(results.Return))
	}

	// 7) Cleanup in reverse order
	if _, err := m.DeleteAlias(ctx, mail.DeleteAliasOptions{Source: aliasSource, Destination: acct}); err != nil {
		log.Printf("⚠ DeleteAlias: %v", err)
	}
	if _, err := m.DeleteForward(ctx, mail.DeleteForwardOptions{Source: forwardSource, Destination: forwardDest}); err != nil {
		log.Printf("⚠ DeleteForward: %v", err)
	}
	if _, err := m.DeleteAccount(ctx, mail.DeleteAccountOptions{Email: acct}); err != nil {
		log.Printf("⚠ DeleteAccount: %v", err)
	}
	if _, err := m.DeleteDomain(ctx, mail.DeleteDomainOptions{Domain: zone}); err != nil {
		log.Printf("⚠ DeleteDomain: %v (may need manual cleanup)", err)
	}
	if _, err := d.DeleteZone(ctx, dns.DeleteZoneRequest{DomainName: zone}); err != nil {
		log.Printf("⚠ DeleteZone: %v (may need manual cleanup of %s)", err, zone)
	}
	log.Printf("✓ Cleanup complete")
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("required env var %s is not set", name)
	}
	return v
}
