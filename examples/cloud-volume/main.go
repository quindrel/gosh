// Program cloud-volume demonstrates the cloud volume lifecycle:
//
//   1. Create a small volume on a cloud server
//   2. List all volumes (filtered by server)
//   3. Get the new volume's details
//   4. Delete the volume
//
// Required environment:
//
//	SH_API_KEY        — your SiteHost API key
//	SH_CLIENT_ID      — your client ID
//	SH_CLOUD_SERVER   — name of a cloud container you don't mind
//	                    briefly attaching a test volume to. The
//	                    example creates the volume, lists, gets
//	                    its details, and deletes within a few
//	                    seconds.
//
// Creates and deletes a real volume. Storage costs are negligible
// for the brief lifetime.
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
	"github.com/sitehostnz/gosh/pkg/api/cloud/volume"
)

func main() {
	apiKey := mustEnv("SH_API_KEY")
	clientID := mustEnv("SH_CLIENT_ID")
	server := mustEnv("SH_CLOUD_SERVER")

	c, err := api.New(apiKey, clientID)
	if err != nil {
		log.Fatalf("api.New: %v", err)
	}
	v := volume.New(c)
	ctx := context.Background()

	var rb [4]byte
	_, _ = rand.Read(rb[:])
	// Volume names have an undocumented max length around ~16
	// chars — longer names get rejected with a misleading
	// "letters, numbers, hyphens, underscores" message even when
	// they only contain those. See docs/open-api-questions.md.
	volName := fmt.Sprintf("goshv%s", hex.EncodeToString(rb[:]))
	log.Printf("Using test volume: %s on %s", volName, server)

	if _, err := v.Add(ctx, volume.AddOptions{ServerName: server, VolumeName: volName}); err != nil {
		log.Fatalf("Add: %v", err)
	}
	log.Printf("✓ Volume created")

	// Briefly settle.
	time.Sleep(2 * time.Second)

	listed, err := v.List(ctx, &volume.ListOptions{ServerName: server})
	if err != nil {
		log.Printf("⚠ List: %v", err)
	} else {
		log.Printf("✓ List returned %d volume(s) on %s", len(listed.Return.Data), server)
	}

	got, err := v.Get(ctx, volume.GetOptions{Server: server, Volume: volName})
	if err != nil {
		log.Printf("⚠ Get: %v", err)
	} else {
		log.Printf("✓ Get: id=%s, pending=%q, %d container(s) attached",
			got.Return.ID, got.Return.Pending, len(got.Return.Containers))
	}

	if _, err := v.Delete(ctx, volume.DeleteOptions{Server: server, Volume: volName}); err != nil {
		log.Fatalf("Delete: %v (manual cleanup of %s required)", err, volName)
	}
	log.Printf("✓ Volume %s deleted", volName)
}

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		log.Fatalf("required env var %s is not set", name)
	}
	return v
}
