package ghapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// User is the authenticated identity.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Name  string `json:"name"`
}

// Whoami returns the authenticated user and the token's OAuth scopes.
// For fine-grained PATs the scope list is empty (GitHub sends no
// X-OAuth-Scopes header), which callers must treat as "unknown", not "none".
func (c *Client) Whoami(ctx context.Context) (*User, []string, error) {
	var u User
	resp, err := c.Get(ctx, "/user", &u)
	if err != nil {
		return nil, nil, err
	}
	return &u, resp.Scopes, nil
}

// Org is a GitHub organization the identity belongs to.
type Org struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	// Role is filled in by ListOrgs via the memberships endpoint.
	Role string `json:"-"`
}

// Repo is a repository, with the viewer's permissions attached.
type Repo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Fork     bool   `json:"fork"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
		Maint bool `json:"maintain"`
		Push  bool `json:"push"`
	} `json:"permissions"`
	DefaultBranch string     `json:"default_branch"`
	CloneURL      string     `json:"clone_url"`
	SSHURL        string     `json:"ssh_url"`
	PushedAt      *time.Time `json:"pushed_at"`
}

// IsPublic is the inverse of Private, spelled out for readability at call sites
// where the public-repo security gate is enforced.
func (r Repo) IsPublic() bool { return !r.Private }

// ListRepos returns every repo the identity can see, across personal and org
// ownership. Callers filter on Permissions.Admin to find linkable repos.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	var all []Repo
	page := 1
	for page > 0 && page <= 100 {
		var batch []Repo
		path := withPage("/user/repos?affiliation=owner,organization_member,collaborator&sort=pushed", page, 100)
		resp, err := c.Get(ctx, path, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		page = resp.NextPage
	}
	return all, nil
}

// ListOrgs returns organizations where the identity is an admin/owner.
// Runner registration at org level requires owner rights, so non-admin orgs
// are excluded rather than shown and then failing at registration time.
func (c *Client) ListOrgs(ctx context.Context) ([]Org, error) {
	var orgs []Org
	page := 1
	for page > 0 && page <= 100 {
		var batch []Org
		resp, err := c.Get(ctx, withPage("/user/orgs", page, 100), &batch)
		if err != nil {
			return nil, err
		}
		orgs = append(orgs, batch...)
		page = resp.NextPage
	}

	var admin []Org
	for _, o := range orgs {
		var m struct {
			Role  string `json:"role"`
			State string `json:"state"`
		}
		if _, err := c.Get(ctx, "/user/memberships/orgs/"+o.Login, &m); err != nil {
			// A fine-grained PAT may not carry the members permission; skip
			// rather than abort the whole listing.
			continue
		}
		if m.Role == "admin" && m.State == "active" {
			o.Role = m.Role
			admin = append(admin, o)
		}
	}
	return admin, nil
}

// GetRepo fetches a single repo by "owner/name".
func (c *Client) GetRepo(ctx context.Context, slug string) (*Repo, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var r Repo
	if _, err := c.Get(ctx, fmt.Sprintf("/repos/%s/%s", owner, name), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SplitSlug parses "owner/repo".
func SplitSlug(slug string) (owner, name string, err error) {
	parts := strings.Split(strings.TrimSpace(slug), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected owner/repo, got %q", slug)
	}
	return parts[0], parts[1], nil
}

// ---------- Runner registration ----------

// RegistrationToken is a short-lived (1 hour) token used by config.sh.
type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Expired reports whether the token is unusable, with a safety margin because
// registration itself takes a few seconds.
func (t RegistrationToken) Expired() bool {
	return time.Now().After(t.ExpiresAt.Add(-2 * time.Minute))
}

// RepoRegistrationToken mints a runner registration token for one repository.
// Requires: classic `repo` scope, or fine-grained "Administration: read+write".
func (c *Client) RepoRegistrationToken(ctx context.Context, slug string) (*RegistrationToken, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var t RegistrationToken
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/registration-token", owner, name), nil, &t)
	if err != nil {
		return nil, annotateAdminErr(err, slug, "repository")
	}
	return &t, nil
}

// OrgRegistrationToken mints a runner registration token for an organization.
// Requires: classic `admin:org` scope, or fine-grained org
// "Self-hosted runners: read+write".
func (c *Client) OrgRegistrationToken(ctx context.Context, org string) (*RegistrationToken, error) {
	var t RegistrationToken
	_, err := c.Post(ctx, fmt.Sprintf("/orgs/%s/actions/runners/registration-token", org), nil, &t)
	if err != nil {
		return nil, annotateAdminErr(err, org, "organization")
	}
	return &t, nil
}

// RepoRemoveToken mints a token that de-registers a repo runner.
func (c *Client) RepoRemoveToken(ctx context.Context, slug string) (*RegistrationToken, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var t RegistrationToken
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/remove-token", owner, name), nil, &t)
	return &t, err
}

// OrgRemoveToken mints a token that de-registers an org runner.
func (c *Client) OrgRemoveToken(ctx context.Context, org string) (*RegistrationToken, error) {
	var t RegistrationToken
	_, err := c.Post(ctx, fmt.Sprintf("/orgs/%s/actions/runners/remove-token", org), nil, &t)
	return &t, err
}

func annotateAdminErr(err error, target, kind string) error {
	switch {
	case IsNotFound(err):
		return fmt.Errorf("%w\n  -> Homeplate could not mint a runner registration token for %s %s.\n"+
			"     GitHub returns 404 (not 403) when the token lacks admin rights.\n"+
			"     Needed: classic scope `%s`, or a fine-grained PAT with %s.",
			err, kind, target, scopeHint(kind), permHint(kind))
	case IsForbidden(err):
		return fmt.Errorf("%w\n  -> Token was rejected for %s %s (SSO not authorized, or org policy blocks self-hosted runners)", err, kind, target)
	}
	return err
}

func scopeHint(kind string) string {
	if kind == "organization" {
		return "admin:org"
	}
	return "repo"
}

func permHint(kind string) string {
	if kind == "organization" {
		return `organization permission "Self-hosted runners: Read and write"`
	}
	return `repository permission "Administration: Read and write"`
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	Status    string `json:"status"`
	Busy      bool   `json:"busy"`
	Ephemeral bool   `json:"ephemeral"`
	Labels    []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"labels"`
}

