// Program probe-images dumps the cloud/image and cloud/stack/image
// catalogs side-by-side. Optional cleanup levers:
//
//	CLEANUP_KEY=<id>          delete one ssh.key
//	CLEANUP_IMAGE_PREFIX=<p>  delete every customer image whose code
//	                          starts with <p> (waits for each delete job).
//
// Used as an iteration tool while developing the custom-image
// example. Will be folded back in or removed once parent-image
// discovery and orphan-cleanup patterns are settled.
//
// Required env: SH_API_KEY.
// Optional env: SH_CLIENT_ID, CLEANUP_KEY, CLEANUP_IMAGE_PREFIX.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	cloudImage "github.com/sitehostnz/gosh/pkg/api/cloud/image"
	stackImage "github.com/sitehostnz/gosh/pkg/api/cloud/stack/image"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	sshKey "github.com/sitehostnz/gosh/pkg/api/ssh/key"
)

func main() {
	ctx := context.Background()
	c, err := info.NewClientWithDiscovery(ctx, os.Getenv("SH_API_KEY"))
	if err != nil {
		log.Fatal(err)
	}

	if id := os.Getenv("CLEANUP_KEY"); id != "" {
		if _, err := sshKey.New(c).Delete(ctx, sshKey.DeleteRequest{ID: id}); err != nil {
			log.Printf("ssh.key.Delete %s: %v", id, err)
		} else {
			log.Printf("✓ deleted ssh.key %s", id)
		}
	}

	if prefix := os.Getenv("CLEANUP_IMAGE_PREFIX"); prefix != "" {
		ci, err := cloudImage.New(c).List(ctx)
		if err != nil {
			log.Printf("list for cleanup: %v", err)
		} else {
			for _, img := range ci.Return.Images {
				if strings.HasPrefix(img.Code, prefix) {
					resp, err := cloudImage.New(c).Delete(ctx, cloudImage.DeleteRequest{Code: img.Code})
					if err != nil {
						log.Printf("delete %s: %v", img.Code, err)
						continue
					}
					if resp.Return.ID > 0 {
						if err := waitForJob(ctx, c, resp.Return.ID, resp.Return.Type, 5*time.Minute); err != nil {
							log.Printf("wait for delete %s: %v", img.Code, err)
							continue
						}
					}
					log.Printf("✓ deleted image %s", img.Code)
				}
			}
		}
	}

	fmt.Println("\n=== cloud.image.list_all (customer custom images) ===")
	ci, err := cloudImage.New(c).List(ctx)
	if err != nil {
		log.Printf("cloud.image.List: %v", err)
	} else {
		fmt.Printf("total: %d\n", len(ci.Return.Images))
		for _, img := range ci.Return.Images {
			fmt.Printf("  id=%-6s code=%-40s label=%-30s is_public=%-5v image_type=%s\n",
				img.ID, img.Code, img.Label, bool(img.IsPublic), img.ImageType)
		}
	}

	fmt.Println("\n=== cloud.stack.image.list_all (platform stack images) ===")
	si, err := stackImage.New(c).List(ctx)
	if err != nil {
		log.Printf("cloud.stack.image.List: %v", err)
	} else {
		fmt.Printf("total: %d\n", len(si.Return))
		for _, img := range si.Return {
			fmt.Printf("  id=%-6s code=%-45s label=%s\n", img.ID, img.Code, img.Label)
		}
	}
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
		time.Sleep(3 * time.Second)
	}
}
