// Program dns demonstrates the DNS zone + records lifecycle:
//
//   1. Generate a candidate zone name (.nz, whois-checked)
//   2. Create the zone with dns.CreateZone
//   3. Add an A record and a TXT record
//   4. List records to verify
//   5. Update the A record's content
//   6. Delete the records, then the zone
//
// Required environment:
//
//	SH_API_KEY    — your SiteHost API key
//	SH_CLIENT_ID  — your client ID
//
// Creates and deletes a real DNS zone. Storage is free; no
// billing implications. Cleanup happens before exit on the happy
// path; if the program crashes, manual cleanup may be needed.
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
	"github.com/sitehostnz/gosh/pkg/api/srs"
	"github.com/sitehostnz/gosh/pkg/models"
)

func main() {
	apiKey := os.Getenv("SH_API_KEY")
	clientID := os.Getenv("SH_CLIENT_ID")
	if apiKey == "" || clientID == "" {
		log.Fatalf("SH_API_KEY and SH_CLIENT_ID must be set")
	}

	c, err := api.New(apiKey, clientID)
	if err != nil {
		log.Fatalf("api.New: %v", err)
	}
	d := dns.New(c)
	r := srs.New(c)
	ctx := context.Background()

	var rb [4]byte
	_, _ = rand.Read(rb[:])
	zone := fmt.Sprintf("gosh-example-%s-%d.co.nz", hex.EncodeToString(rb[:]), time.Now().Unix())
	log.Printf("Using test zone: %s", zone)

	// Whois confirm the candidate name isn't already a registered
	// domain (DNS hosting still works for unregistered names, but
	// an unintentional collision would be embarrassing).
	whois, err := r.Whois(ctx, srs.WhoisOptions{Domain: zone})
	if err == nil && whois.Return.State == "Active" {
		log.Fatalf("%s is already a registered domain — pick a different test zone name", zone)
	}

	created, err := d.CreateZone(ctx, dns.CreateZoneRequest{DomainName: zone})
	if err != nil {
		log.Fatalf("CreateZone: %v", err)
	}
	log.Printf("✓ Zone created (is_migration=%v)", created.Return.IsMigration)

	addedA, err := d.AddRecord(ctx, dns.AddRecordRequest{
		Domain:  zone,
		Type:    "A",
		Name:    "www." + zone,
		Content: "192.0.2.10",
	})
	if err != nil {
		log.Fatalf("AddRecord (A): %v", err)
	}
	log.Printf("✓ A record added (id=%s)", addedA.Return.ID)

	addedTXT, err := d.AddRecord(ctx, dns.AddRecordRequest{
		Domain:  zone,
		Type:    "TXT",
		Name:    "_test." + zone,
		Content: "gosh-example-marker",
	})
	if err != nil {
		log.Fatalf("AddRecord (TXT): %v", err)
	}
	log.Printf("✓ TXT record added (id=%s)", addedTXT.Return.ID)

	records, err := d.ListRecords(ctx, dns.ListRecordsRequest{Domain: zone})
	if err != nil {
		log.Fatalf("ListRecords: %v", err)
	}
	log.Printf("✓ ListRecords returned %d records (NS/SOA + the two we added)", len(records.Return))
	for _, rec := range records.Return {
		if rec.Type == "A" || rec.Type == "TXT" {
			log.Printf("  user record: %s %s %s -> %s", rec.ID, rec.Type, rec.Name, rec.Content)
		}
	}

	// Update the A record's content.
	if _, err := d.UpdateRecord(ctx, dns.UpdateRecordRequest{
		Domain:   zone,
		RecordID: addedA.Return.ID,
		Type:     "A",
		Name:     "www." + zone,
		Content:  "192.0.2.20",
	}); err != nil {
		log.Fatalf("UpdateRecord: %v", err)
	}
	log.Printf("✓ A record updated to 192.0.2.20")

	// Cleanup: delete user records, then the zone.
	for _, rec := range []models.DNSRecord{{ID: addedA.Return.ID}, {ID: addedTXT.Return.ID}} {
		if _, err := d.DeleteRecord(ctx, dns.DeleteRecordRequest{Domain: zone, RecordID: rec.ID}); err != nil {
			log.Printf("⚠ DeleteRecord %s: %v (continuing cleanup)", rec.ID, err)
		}
	}
	log.Printf("✓ User records deleted")

	if _, err := d.DeleteZone(ctx, dns.DeleteZoneRequest{DomainName: zone}); err != nil {
		log.Fatalf("DeleteZone: %v (manual cleanup of %s may be required)", err, zone)
	}
	log.Printf("✓ Zone %s deleted", zone)
}