// LabelNames flattens the label objects.
func (r Runner) LabelNames() []string {
	out := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		out = append(out, l.Name)
	}
	return out
}

type runnerList struct {
	TotalCount int      `json:"total_count"`
	Runners    []Runner `json:"runners"`
}

// ListRepoRunners lists runners registered to a repository.
func (c *Client) ListRepoRunners(ctx context.Context, slug string) ([]Runner, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var out []Runner
	page := 1
	for page > 0 {
		var l runnerList
		resp, err := c.Get(ctx, withPage(fmt.Sprintf("/repos/%s/%s/actions/runners", owner, name), page, 100), &l)
		if err != nil {
			return nil, err
		}
		out = append(out, l.Runners...)
		page = resp.NextPage
	}
	return out, nil
}

// ListOrgRunners lists runners registered to an organization.
func (c *Client) ListOrgRunners(ctx context.Context, org string) ([]Runner, error) {
	var out []Runner
	page := 1
	for page > 0 {
		var l runnerList
		resp, err := c.Get(ctx, withPage(fmt.Sprintf("/orgs/%s/actions/runners", org), page, 100), &l)
		if err != nil {
			return nil, err
		}
		out = append(out, l.Runners...)
		page = resp.NextPage
	}
	return out, nil
}

// DeleteRepoRunner removes a runner registration from a repo.
func (c *Client) DeleteRepoRunner(ctx context.Context, slug string, id int64) error {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return err
	}
	_, err = c.Delete(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", owner, name, id))
	return err
}

// DeleteOrgRunner removes a runner registration from an org.
func (c *Client) DeleteOrgRunner(ctx context.Context, org string, id int64) error {
	_, err := c.Delete(ctx, fmt.Sprintf("/orgs/%s/actions/runners/%d", org, id))
	return err
}

// ---------- JIT (just-in-time) runner configs ----------

// JITConfigRequest is the generate-jitconfig payload. RunnerGroupID 1 is the
// "Default" group everywhere; named runner groups still use the registration
// -token flow because their IDs need a separate lookup.
type JITConfigRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

