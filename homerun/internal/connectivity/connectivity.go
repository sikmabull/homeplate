// Package connectivity decides which engine should be used right now.
//
// Homerun distinguishes three states, because they demand different responses:
//
//	Online       - GitHub reachable and Actions healthy      -> Engine A
//	Degraded     - GitHub reachable but Actions is down/slow  -> Engine B, replay later
//	Offline      - no network at all                          -> Engine B, replay later
package connectivity

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// State is the current connectivity verdict.
type State string

const (
	Online   State = "online"
	Degraded State = "degraded"
	Offline  State = "offline"
)

// UseOffline reports whether Engine B should handle work in this state.
func (s State) UseOffline() bool { return s != Online }

// UseOffline reports whether the snapshot implies Engine B.
func (s Status) UseOffline() bool { return s.State.UseOffline() }

// Status is a full connectivity snapshot with reasons.
type Status struct {
	State State
	// Reason is always populated, so the daemon log and `homerun status`
	// can explain WHY jobs went to a particular engine.
	Reason string
	// ActionsIndicator is githubstatus.com's indicator: none|minor|major|critical.
	ActionsIndicator string
	// ActionsDescription is the human status text from GitHub.
	ActionsDescription string
	// APIReachable is whether api.github.com answered.
	APIReachable bool
	CheckedAt    time.Time
}

// statusPageURL is GitHub's Statuspage summary endpoint.
//
// Statuspage exposes:
//
//	/api/v2/status.json      - overall indicator only
//	/api/v2/components.json  - per-component status (what we need)
//	/api/v2/summary.json     - both, one request
//
// summary.json is used so a single request covers the overall indicator and
// the specific "Actions" component.
const statusPageURL = "https://www.githubstatus.com/api/v2/summary.json"

// ActionsComponent is the component name on githubstatus.com.
const ActionsComponent = "Actions"

// Monitor polls connectivity and caches the result.
type Monitor struct {
	HTTP     *http.Client
	Interval time.Duration
	// APIHost is overridable for GHES.
	APIHost string

	mu      sync.RWMutex
	last    Status
	lastRun time.Time
}

// NewMonitor builds a monitor with sane timeouts. Timeouts are short because a
// hung status check must never stall job pickup.
func NewMonitor() *Monitor {
	return &Monitor{
		HTTP:     &http.Client{Timeout: 8 * time.Second},
		Interval: 2 * time.Minute,
		APIHost:  "api.github.com",
	}
}

// Current returns the cached status, refreshing if stale.
func (m *Monitor) Current(ctx context.Context) Status {
	m.mu.RLock()
	last, at := m.last, m.lastRun
	m.mu.RUnlock()
	if time.Since(at) < m.Interval && last.State != "" {
		return last
	}
	return m.Check(ctx)
}

// Check performs a fresh probe.
func (m *Monitor) Check(ctx context.Context) Status {
	s := Status{CheckedAt: time.Now().UTC()}

	// Step 1: can we reach GitHub's API at all? This is a TCP+TLS+HTTP probe
	// against a cheap unauthenticated endpoint.
	s.APIReachable = m.probeAPI(ctx)
	if !s.APIReachable {
		s.State = Offline
		s.Reason = "api.github.com is unreachable (no network, DNS failure, or GitHub is hard down)"
		m.store(s)
		return s
	}

	// Step 2: GitHub is reachable, but is ACTIONS healthy? A green API with a
	// red Actions component is exactly the "GitHub is degraded" case that
	// Engine B exists for.
	ind, desc, err := m.probeStatusPage(ctx)
	if err != nil {
		// Cannot read the status page but the API works: trust the API.
		s.State = Online
		s.Reason = fmt.Sprintf("api.github.com is reachable; githubstatus.com unreadable (%v), assuming healthy", err)
		m.store(s)
		return s
	}
	s.ActionsIndicator = ind
	s.ActionsDescription = desc

	switch ind {
	case "none", "":
		s.State = Online
		s.Reason = "GitHub Actions is operational"
	case "minor":
		// Minor incidents usually still dispatch jobs. Staying on Engine A
		// avoids needlessly degrading to act's weaker fidelity.
		s.State = Online
		s.Reason = "GitHub Actions reports a minor incident (" + desc + "); staying on connected mode"
	default: // major, critical
		s.State = Degraded
		s.Reason = "GitHub Actions is degraded (" + ind + ": " + desc + "); switching to offline mode"
	}
	m.store(s)
	return s
}

func (m *Monitor) store(s Status) {
	m.mu.Lock()
	m.last, m.lastRun = s, time.Now()
	m.mu.Unlock()
}

func (m *Monitor) probeAPI(ctx context.Context) bool {
	host := m.APIHost
	if host == "" {
		host = "api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	// A DNS+TCP check first gives a fast, unambiguous offline signal.
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", host+":443")
	if err != nil {
		return false
	}
	conn.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/meta", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "homerun")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any answer at all (including 403 rate limit) proves reachability.
	return resp.StatusCode < 500
}

type summaryResponse struct {
	Status struct {
		Indicator   string `json:"indicator"`
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"components"`
	Incidents []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Impact string `json:"impact"`
	} `json:"incidents"`
}

// probeStatusPage reads the Actions component specifically. GitHub's overall
// indicator can be "major" because of Codespaces while Actions is fine, so
// keying off the component is materially more accurate.
func (m *Monitor) probeStatusPage(ctx context.Context) (indicator, description string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusPageURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "homerun")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("githubstatus.com returned %s", resp.Status)
	}
	var out summaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}

	for _, c := range out.Components {
		if !strings.EqualFold(c.Name, ActionsComponent) {
			continue
		}
		switch c.Status {
		case "operational":
			return "none", "Actions operational", nil
		case "degraded_performance":
			return "minor", "Actions degraded performance", nil
		case "partial_outage":
			return "major", "Actions partial outage", nil
		case "major_outage":
			return "critical", "Actions major outage", nil
		default:
			return "minor", "Actions status: " + c.Status, nil
		}
	}
	// No Actions component found: fall back to the overall indicator.
	return out.Status.Indicator, out.Status.Description, nil
}
