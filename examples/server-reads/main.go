// Program server-reads demonstrates read-only server diagnostics
// — useful as a smoke test that gosh can talk to the API
// without doing anything destructive.
//
//   1. List all servers for the client
//   2. Pick the first server
//   3. Fetch its state and upgrade quotas
//   4. List available images and locations
//
// Required environment:
//
//	SH_API_KEY    — your SiteHost API key
//	SH_CLIENT_ID  — your client ID
//
// 100% read-only — safe to run on any account.
package main

import (
	"context"
	"log"
	"os"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/server"
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
	s := server.New(c)
	ctx := context.Background()

	servers, err := s.List(ctx)
	if err != nil {
		log.Fatalf("List: %v", err)
	}
	log.Printf("✓ %d server(s) on the account", len(servers.Return.Servers))
	if len(servers.Return.Servers) == 0 {
		log.Printf("(no servers — examples below are skipped)")
		return
	}

	first := servers.Return.Servers[0]
	log.Printf("First server: name=%s, label=%s, type=%s, state=%s",
		first.Name, first.Label, first.ProductType, first.State)

	state, err := s.GetState(ctx, server.GetStateOptions{Name: first.Name})
	if err != nil {
		log.Printf("⚠ GetState: %v", err)
	} else {
		log.Printf("✓ State=%s, rescue=%v, last_job=%s/%s",
			state.Return.State, state.Return.Rescue,
			state.Return.LastJob.ID, state.Return.LastJob.State)
	}

	upgrades, err := s.ListUpgrades(ctx, server.ListUpgradesOptions{Name: first.Name})
	if err != nil {
		log.Printf("⚠ ListUpgrades: %v", err)
	} else {
		q := upgrades.Return.Quota
		log.Printf("✓ Quota: ram=%d/%d, disk=%d/%d, cores=%d/%d",
			q.RAM.Used, q.RAM.Total, q.Disk.Used, q.Disk.Total, q.Cores.Used, q.Cores.Total)
	}

	images, err := s.ListImages(ctx)
	if err != nil {
		log.Printf("⚠ ListImages: %v", err)
	} else {
		log.Printf("✓ %d server image(s) available", len(images.Return))
	}

	locations, err := s.ListLocations(ctx)
	if err != nil {
		log.Printf("⚠ ListLocations: %v", err)
	} else {
		log.Printf("✓ %d location(s) available for provisioning", len(locations.Return))
	}
}
