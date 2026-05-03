package image

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sitehostnz/gosh/pkg/api/job"
)

// transientDeleteHints — substrings observed in the *job's* failure
// message when cloud.image.Delete is called too soon after a build
// or push. Empirically, these failures clear within a few seconds.
// Match on Message text rather than HTTP status because the API
// layer returns success (200, status:true) and tucks the failure
// into the scheduler-job state.
var transientDeleteHints = []string{
	"could not delete your custom image",
	"Please contact support@sitehost.co.nz",
}

// DeleteAndWait calls cloud.image.Delete and waits for the resulting
// scheduler job to complete. If the job fails with a known-transient
// "could not delete your custom image right now" message — typically
// observed when an image has just been built or pushed to and the
// platform's GC machinery hasn't released the lock yet — the helper
// re-issues Delete with backoff up to maxAttempts times.
//
// Without this retry, AI agents and other automated consumers see
// every fresh-image cleanup fail on the first attempt and accumulate
// orphan image records. The transient is *not* surfaced via HTTP
// status — both the API call and CheckResponse return success; the
// failure is in the scheduler job's Message field.
//
// Returns nil only after a delete job reaches Completed. If the
// final attempt's job fails with a non-transient message, that
// error is surfaced. If maxAttempts is 0, defaults to 5.
func (s *Client) DeleteAndWait(ctx context.Context, code string, maxAttempts int, perAttemptTimeout, retryBackoff time.Duration) error {
	if code == "" {
		return fmt.Errorf("cloud.image.DeleteAndWait: code is required")
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	if perAttemptTimeout <= 0 {
		perAttemptTimeout = 2 * time.Minute
	}
	if retryBackoff <= 0 {
		retryBackoff = 10 * time.Second
	}

	jc := job.New(s.client)
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := s.Delete(ctx, DeleteRequest{Code: code})
		if err != nil {
			return fmt.Errorf("cloud.image.Delete(attempt %d): %w", attempt, err)
		}
		if resp.Return.ID == 0 {
			// No job returned — treat as already-deleted / nothing-to-do.
			return nil
		}

		jobErr := waitForDeleteJob(ctx, jc, resp.Return.ID, resp.Return.Type, perAttemptTimeout)
		if jobErr == nil {
			return nil
		}

		if !isTransientDeleteError(jobErr) {
			return fmt.Errorf("cloud.image.Delete(attempt %d): %w", attempt, jobErr)
		}
		lastErr = jobErr

		if attempt < maxAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
	}

	return fmt.Errorf("cloud.image.DeleteAndWait: gave up after %d attempts: %w", maxAttempts, lastErr)
}

// waitForDeleteJob polls the job until it reaches Completed or
// Failed. Distinct from a generic poller so we can return the
// job's Message verbatim — the retry logic in DeleteAndWait keys
// off it.
func waitForDeleteJob(ctx context.Context, jc *job.Client, id int, jobType string, timeout time.Duration) error {
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
		case <-time.After(3 * time.Second):
		}
	}
}

func isTransientDeleteError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, hint := range transientDeleteHints {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}
