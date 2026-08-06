package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultClientID is the OAuth App client_id used for Device Flow.
//
// HONEST NOTE (see README "Known limits"): Device Flow REQUIRES a registered
// OAuth App client_id - GitHub has no anonymous/generic device flow, and
// device flow must be explicitly enabled in the app's settings. (No client
// SECRET is needed, which is why this works from a CLI.)
//
// Homerun ships with no client_id baked in. The GitHub CLI's client_id is
// public and technically works, but using it would make your tokens appear to
// GitHub as "GitHub CLI", put them under an app you do not control, and
// impersonate another product. Homerun will not do that by default. Options:
//
//  1. Register your own OAuth App (30 seconds, no secret needed for device
//     flow), then `homerun auth add work --client-id Iv1.xxxxxxxx`
//     or export HOMERUN_GITHUB_CLIENT_ID.
//  2. Distributors set it at build time:
//     -ldflags "-X github.com/homerun-ci/homerun/internal/auth.DefaultClientID=Iv1.xxxx"
//  3. Skip OAuth entirely: `homerun auth add work --pat` (fine-grained PAT).
var DefaultClientID = ""

// Scopes required for full Homerun operation with a classic OAuth token.
//
//	repo         - repo-level runner registration tokens (GitHub's own docs:
//	               "OAuth tokens and personal access tokens (classic) need the
//	               `repo` scope"), plus commit statuses and PR merges
//	admin:org    - ORG-level runner registration tokens
//	workflow     - required only for `homerun adopt` (rewriting workflow YAML)
//
// Fine-grained PAT equivalents:
//
//	repo runners : repository permission "Administration: Read and write"
//	org runners  : organization permission "Self-hosted runners: Read and write"
//	statuses     : repository permission "Commit statuses: Read and write"
var DefaultScopes = []string{"repo", "workflow", "admin:org"}

// DeviceCode is the first-leg response of the device flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// ExpiresAt is when the user code stops working.
func (d DeviceCode) ExpiresAt() time.Time {
	return time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
}

// DeviceFlow drives GitHub's OAuth 2.0 Device Authorization Grant.
type DeviceFlow struct {
	ClientID string
	Host     string // "github.com" or a GHES hostname
	Scopes   []string
	HTTP     *http.Client
}

// NewDeviceFlow builds a flow, resolving the client_id from flag > env > build.
func NewDeviceFlow(clientID, host string, scopes []string) (*DeviceFlow, error) {
	if clientID == "" {
		clientID = os.Getenv("HOMERUN_GITHUB_CLIENT_ID")
	}
	if clientID == "" {
		clientID = DefaultClientID
	}
	if clientID == "" {
		return nil, errors.New(
			"device flow needs an OAuth App client_id.\n" +
				"  Fastest path:  homerun auth add <name> --pat        (paste a fine-grained PAT)\n" +
				"  Or register an OAuth App at https://github.com/settings/applications/new\n" +
				"  (enable \"Device flow\"; no client secret needed) then re-run with\n" +
				"    homerun auth add <name> --client-id Iv1.xxxxxxxxxxxx\n" +
				"  or export HOMERUN_GITHUB_CLIENT_ID=Iv1.xxxxxxxxxxxx")
	}
	if host == "" {
		host = "github.com"
	}
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	return &DeviceFlow{
		ClientID: clientID,
		Host:     host,
		Scopes:   scopes,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (f *DeviceFlow) baseURL() string {
	if f.Host == "github.com" || f.Host == "" {
		return "https://github.com"
	}
	return "https://" + f.Host
}

// RequestCode performs POST /login/device/code.
func (f *DeviceFlow) RequestCode(ctx context.Context) (*DeviceCode, error) {
	form := url.Values{}
	form.Set("client_id", f.ClientID)
	form.Set("scope", strings.Join(f.Scopes, " "))

	var dc DeviceCode
	if err := f.postForm(ctx, f.baseURL()+"/login/device/code", form, &dc); err != nil {
		return nil, err
	}
	if dc.DeviceCode == "" {
		return nil, errors.New("device flow: GitHub returned no device_code (is Device Flow enabled on the OAuth App?)")
	}
	if dc.Interval <= 0 {
		dc.Interval = 5
	}
	return &dc, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

// Poll blocks until the user authorizes, the code expires, or ctx is done.
// It honours GitHub's slow_down backoff exactly as the spec requires.
func (f *DeviceFlow) Poll(ctx context.Context, dc *DeviceCode, onTick func(remaining time.Duration)) (token string, scopes []string, err error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := dc.ExpiresAt()

	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return "", nil, errors.New("device flow: the code expired before it was authorized")
		}
		if onTick != nil {
			onTick(time.Until(deadline))
		}

		form := url.Values{}
		form.Set("client_id", f.ClientID)
		form.Set("device_code", dc.DeviceCode)
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

		var tr tokenResponse
		if err := f.postForm(ctx, f.baseURL()+"/login/oauth/access_token", form, &tr); err != nil {
			return "", nil, err
		}
		switch tr.Error {
		case "":
			if tr.AccessToken == "" {
				return "", nil, errors.New("device flow: empty access_token with no error")
			}
			return tr.AccessToken, splitScopes(tr.Scope), nil
		case "authorization_pending":
			// Expected: keep polling at the current interval.
		case "slow_down":
			// GitHub asks us to back off; it may supply a new interval.
			if tr.Interval > 0 {
				interval = time.Duration(tr.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		case "expired_token":
			return "", nil, errors.New("device flow: the code expired; run the command again")
		case "access_denied":
			return "", nil, errors.New("device flow: authorization was denied in the browser")
		case "device_flow_disabled":
			return "", nil, errors.New("device flow: this OAuth App does not have Device Flow enabled " +
				"(check the box in the app settings)")
		case "incorrect_client_credentials":
			return "", nil, fmt.Errorf("device flow: client_id %q was rejected by GitHub", f.ClientID)
		default:
			d := tr.ErrorDescription
			if d == "" {
				d = tr.Error
			}
			return "", nil, fmt.Errorf("device flow: %s", d)
		}
	}
}

func (f *DeviceFlow) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Without this header GitHub replies in form-urlencoded, not JSON.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "homerun")

	resp, err := f.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("device flow: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("device flow: GitHub returned %s (GitHub may be degraded; check `homerun doctor`)", resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("device flow: decoding %s response: %w", endpoint, err)
	}
	return nil
}

func splitScopes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