// JITConfig is the minted, single-use, always-ephemeral runner configuration.
type JITConfig struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// GenerateRepoJITConfig mints a per-job runner config for a repository.
//
// WHY PREFER THIS OVER registration-token: the config is created server-side,
// is exactly-once, and is passed to `run.sh --jitconfig` - no secret ever
// touches disk or a container filesystem. Same scopes as registration-token
// (classic `repo`, fine-grained Administration: write).
func (c *Client) GenerateRepoJITConfig(ctx context.Context, slug string, req JITConfigRequest) (*JITConfig, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var out JITConfig
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", owner, name), req, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GenerateOrgJITConfig mints a per-job runner config for an organization.
func (c *Client) GenerateOrgJITConfig(ctx context.Context, org string, req JITConfigRequest) (*JITConfig, error) {
	var out JITConfig
	_, err := c.Post(ctx, fmt.Sprintf("/orgs/%s/actions/runners/generate-jitconfig", org), req, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GenerateJITConfigFor picks repo or org scope from the target shape.
// slug is "owner/repo" for repos, "owner" for orgs (scope string decides).
func (c *Client) GenerateJITConfigFor(ctx context.Context, scope, slug string, req JITConfigRequest) (*JITConfig, error) {
	if scope == "org" {
		return c.GenerateOrgJITConfig(ctx, slug, req)
	}
	return c.GenerateRepoJITConfig(ctx, slug, req)
}

// ---------- Result replay: statuses, checks, PRs ----------

// StatusState is a commit status state.
type StatusState string

const (
	StatusPending StatusState = "pending"
	StatusSuccess StatusState = "success"
	StatusFailure StatusState = "failure"
	StatusError   StatusState = "error"
)

// CommitStatus is the payload for the Status API.
type CommitStatus struct {
	State       StatusState `json:"state"`
	TargetURL   string      `json:"target_url,omitempty"`
	Description string      `json:"description"`
	Context     string      `json:"context"`
}

// CreatedStatus is the API response.
type CreatedStatus struct {
	ID      int64     `json:"id"`
	State   string    `json:"state"`
	Context string    `json:"context"`
	Created time.Time `json:"created_at"`
	URL     string    `json:"url"`
}

// CreateStatus posts a commit status.
//
// This is the primary replay mechanism for Engine B results. Commit statuses
// are writable by user-to-server OAuth tokens with `repo` scope; the Checks
// API (check-runs) is restricted to GitHub App installation tokens, so a
// device-flow token cannot create check runs. See README "Known limits".
//
// Description is capped at 140 characters by GitHub; we truncate rather than
// let the API 422.
func (c *Client) CreateStatus(ctx context.Context, slug, sha string, st CommitStatus) (*CreatedStatus, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	if len(st.Description) > 140 {
		st.Description = st.Description[:137] + "..."
	}
	var out CreatedStatus
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/statuses/%s", owner, name, sha), st, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListStatuses returns existing statuses on a commit, used to make replay
// idempotent: if a status with the same context and state already exists,
// Homeplate does not post a duplicate.
func (c *Client) ListStatuses(ctx context.Context, slug, sha string) ([]CreatedStatus, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var out []CreatedStatus
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s/statuses?per_page=100", owner, name, sha), &out)
	return out, err
}

// CheckRun is the Checks API payload. Homeplate attempts this first when the
// token happens to be a GitHub App installation token, and falls back to
// commit statuses otherwise.
type CheckRun struct {
	Name        string     `json:"name"`
	HeadSHA     string     `json:"head_sha"`
	Status      string     `json:"status"`     // queued|in_progress|completed
	Conclusion  string     `json:"conclusion"` // success|failure|neutral|cancelled|timed_out
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	DetailsURL  string     `json:"details_url,omitempty"`
	ExternalID  string     `json:"external_id,omitempty"`
	Output      *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Text    string `json:"text,omitempty"`
	} `json:"output,omitempty"`
}

// ErrChecksUnavailable indicates the token cannot write check runs.
var ErrChecksUnavailable = fmt.Errorf("checks API unavailable for this token (GitHub App installation token required)")

// CreateCheckRun attempts the Checks API. Returns ErrChecksUnavailable when
// GitHub rejects the token type, so callers can degrade to CreateStatus.
func (c *Client) CreateCheckRun(ctx context.Context, slug string, cr CheckRun) (int64, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/check-runs", owner, name), cr, &out)
	if err != nil {
		var e *Error
		if AsError(err, &e) && (e.StatusCode == http.StatusForbidden || e.StatusCode == http.StatusUnprocessableEntity ||
			strings.Contains(strings.ToLower(e.Message), "integration")) {
			return 0, fmt.Errorf("%w: %v", ErrChecksUnavailable, err)
		}
		return 0, err
	}
	return out.ID, nil
}

// PullRequest is the subset of PR fields Homeplate uses.
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Title  string `json:"title"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *Repo  `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo *Repo  `json:"repo"`
	} `json:"base"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	MergedAt       *time.Time `json:"merged_at"`
}

// IsFork reports whether the PR head lives in a different repository, i.e.
// the "strangers' code on your machine" case for public repos.
func (p PullRequest) IsFork() bool {
	if p.Head.Repo == nil || p.Base.Repo == nil {
		return true // unknown head repo means deleted fork; treat as untrusted
	}
	return !strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName)
}

// PRsForCommit finds open PRs whose head is the given SHA.
func (c *Client) PRsForCommit(ctx context.Context, slug, sha string) ([]PullRequest, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var out []PullRequest
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s/pulls", owner, name, sha), &out)
	return out, err
}

// GetPR fetches a single PR (needed for the fresh `mergeable` field).
func (c *Client) GetPR(ctx context.Context, slug string, number int) (*PullRequest, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var pr PullRequest
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, name, number), &pr)
	return &pr, err
}

// Review is a PR review submission.
type Review struct {
	Event string `json:"event"` // APPROVE|REQUEST_CHANGES|COMMENT
	Body  string `json:"body"`
}

// ListReviews returns existing reviews, used to keep approval idempotent.
func (c *Client) ListReviews(ctx context.Context, slug string, number int) ([]struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Body  string `json:"body"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
}, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var out []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	_, err = c.Get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, name, number), &out)
	return out, err
}

