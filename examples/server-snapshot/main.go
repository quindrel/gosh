// Program server-snapshot demonstrates the snapshot lifecycle:
//
//   1. Take a snapshot of a server's disk (1-hour lifetime)
//   2. List snapshots to verify
//   3. Adjust the lifetime to 2 hours
//   4. Delete the snapshot
//
// Required environment:
//
//	SH_API_KEY     — your SiteHost API key
//	SH_CLIENT_ID   — your client ID
//	SH_TEST_SERVER — name of a server you don't mind briefly
//	                 snapshotting. Use a low-stakes / test server,
//	                 not anything in production. The example takes
//	                 a snapshot, lists, adjusts lifetime, and
//	                 deletes within a few seconds.
//
// Optional environment:
//
//	SH_TEST_PARTITION — disk partition slot to snapshot. Defaults
//	                    to "scsi0".
//
// Creates and deletes a real snapshot. Storage costs are
// negligible for the brief lifetime.
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/server/snapshot"
)

func main() {
	apiKey := mustEnv("SH_API_KEY")
	clientID := mustEnv("SH_CLIENT_ID")
	server := mustEnv("SH_TEST_SERVER")

	partition := os.Getenv("SH_TEST_PARTITION")
	if partition == "" {
		partition = "scsi0"
	}

	c, err := api.New(apiKey, clientID)
	if err != nil {
		log.Fatalf("api.New: %v", err)
	}
	s := snapshot.New(c)
	ctx := context.Background()

	log.Printf("Snapshotting %s partition %s (1-hour lifetime)", server, partition)

	if _, err := s.Create(ctx, snapshot.CreateOptions{
		Name: server, Partition: partition, Lifetime: 1,
	}); err != nil {
		log.Fatalf("Create: %v", err)
	}
	log.Printf("✓ Snapshot job queued")

	time.Sleep(3 * time.Second)

	listed, err := s.List(ctx, snapshot.ListOptions{Name: server})
	if err != nil {
		log.Fatalf("List: %v", err)
	}
	if len(listed.Return) == 0 {
		log.Fatalf("List returned no snapshots — registration may still be pending")
	}
	snap := listed.Return[len(listed.Return)-1]
	log.Printf("✓ Most recent snapshot: id=%s, name=%s, expires=%s", snap.ID, snap.Name, snap.Expires)

	if _, err := s.SetLifetime(ctx, snapshot.SetLifetimeOptions{
		Name: server, Snapshot: snap.ID, Lifetime: 2,
	}); err != nil {
		log.Printf("⚠ SetLifetime: %v (may be rate-limited if a job is still running)", err)
	} else {
		log.Printf("✓ Snapshot lifetime extended to 2 hours")
	}

	if _, err := s.Delete(ctx, snapshot.SnapshotOptions{Name: server, Snapshot: snap.ID}); err != nil {
		log.Fatalf("Delete: %v (manual cleanup of snapshot %s required)", err, snap.ID)
	}
	log.Printf("✓ Snapshot %s deleted", snap.ID)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("required env var %s is not set", name)
	}
	return v
}
