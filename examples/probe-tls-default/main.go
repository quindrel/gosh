// Program probe-tls-default discovers the platform-default minimum
// TLS version on a fresh Cloud Container Server, then optionally
// applies that value to a target CCS.
//
// Why this exists: cloud/server/update_minimum_tls_version.json sets
// the value, but there's no read endpoint exposing the current value.
// The only way to learn the platform default is to provision a fresh
// CCS and observe what its nginx-proxy actually negotiates. This
// example does that, end-to-end, via gosh.
//
// Required env:
//
//	SH_API_KEY     — your API key
//
// Optional env:
//
//	SH_CLIENT_ID         — sub-account targeting; otherwise discovered
//	APPLY_TO_SERVER      — name of an existing CCS to apply the
//	                       discovered default to (revert path after
//	                       an accidental change). Skipped if unset.
//	JOURNEY_KEEP=1       — leave the probe CCS in place; otherwise
//	                       delete it after the probe.
//
// Side effects:
//   - Provisions one CCS (CLDCON4-P, AKLCITY) for the duration of the
//     probe. Tears down by default.
//   - If APPLY_TO_SERVER is set, calls update_minimum_tls_version on
//     that server. Print prominently before doing so.
//
// This is dogfood: every API interaction goes through gosh. The one
// exception is cloud/server/update_minimum_tls_version.json itself,
// which gosh doesn't yet wrap — those are inline raw calls flagged
// with TODO(gosh:cloud-server-config).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/sitehostnz/gosh/pkg/api"
	"github.com/sitehostnz/gosh/pkg/api/info"
	"github.com/sitehostnz/gosh/pkg/api/job"
	pnet "github.com/sitehostnz/gosh/pkg/net"
	"github.com/sitehostnz/gosh/pkg/api/server"
)

const (
	location    = "AKLCITY"
	productCode = "CLDCON4-P" // Performance Cloud Container - 4 Core; matches ch-faraday
	imageCode   = "ubuntu-focal.amd64.cloud.3002-2"
)

func main() {
	ctx := context.Background()
	c, err := buildClient(ctx)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	srvClient := server.New(c)

	// 1. Find a free IP. server.ListIPs lives on feat/server-reads-extras
	// which hasn't been merged into feat/examples yet — using inline raw
	// call to keep this example self-contained until we consolidate
	// branches. TODO(gosh:server-reads-extras): swap to server.ListIPs.
	ipAddr, err := rawFirstFreeIP(c, location)
	if err != nil {
		log.Fatalf("list IPs: %v", err)
	}
	log.Printf("✓ free IP in %s: %s", location, ipAddr)

	// 2. Provision the CCS.
	createResp, err := srvClient.Create(ctx, server.CreateRequest{
		Label:       "gosh-tls-default-probe-" + tsString(),
		Location:    location,
		ProductCode: productCode,
		Image:       imageCode,
		Params:      server.ParamsOptions{IPv4: []string{ipAddr}},
	})
	if err != nil {
		log.Fatalf("server.Create: %v", err)
	}
	newName := createResp.Return.Name
	log.Printf("✓ provisioning CCS %q (job %d, type=%s)",
		newName, createResp.Return.ID, createResp.Return.Type)

	// Defer cleanup unless JOURNEY_KEEP=1.
	defer func() {
		if os.Getenv("JOURNEY_KEEP") == "1" {
			log.Printf("- JOURNEY_KEEP=1 — leaving CCS %s in place at %s", newName, ipAddr)
			return
		}
		// Delete the probe CCS via gosh.
		log.Printf("─── cleanup ───")
		if _, err := srvClient.Delete(ctx, server.DeleteRequest{Name: newName}); err != nil {
			log.Printf("⚠ server.Delete %s: %v", newName, err)
			return
		}
		log.Printf("✓ delete queued for %s", newName)
	}()

	// 3. Wait for provision job to complete.
	if err := waitForJob(ctx, c, createResp.Return.ID, createResp.Return.Type, 15*time.Minute); err != nil {
		log.Fatalf("provision job: %v", err)
	}
	log.Printf("✓ CCS provisioned; sleeping 30s for nginx-proxy to come up")
	time.Sleep(30 * time.Second)

	// 4. Probe TLS at each version. The lowest version that handshakes
	// is the platform default minimum.
	log.Printf("─── probing TLS versions on %s:443 ───", ipAddr)
	defaultMin := probeTLS(ipAddr)
	if defaultMin == "" {
		log.Printf("⚠ could not determine TLS minimum — nginx-proxy may not be listening; try JOURNEY_KEEP=1 and rerun the probe")
		return
	}
	log.Printf("✓ platform default minimum TLS: %s", defaultMin)

	// 5. Optionally apply to a target server.
	if target := os.Getenv("APPLY_TO_SERVER"); target != "" {
		log.Println()
		log.Printf("  ╔═══════════════════════════════════════════════════════════╗")
		log.Printf("  ║  ABOUT TO SET TLS MIN ON: %-32s║", target)
		log.Printf("  ║  Setting to: %-45s║", defaultMin)
		log.Printf("  ╚═══════════════════════════════════════════════════════════╝")
		log.Println()
		if err := setMinTLS(c, target, defaultMin); err != nil {
			log.Fatalf("set min TLS: %v", err)
		}
		log.Printf("✓ %s minimum TLS set to %s", target, defaultMin)
	}
}

