// Package ghapi is a dependency-free GitHub REST client scoped to exactly what
// Homeplate needs: enumerating admin-able repos/orgs, minting runner
// registration tokens, and replaying offline results as commit statuses.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion pins the REST API surface so GitHub changes cannot silently
// alter response shapes underneath us.
const APIVersion = "2022-11-28"

// Client talks to one GitHub host as one identity.
type Client struct {
	Token   string
	Host    string // "github.com" or GHES hostname
	HTTP    *http.Client
	Profile string // for error messages

	// UserAgent identifies Homeplate in GitHub's logs.
	UserAgent string

	// BaseURL overrides the derived API root entirely. It exists so tests can
	// point the client at an httptest server (which serves plain HTTP on a
	// loopback port) without weakening the https-only default for real hosts.
	BaseURL string
}

// New builds a client for github.com.
func New(token string) *Client {
	return &Client{
		Token:     token,
		Host:      "github.com",
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		UserAgent: "homeplate",
	}
}

// WithHost targets GitHub Enterprise Server.
func (c *Client) WithHost(host string) *Client {
	cp := *c
	cp.Host = host
	return &cp
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	if c.Host == "" || c.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + c.Host + "/api/v3"
}

// Error is a structured GitHub API failure.
type Error struct {
	StatusCode int
	Method     string
	Path       string
	Message    string `json:"message"`
	DocURL     string `json:"documentation_url"`
	Errors     []struct {
		Resource string `json:"resource"`
		Field    string `json:"field"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	} `json:"errors"`
	// RetryAfter is set when GitHub applies secondary rate limiting.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github %s %s: %d", e.Method, e.Path, e.StatusCode)
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	for _, sub := range e.Errors {
		fmt.Fprintf(&b, " [%s.%s %s %s]", sub.Resource, sub.Field, sub.Code, sub.Message)
	}
	return b.String()
}

// IsNotFound reports a 404, which for GitHub often means "no permission".
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

// IsForbidden reports 403.
func IsForbidden(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusForbidden
}

// IsUnauthorized reports 401 (bad or revoked token).
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusUnauthorized
}

// AsError unwraps err into a *Error, mirroring errors.As with a concrete type.
func AsError(err error, target **Error) bool {
	return errors.As(err, target)
}

// Response carries pagination and scope metadata alongside the decoded body.
type Response struct {
	StatusCode int
	NextPage   int
	Scopes     []string
	Header     http.Header
}

// do executes a request, decodes JSON into out, and returns metadata.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) (*Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.baseURL() + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	ua := c.UserAgent
	if ua == "" {
		ua = "homeplate"
	}
	req.Header.Set("User-Agent", ua)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	meta := &Response{
		StatusCode: resp.StatusCode,
		NextPage:   nextPage(resp.Header.Get("Link")),
		Scopes:     splitList(resp.Header.Get("X-OAuth-Scopes")),
		Header:     resp.Header,
	}

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := &Error{StatusCode: resp.StatusCode, Method: method, Path: path}
		_ = json.Unmarshal(raw, apiErr)
		if apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(raw))
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				apiErr.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return meta, apiErr
	}

	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return meta, fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return meta, nil
}

// Get issues a GET.
func (c *Client) Get(ctx context.Context, path string, out any) (*Response, error) {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post issues a POST.
func (c *Client) Post(ctx context.Context, path string, body, out any) (*Response, error) {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// Put issues a PUT.
func (c *Client) Put(ctx context.Context, path string, body, out any) (*Response, error) {
	return c.do(ctx, http.MethodPut, path, body, out)
}

// Delete issues a DELETE.
func (c *Client) Delete(ctx context.Context, path string) (*Response, error) {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func nextPage(link string) int {
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		if p, err := strconv.Atoi(u.Query().Get("page")); err == nil {
			return p
		}
	}
	return 0
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withPage appends ?page=N&per_page=M to a path that may already have a query.
func withPage(path string, page, perPage int) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%spage=%d&per_page=%d", path, sep, page, perPage)
}
