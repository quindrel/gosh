package image

import (
	"context"
	"fmt"
	"strconv"

	stackImage "github.com/sitehostnz/gosh/pkg/api/cloud/stack/image"
)

// ForkFromImage creates a new custom image forked from a SiteHost
// platform stack image identified by parentCode (e.g.
// "sitehost-php85-apache").
//
// This composite helper resolves the parent's numeric id by listing
// /cloud/stack/image/list_all (which is where the platform's public
// images actually live — /cloud/image/list_all only returns the
// customer's own custom images, not the public catalog), then calls
// Create with that fork_id. Without this helper, consumers have to
// know the right endpoint to query and do the list-filter-extract-id
// dance themselves.
//
// label is required (used as the new image's display label).
// code is the desired image code (slug used in the GitLab repo URL
// and registry); pass "" to let the API auto-generate it from label.
// sshKeyIDs are customer-level SSH key IDs (from /ssh/key/list_keys)
// that get push access to the backing GitLab repository — at least
// one is required for the consumer to push commits.
//
// Returns the JobResponse from Create. Consumers should poll the
// job to completion before calling CloneURL or attempting a clone,
// since the GitLab repo is provisioned asynchronously.
func (s *Client) ForkFromImage(ctx context.Context, parentCode, label, code string, sshKeyIDs []int) (response JobResponse, err error) {
	if parentCode == "" {
		return response, fmt.Errorf("cloud.image.ForkFromImage: parentCode is required")
	}
	if label == "" {
		return response, fmt.Errorf("cloud.image.ForkFromImage: label is required")
	}

	listing, err := stackImage.New(s.client).List(ctx)
	if err != nil {
		return response, fmt.Errorf("listing platform stack images to resolve fork target %q: %w", parentCode, err)
	}

	var forkID int
	for _, img := range listing.Return {
		if img.Code == parentCode {
			id, perr := strconv.Atoi(img.ID)
			if perr != nil {
				return response, fmt.Errorf("platform image %q has non-numeric id %q: %w", parentCode, img.ID, perr)
			}
			forkID = id
			break
		}
	}
	if forkID == 0 {
		return response, fmt.Errorf("platform SiteHost image with code %q not found in cloud/stack/image/list_all", parentCode)
	}

	return s.Create(ctx, CreateRequest{
		Label:   label,
		Code:    code,
		ForkID:  forkID,
		SSHKeys: sshKeyIDs,
	})
}