// ApprovePR submits an approving review.
//
// GitHub rejects self-approval ("Can not approve your own pull request"), which
// is the common case for a solo developer. Callers must treat that as a
// non-fatal outcome, not a crash.
func (c *Client) ApprovePR(ctx context.Context, slug string, number int, body string) error {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return err
	}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, name, number),
		Review{Event: "APPROVE", Body: body}, nil)
	return err
}

// MergeRequest is the merge payload.
type MergeRequest struct {
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	SHA           string `json:"sha,omitempty"` // guards against racing new pushes
	MergeMethod   string `json:"merge_method,omitempty"`
}

// MergeResult is the merge response.
type MergeResult struct {
	SHA     string `json:"sha"`
	Merged  bool   `json:"merged"`
	Message string `json:"message"`
}

// MergePR merges a pull request. SHA is required by Homeplate so that a merge
// can never apply to a commit whose local run was not the one that passed.
func (c *Client) MergePR(ctx context.Context, slug string, number int, req MergeRequest) (*MergeResult, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	if req.SHA == "" {
		return nil, fmt.Errorf("refusing to merge %s#%d without pinning the head SHA", slug, number)
	}
	var out MergeResult
	_, err = c.Put(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, name, number), req, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkflowFile is a workflow definition in .github/workflows.
type WorkflowFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

// ListWorkflowFiles lists .github/workflows contents on the default branch.
func (c *Client) ListWorkflowFiles(ctx context.Context, slug, ref string) ([]WorkflowFile, error) {
	owner, name, err := SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/%s/contents/.github/workflows", owner, name)
	if ref != "" {
		path += "?ref=" + url.QueryEscape(ref)
	}
	var out []WorkflowFile
	_, err = c.Get(ctx, path, &out)
	if IsNotFound(err) {
		return nil, nil
	}
	return out, err
}

// ---------- Actions variables ----------

// SetRepoVariable creates an Actions variable, or updates it when it already
// exists (GitHub answers 409 Conflict to a duplicate POST). Used by
// `homeplate adopt --variable` to install the RUNNER_LABEL flip variable.
//
// Requires: classic `repo` scope, or a fine-grained PAT with repository
// "Actions variables: Read and write" (part of Administration on some plans).
func (c *Client) SetRepoVariable(ctx context.Context, slug, name, value string) error {
	owner, repo, err := SplitSlug(slug)
	if err != nil {
		return err
	}
	body := map[string]string{"name": name, "value": value}
	_, err = c.Post(ctx, fmt.Sprintf("/repos/%s/%s/actions/variables", owner, repo), body, nil)
	if err == nil {
		return nil
	}
	var e *Error
	if AsError(err, &e) && e.StatusCode == http.StatusConflict {
		_, err = c.Put(ctx, fmt.Sprintf("/repos/%s/%s/actions/variables/%s", owner, repo, name),
			map[string]string{"name": name, "value": value}, nil)
		return err
	}
	return err
}