// buildClient — info.NewClientWithDiscovery if SH_CLIENT_ID is unset,
// else api.New directly (preserves sub-account targeting).
func buildClient(ctx context.Context) (*api.Client, error) {
	apiKey := os.Getenv("SH_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("SH_API_KEY must be set")
	}
	if cid := os.Getenv("SH_CLIENT_ID"); cid != "" {
		return api.New(apiKey, cid)
	}
	return info.NewClientWithDiscovery(ctx, apiKey)
}

// waitForJob polls /job/get until state=Completed or Failed.
// Mirrors examples/build-a-site's helper; consider lifting both to a
// shared helper package after we squash examples.
func waitForJob(ctx context.Context, c *api.Client, id int, jobType string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	jc := job.New(c)
	prev := ""
	var sleep time.Duration
	for time.Now().Before(deadline) {
		resp, err := jc.Get(ctx, job.GetRequest{ID: id, Type: jobType})
		if err == nil {
			if resp.Return.State != prev {
				log.Printf("  job state: %s", resp.Return.State)
				prev = resp.Return.State
			}
			switch resp.Return.State {
			case "Completed":
				return nil
			case "Failed":
				return fmt.Errorf("job %d (%s) failed: %s", id, jobType, resp.Return.Message)
			}
		}
		sleep = nextBackoff(sleep)
		time.Sleep(sleep)
	}
	return fmt.Errorf("job %d (%s) did not reach terminal state in %s", id, jobType, timeout)
}

// nextBackoff returns the next sleep in 1s → 2s → 4s → 8s → 10s cap.
func nextBackoff(prev time.Duration) time.Duration {
	if prev == 0 {
		return time.Second
	}
	if next := prev * 2; next < 10*time.Second {
		return next
	}
	return 10 * time.Second
}

// probeTLS attempts handshakes at each version, returns the lowest
// version that's ACCEPTED (i.e., the floor). Empty string if nothing
// handshakes (port closed, nginx not up, etc.).
func probeTLS(ipAddr string) string {
	addr := net.JoinHostPort(ipAddr, "443")
	versions := []struct {
		name string
		v    uint16
	}{
		{"TLSv1.0", tls.VersionTLS10},
		{"TLSv1.1", tls.VersionTLS11},
		{"TLSv1.2", tls.VersionTLS12},
		{"TLSv1.3", tls.VersionTLS13},
	}
	floor := ""
	for _, v := range versions {
		cfg := &tls.Config{InsecureSkipVerify: true, MinVersion: v.v, MaxVersion: v.v}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, cfg)
		if err != nil {
			log.Printf("  %s  REJECTED  (%v)", v.name, summariseTLSErr(err))
			continue
		}
		_ = conn.Close()
		log.Printf("  %s  ACCEPTED", v.name)
		if floor == "" {
			floor = v.name
		}
	}
	return floor
}

// summariseTLSErr trims the verbose dial error to its core message.
func summariseTLSErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// setMinTLS calls cloud/server/update_minimum_tls_version.json on
// the named CCS. TODO(gosh:cloud-server-config): wrap this in a
// proper cloud/server package operation, then drop this raw call.
func setMinTLS(c *api.Client, serverName, version string) error {
	values := url.Values{}
	values.Add("client_id", c.ClientID)
	values.Add("server_name", serverName)
	values.Add("minimum_tls_version", version)
	body := pnet.Encode(values, []string{"client_id", "server_name", "minimum_tls_version"})
	req, err := c.NewRequest("POST", "cloud/server/update_minimum_tls_version.json", body)
	if err != nil {
		return err
	}
	var resp struct {
		Return struct {
			ID   int    `json:"id"`
			Type string `json:"type"`
		} `json:"return"`
		Status bool   `json:"status"`
		Msg    string `json:"msg"`
	}
	if err := c.Do(context.Background(), req, &resp); err != nil {
		return err
	}
	if !resp.Status {
		return fmt.Errorf("API: %s", resp.Msg)
	}
	if resp.Return.ID > 0 {
		return waitForJob(context.Background(), c, resp.Return.ID, resp.Return.Type, 5*time.Minute)
	}
	return nil
}

func tsString() string { return strconv.FormatInt(time.Now().Unix(), 10) }

// rawFirstFreeIP — inline call to server/list_ips.json. Swap to
// gosh's server.ListIPs once feat/server-reads-extras is on this
// branch. The response shape is [{ip_addr, prefix, family}].
func rawFirstFreeIP(c *api.Client, loc string) (string, error) {
	req, err := c.NewRequest("GET", "server/list_ips.json", "")
	if err != nil {
		return "", err
	}
	v := req.URL.Query()
	v.Add("location", loc)
	req.URL.RawQuery = pnet.Encode(v, []string{"apikey", "client_id", "location"})
	var resp struct {
		Return []struct {
			IPAddr string `json:"ip_addr"`
		} `json:"return"`
	}
	if err := c.Do(context.Background(), req, &resp); err != nil {
		return "", err
	}
	if len(resp.Return) == 0 {
		return "", fmt.Errorf("no free IPs at %s", loc)
	}
	return resp.Return[0].IPAddr, nil
}

// silence unused-import linter — io and json kept for future shape probes.
var _ = io.Discard
var _ = json.Marshal
var _ = http.MethodGet
