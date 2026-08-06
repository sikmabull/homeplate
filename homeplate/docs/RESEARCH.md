# Homeplate — Research Dossier (facts, endpoints, verdicts)

**Compiled:** 2026-08-06 (UTC-05). All claims below were verified on this date against
`docs.github.com`, `api.github.com`, `raw.githubusercontent.com`, Apple open-source
(`apple-oss-distributions`), local macOS 26.6 (arm64) command output, or a live GitHub device-flow
call. Where evidence is weak or absent, the line is explicitly marked **UNVERIFIED**.

---

## VERDICTS (one line each)

| # | Question | Verdict |
|---|---|---|
| 1 | Can a self-hosted runner claim `ubuntu-latest` / `macos-latest` so existing `runs-on: ubuntu-latest` workflows route to it, with no YAML edit? | **NO — not supported.** No GitHub documentation describes hosted-label claiming; there is no supported mechanism. The label string is *accepted* at registration (no client-side or schema-level rejection), and it historically routed (community reports 2023), but multiple users report it stopped routing around May–June 2025 (community discussion #162274, Jun 9 2025, still unanswered). Treat as unsupported and non-deterministic. **UNVERIFIED (live)**: I have no token to POST a real registration and observe server behavior. |
| 1b | Reserved label `self-hosted` | It is a *default* label auto-added by `config.sh` unless `--no-default-labels`; it is **not** required in `runs-on`, but GitHub recommends listing it first. Actions Runner Controller does **not** support it. |
| 1c | `runs-on` matching semantics | **Runner must have ALL listed labels (AND / superset OK).** A runner with extra labels still matches. |
| 2 | Ephemeral runner flags | `--ephemeral --unattended --replace --disableupdate --no-default-labels --labels --url --token --name --work --runnergroup` all exist and are valid for `config.sh` (verified against runner v2.336.0 `--help` on this Mac and against `Constants.cs`/`CommandSettings.cs`). |
| 3 | Latest actions/runner | **v2.336.0**, published 2026-07-20T17:45:55Z. Runs **natively on macOS arm64** (Mach-O arm64, verified by `file`). |
| 3b | macOS Gatekeeper | Binaries are **ad-hoc signed, NOT notarized** (`spctl -a` → *rejected*). But `curl`-downloaded tarballs get **no `com.apple.quarantine` xattr** (only `com.apple.provenance`), so CLI execution works. Verified locally. |
| 4 | Does device flow require a registered OAuth App client_id? | **YES.** `client_id` is a required parameter and device flow must be enabled in the app's settings; there is no anonymous device flow. (`client_secret` is *not* needed.) |
| 4b | GitHub CLI public client_id | **`178c6fc778ccc68e1d6a`** (hard-coded in `cli/cli`, `internal/authflow/flow.go`, with the comment "This value is safe to be embedded in version control"). **Live-verified working** (see §4). Reusing it in a third-party product is an impersonation/ToS risk — see §4.6. **Recommendation: register your own OAuth App.** |
| 4c | Can a fine-grained PAT create runner registration tokens? | **YES.** Repo-level: fine-grained token needs **"Administration" repository permissions (write)**. Org-level: **"Self-hosted runners" organization permissions (write)**. |
| 6 | Is `POST /repos/{o}/{r}/check-runs` GitHub-App-only? | **YES — definitively.** GitHub's own REST description: *"To create a check run, you must use a GitHub App. OAuth apps and authenticated users are not able to create a check suite."* A user-to-server OAuth/device-flow token **cannot** create check runs. |
| 6b | Fallback for a non-App token | **`POST /repos/{owner}/{repo}/statuses/{sha}`** (Commit Status API). *"Users with push access in a repository can create commit statuses."* Works with classic `repo` / `repo:status` scope or fine-grained "Commit statuses: write". |
| 9 | On an Apple Silicon MacBook, does closing the lid on AC **with a normal sleep assertion held** keep jobs running? | **NO.** Kernel source (`IOPMrootDomain::shouldSleepOnClamshellClosed`) sleeps on lid close unless `desktopMode && acAdaptorConnected` (i.e. **external display + AC**) or a privileged clamshell-disable bit is set. A third-party `PreventUserIdleSystemSleep`/`PreventSystemSleep` assertion does **not** set that bit — the lid-close override (`AppliesOnLidClose`) requires the Apple-private entitlement `com.apple.private.iokit.assertonlidclose`. |
| 9b | Is `sudo pmset -b disablesleep 1` a real workaround? | **YES, it is real and it does block lid-close sleep** — it sets the system-wide `SleepDisabled` property, which sets `userDisabledAllSleep` in the kernel; `privateSleepSystem(kIOPMSleepReasonClamshell)` first calls `checkSystemSleepEnabled()` which returns false. Caveats: undocumented in `man pmset`, requires root, and it is a **system-wide** setting — the `-b`/`-c`/`-a` prefix is *ignored* for it. |
| 9c | Does an external display/keyboard help? | **YES** — Apple's own requirements for closed-display (clamshell) mode: AC power adapter + external display + external keyboard/mouse. That path sets `desktopMode`, which is exactly the kernel condition that disables clamshell sleep. |
| 5 | Multiplier vs. paid rates | The 1× / 2× / 10× multipliers apply **only to included (free) minutes**, never to paid per-minute rates (GitHub's own archived wording: *"Minute multipliers do not apply to the per-minute rates shown below"*). Current paid rates are **not** in a 1:2:10 ratio (Linux $0.006, Windows $0.010, macOS $0.062). The "Minute multipliers" table has been **removed from current docs** (2026); only the flat per-minute table remains. |
| 8 | nektos/act license / embeddability | **MIT**, latest **v0.2.89** (2026-06-01). Importable as a Go library: `github.com/nektos/act/pkg/runner`, `/pkg/model`, `/pkg/common`, `/pkg/container`, `/pkg/artifacts`, `/pkg/artifactcache` — **all under `pkg/`, none under `internal/`** (the repo has *no* `internal/` directory). |

---

## 1. LABEL ROUTING (the critical question)

### 1.1 What `runs-on` matching actually does
Source: <https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idruns-on>
and <https://docs.github.com/en/actions/how-tos/write-workflows/choose-where-workflows-run/choose-the-runner-for-a-job>

Verbatim:

> "If you specify an array of strings or variables, your workflow will execute on any runner that
> matches **all** of the specified `runs-on` values. For example, here the job will only run on a
> self-hosted runner that has the labels `linux`, `x64`, and `gpu`: `runs-on: [self-hosted, linux, x64, gpu]`"

> "These labels operate **cumulatively**, so a self-hosted runner must have all four labels to be
> eligible to process the job."
> — <https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-in-a-workflow>

**Rule:** the runner's label set must be a **superset** of the `runs-on` list. Extra labels on the
runner are fine. Matching is **AND**, never OR. Labels are **case-insensitive**
(<https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/apply-labels>).

`runs-on: [self-hosted, linux, x64]` therefore requires a runner carrying **all three** of
`self-hosted`, `linux`, `x64`. Those three are exactly the default labels a Linux/x64 runner gets.
On this Mac (arm64), `./config.sh --help` prints the defaults as `'self-hosted,OSX,Arm64'` — so
`[self-hosted, linux, x64]` will **not** match a Mac.

### 1.2 The reserved `self-hosted` label
Verbatim from the workflow-syntax reference:

> "Self-hosted runners may have the `self-hosted` label. When setting up a self-hosted runner, by
> default we will include the label `self-hosted`. You may pass in the `--no-default-labels` flag
> to prevent the self-hosted label from being applied. ... we recommend providing an array of labels
> that begins with `self-hosted` (**this must be listed first**) and then includes additional labels
> as needed."
>
> "Note: **Actions Runner Controller does not support the `self-hosted` label.**"

Default labels applied automatically
(<https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-in-a-workflow>):
* `self-hosted`
* OS: `linux`, `windows`, or `macOS`
* Arch: `x64`, `ARM`, or `ARM64`

Runner source confirms this (`src/Runner.Listener/Configuration/ConfigurationManager.cs`):

```csharp
if (!noDefaultLabels) {
    agent.Labels.Add(new AgentLabel("self-hosted", LabelType.System));
    agent.Labels.Add(new AgentLabel(VarUtil.OS, LabelType.System));
    agent.Labels.Add(new AgentLabel(VarUtil.OSArchitecture, LabelType.System));
} else if (userLabels.Count == 0) {
    throw new NotSupportedException("Disabling default labels via --no-default-labels without specifying --labels is not supported");
}
```

### 1.3 Can a self-hosted runner be registered with `ubuntu-latest`?

**Registration side: nothing rejects the string.**
* `config.sh --labels` takes an arbitrary comma-separated list; the runner's `ConfigurationManager`
  does **no** validation against hosted-label names (verified by reading
  `src/Runner.Listener/Configuration/ConfigurationManager.cs` and `CommandSettings.cs` at `main`).
* The REST schemas impose no name restrictions either. `POST /repos/{owner}/{repo}/actions/runners/{runner_id}/labels`
  and `POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig` both declare
  `labels: array<string>, minItems: 1, maxItems: 100` with **no pattern / enum / reserved-word list**
  (source: `github/rest-api-description`, `descriptions/api.github.com/dereferenced/api.github.com.deref.json`).

**Routing side: undocumented, and evidently changed.**
* GitHub documentation **never** describes routing a hosted label to a self-hosted runner. The
  hosted-runner label tables and the self-hosted label docs are separate, and neither mentions
  precedence or overlap.
* Community evidence *for* it working (2023):
  <https://github.com/orgs/community/discussions/20019> — @ChristopherHX, Jan 18 2023:
  > "Did you ever try to assign your self-hosted runner to GitHub hosted runner labels like
  > `ubuntu-latest`, `macos-latest` and `windows-latest`? I observed these priorities if you use
  > `ubuntu-latest` as self-hosted runner label: a self-hosted runner with custom label
  > `ubuntu-latest` is available => send job to your self-hosted runner; no self-hosted runner is
  > connected with custom label `ubuntu-latest` => start your job in a GitHub Hosted Runner."
  (21 upvotes; a later reply "It works! Thank you! =)", Aug 2023.)
* Community evidence *against* it working now (2025):
  <https://github.com/orgs/community/discussions/162274> — "Actions jobs with `ubuntu-latest` label
  don't run on self hosted runners anymore", Jun 9 2025, **Unanswered**:
  > "The `ubuntu-latest` label used to allow prioritise running jobs on self hosted runners, recently
  > (within the last 2/3 weeks) this behaviour has seemingly changed and jobs are no longer being
  > sent to self hosted runners. ... I can force jobs to be picked up on self hosted runners using
  > the `self-hosted` label."
  Confirmed by a second user Jun 24 2025.

**Definitive answer: NO — there is no supported mechanism that lets a self-hosted runner claim
`ubuntu-latest`.** GitHub does not document one, has never documented one, and the previously
observable behaviour regressed in mid-2025 with no acknowledgement. Homeplate must not depend on it.

*Honest caveat (UNVERIFIED):* I could not perform a live registration test (no account/token in this
environment), so I cannot state whether the API now returns a 4xx for the label, silently strips it,
or accepts it but never routes. The most likely behaviour, given the schema has no restriction and
users report the runner shows the label but never gets jobs, is **accept-but-don't-route**.

### 1.4 Do runner GROUPS or repo-level registration change this?
No. Groups add an **additional AND condition**, they never relax label matching:

> "When you combine groups and labels, the runner must meet **both** requirements to be eligible to
> run the job." — workflow-syntax reference

```yaml
runs-on:
  group: ubuntu-runners
  labels: ubuntu-24.04-16core
```
Runner groups "can only have larger runners or self-hosted runners as members"
(<https://docs.github.com/en/actions/concepts/runners/runner-groups>). Registering the runner at
repo level vs. org level changes *visibility*, not label semantics.

Closest thing to an official lever (and it does **not** give you hosted-label claiming):
2026-06-25 changelog "More control over your GitHub-hosted runners"
(<https://github.blog/changelog/2026-06-25-more-control-over-your-github-hosted-runners/>):
> "Admins can now disable the standard labels for hosted runners such as `ubuntu-latest`, as well as
> add macOS runners to runner groups."
Org setting: Settings → Actions → General → "Standard hosted runners" → **Disable for all repositories**.
This "requires workflows to target runners through **runner groups**"
(<https://docs.github.com/en/organizations/managing-organization-settings/disabling-or-limiting-github-actions-for-your-organization>).
It is Team/Enterprise only, and it forces YAML changes (`runs-on: {group: ...}`) — the opposite of
what Homeplate wants. **UNVERIFIED:** whether, with standard hosted runners disabled, a self-hosted
runner labelled `ubuntu-latest` then picks up `runs-on: ubuntu-latest`. Docs do not say.

### 1.5 Routing precedence & timeouts (documented)
Source: <https://docs.github.com/en/actions/reference/runners/self-hosted-runners>
* Online + idle matching runner → job assigned and sent.
* Runner doesn't pick up an assigned job within **60 seconds** → job re-queued for another runner.
* No matching online/idle runner → job stays queued.
* Queued **> 24 hours** → job fails.

### 1.6 Practical implication for Homeplate
The only reliable, supported routing keys are `self-hosted` + OS/arch defaults + custom labels, all
of which require the workflow YAML to name them. Realistic options:
1. Edit workflows (one-line `runs-on:` change), possibly via a Homeplate-opened PR.
2. `runs-on: ${{ vars.RUNNER_LABEL }}` or a `choose-runner` job that emits the label as an output
   (pattern documented in community discussion #20019) — still a YAML edit, but a one-time one.
3. Run the job **locally** with `nektos/act` and replay the result as a **commit status** (§6, §8) —
   no YAML edit at all, but it is not a real GitHub Actions run.

---

## 2. EPHEMERAL RUNNERS

### 2.1 `config.sh` flags — verified against runner v2.336.0 on macOS arm64
Actual `./config.sh --help` output (run locally on the downloaded osx-arm64 tarball):

```
Commands:
 ./config.sh         Configures the runner
 ./config.sh remove  Unconfigures the runner
 ./run.sh            Runs the runner interactively. Does not require any options.

Options:
 --help     Prints the help for each command
 --version  Prints the runner version
 --commit   Prints the runner commit
 --check    Check the runner's network connectivity with GitHub server

Config Options:
 --unattended           Disable interactive prompts for missing arguments. Defaults will be used for missing options
 --url string           Repository to add the runner to. Required if unattended
 --token string         Registration token. Required if unattended
 --name string          Name of the runner to configure (default <hostname>)
 --runnergroup string   Name of the runner group to add this runner to (defaults to the default runner group)
 --labels string        Custom labels that will be added to the runner. This option is mandatory if --no-default-labels is used.
 --no-default-labels    Disables adding the default labels: 'self-hosted,OSX,Arm64'
 --local                Removes the runner config files from your local machine. Used as an option to the remove command
 --work string          Relative runner work directory (default _work)
 --replace              Replace any existing runner with the same name (default false)
 --pat                  GitHub personal access token with repo scope. Used for checking network connectivity when executing `./run.sh --check`
 --disableupdate        Disable self-hosted runner automatic update to the latest released version
 --ephemeral            Configure the runner to only take one job and then let the service un-configure the runner after the job finishes (default false)
```

Additional flags/args present in the source but not in `--help`
(`src/Runner.Common/Constants.cs`, `src/Runner.Listener/CommandSettings.cs` @ `main`):
* configure: `--auth`, `--monitorsocketaddress`, `--runasservice`, `--generateServiceConfig`,
  `--windowslogonaccount`, `--windowslogonpassword`, `--username`
* remove: `--token`, `--pat`, `--local`
* run: `--once` (deprecated in favour of `--ephemeral`), `--jitconfig`, `--startuptype`
* misc: `--check`, `--commit`, `--version`, `--help`

Canonical ephemeral invocation for Homeplate:
```bash
./config.sh --unattended --replace --ephemeral --disableupdate \
  --url https://github.com/OWNER/REPO \
  --token "$REG_TOKEN" \
  --name "homeplate-$(hostname)-$(uuidgen)" \
  --labels "homeplate,macos,arm64" \
  --work _work
./run.sh          # exits after exactly one job; runner self-unregisters
```

Doc note on ephemeral runners
(<https://docs.github.com/en/actions/reference/runners/self-hosted-runners>):
> "To add an ephemeral runner to your environment, include the `--ephemeral` parameter when
> registering your runner"
> Warning: "The runner application log files for ephemeral runners must be forwarded to an external
> log storage solution for troubleshooting and diagnostic purposes."

### 2.2 JIT (just-in-time) alternative — avoids storing a registration token on disk
```
POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig
POST /orgs/{org}/actions/runners/generate-jitconfig
```
Body: `{"name": str (req), "runner_group_id": int (req), "labels": [str] (req, 1..100), "work_folder": str = "_work"}`
Response contains an `encoded_jit_config`, used as `./run.sh --jitconfig <base64>`. JIT runners are
always ephemeral. Scopes: classic `repo` (repo-level) / `admin:org` (org-level).

### 2.3 Exact REST endpoints, methods, scopes
All paths verified against the OpenAPI description (`github/rest-api-description`, `api.github.com.deref.json`)
and <https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28>.

| Operation | Method + path | Classic PAT / OAuth scope | Fine-grained permission |
|---|---|---|---|
| Repo registration token | `POST /repos/{owner}/{repo}/actions/runners/registration-token` | `repo` | **Administration** (repository) **write** |
| Org registration token | `POST /orgs/{org}/actions/runners/registration-token` | `admin:org` (+ `repo` if private) | **Self-hosted runners** (organization) **write** |
| Repo remove token | `POST /repos/{owner}/{repo}/actions/runners/remove-token` | `repo` | **Administration** (repository) **write** |
| Org remove token | `POST /orgs/{org}/actions/runners/remove-token` | `admin:org` (+ `repo` if private) | **Self-hosted runners** (organization) **write** |
| List repo runners | `GET /repos/{owner}/{repo}/actions/runners` | `repo` | **Administration** (repository) **read** |
| List org runners | `GET /orgs/{org}/actions/runners` | `admin:org` (+ `repo` if private) | **Self-hosted runners** (organization) **read** |
| Get one runner | `GET /repos/{owner}/{repo}/actions/runners/{runner_id}` · `GET /orgs/{org}/actions/runners/{runner_id}` | as above | as above |
| Delete repo runner | `DELETE /repos/{owner}/{repo}/actions/runners/{runner_id}` | `repo` | **Administration** (repository) **write** |
| Delete org runner | `DELETE /orgs/{org}/actions/runners/{runner_id}` | `admin:org` (+ `repo` if private) | **Self-hosted runners** (organization) **write** |
| List/add/set/remove labels | `GET|POST|PUT|DELETE /repos/{owner}/{repo}/actions/runners/{runner_id}/labels` and `.../labels/{name}` (DELETE) | `repo` / `admin:org` | Administration (repo) / Self-hosted runners (org) |
| JIT config | `POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig` · `POST /orgs/{org}/actions/runners/generate-jitconfig` | `repo` / `admin:org` | Administration (repo) / Self-hosted runners (org) |
| List runner downloads | `GET /repos/{owner}/{repo}/actions/runners/downloads` · `GET /orgs/{org}/actions/runners/downloads` | `repo` / `admin:org` | as above |

Facts:
* Registration and remove tokens **expire after one hour** (documented for all four endpoints).
* Authenticated user must have **admin access** to the repo/org for every one of these.
* Registration-token response is `201 Created` with `{"token": "...", "expires_at": "..."}`.
* All of these endpoints are `enabledForGitHubApps: true`.

---

## 3. RUNNER BINARY

### 3.1 Latest release
`GET https://api.github.com/repos/actions/runner/releases/latest` (fetched 2026-08-06):

* `tag_name`: **`v2.336.0`**, published **2026-07-20T17:45:55Z**
* Previous: v2.335.1 (2026-06-09), v2.335.0 (2026-06-08), v2.334.0 (2026-04-21), v2.333.1, v2.333.0, v2.332.0, v2.331.0

### 3.2 Exact download URL pattern
```
https://github.com/actions/runner/releases/download/v{VER}/actions-runner-{PLATFORM}-{VER}.{EXT}
```
where `{VER}` has **no** leading `v` in the filename but **does** in the tag path. Real v2.336.0 assets
(byte sizes as returned by the API):

| Platform | URL | Size |
|---|---|---|
| macOS arm64 | `https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-osx-arm64-2.336.0.tar.gz` | 127,389,671 |
| macOS x64 | `https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-osx-x64-2.336.0.tar.gz` | 131,517,013 |
| Linux x64 | `https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-x64-2.336.0.tar.gz` | 226,035,903 |
| Linux arm64 | `https://github.com/actions/runner/releases/download/v2.336.0/actions-runner-linux-arm64-2.336.0.tar.gz` | 138,824,064 |
| Linux arm (32) | `.../actions-runner-linux-arm-2.336.0.tar.gz` | 77,278,514 |
| Windows x64 | `.../actions-runner-win-x64-2.336.0.zip` | 103,253,740 |
| Windows arm64 | `.../actions-runner-win-arm64-2.336.0.zip` | 94,445,234 |

Platform tokens: `osx-arm64`, `osx-x64`, `linux-x64`, `linux-arm64`, `linux-arm`, `win-x64`, `win-arm64`.
Extension: `.tar.gz` for osx/linux, `.zip` for win.

### 3.3 Getting the latest version programmatically
Three options, in order of preference:
1. **Unauthenticated, no scopes:** `GET https://api.github.com/repos/actions/runner/releases/latest`
   → `.tag_name` (strip leading `v`) and `.assets[].browser_download_url`. Rate limit 60/hr/IP unauthenticated.
2. **Authenticated, repo-scoped:** `GET /repos/{owner}/{repo}/actions/runners/downloads`
   (or `GET /orgs/{org}/actions/runners/downloads`) → array of
   `{os, architecture, download_url, filename, temp_download_token?, sha256_checksum?}`. This is the
   endpoint GitHub's own setup UI uses, and it returns the version GitHub currently wants you to use.
   Note the values are `os: "osx"|"linux"|"win"`, `architecture: "x64"|"arm64"|"arm"`.
3. `GET https://api.github.com/repos/actions/runner/releases?per_page=1`.

### 3.4 macOS arm64: native? notarized?
Verified locally on macOS 26.6 (build 25G72), Apple Silicon, by downloading and unpacking the real
osx-arm64 tarball:

```
$ file bin/Runner.Listener
bin/Runner.Listener: Mach-O 64-bit executable arm64          # native, not Rosetta

$ ./config.sh --version
2.336.0                                                       # runs natively

$ codesign -dv bin/Runner.Listener
Identifier=apphost-55554944ca62811866fe36f79784b1f8746a59d8
Format=Mach-O thin (arm64)
CodeDirectory v=20400 size=425 flags=0x2(adhoc) hashes=7+2 location=embedded
Signature=adhoc                                               # AD-HOC signed, no Developer ID

$ spctl -a -vv bin/Runner.Listener
bin/Runner.Listener: rejected                                 # NOT notarized

$ curl -sL -o r.tar.gz https://github.com/.../actions-runner-osx-arm64-2.336.0.tar.gz
$ xattr -l r.tar.gz
com.apple.provenance:                                         # NO com.apple.quarantine
```

**Interpretation (important for Homeplate):**
* The runner is a **native arm64** Mach-O; no Rosetta needed. macOS 11.0 (Big Sur) or later is the
  documented minimum; ARM64 macOS is listed as **public preview**
  (<https://docs.github.com/en/actions/reference/runners/self-hosted-runners> — "ARM64 - Linux, macOS, Windows (currently in public preview)").
* Binaries are **ad-hoc signed and not notarized**, so `spctl` rejects them. This matters **only if a
  quarantine xattr is present**. `curl`/`wget`/Go's `net/http` do **not** set `com.apple.quarantine`;
  Safari/Chrome/Mail do. If Homeplate downloads the tarball itself over HTTP, Gatekeeper will not
  block it.
* Defensive measure if you ever ship or relocate the tree:
  `xattr -dr com.apple.quarantine <runner-dir>` (and, if the user dragged it from a browser download,
  that is exactly the fix for "cannot be opened because the developer cannot be verified").
* Known live bug to be aware of: multiple open issues report **v2.336.0 deadlocks on macOS arm64**
  (`Runner.Worker` hangs in `Process.Start`, 100% CPU, every job wedges); rolling back to **v2.335.1**
  fixes it — <https://github.com/actions/runner/issues/4570>,
  <https://github.com/actions/runner/issues/4575>, mitigation PR
  <https://github.com/actions/runner/pull/4572>. **Homeplate should pin/prefer v2.335.1 on macOS arm64
  until that is closed**, and should always pass `--disableupdate` so an auto-update can't reintroduce it.

---

## 4. DEVICE FLOW

Primary source: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps>
(section "Device flow"). Also <https://docs.github.com/en/apps/creating-github-apps/writing-code-for-a-github-app/building-a-cli-with-a-github-app>.

### 4.1 Endpoints and parameters

**Step 1 — request codes**
```
POST https://github.com/login/device/code
Accept: application/json
Content-Type: application/x-www-form-urlencoded

client_id=<CLIENT_ID>&scope=<space-delimited scopes>
```
Response (JSON form):
```json
{"device_code":"…40 chars…","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}
```
* `user_code` is 8 characters with a hyphen in the middle.
* `expires_in` default **900 s (15 min)**.
* `interval` = minimum seconds between polls (default 5).

**Step 2 — user enters the code at `https://github.com/login/device`.**

**Step 3 — poll for the token**
```
POST https://github.com/login/oauth/access_token
Accept: application/json

client_id=<CLIENT_ID>&device_code=<DEVICE_CODE>&grant_type=urn:ietf:params:oauth:grant-type:device_code
```
* `grant_type` must be **exactly** `urn:ietf:params:oauth:grant-type:device_code`.
* **No `client_secret` is used or needed for device flow.** (Doc: `incorrect_client_credentials` —
  "For the device flow, you must pass your app's client ID … The `client_secret` is not needed for the device flow.")
* Success: `{"access_token":"gho_…","token_type":"bearer","scope":"repo,gist"}`.
* `Accept: application/json` or `application/xml`; the default is form-encoded.

### 4.2 Polling semantics / error codes (verbatim from docs)
| Error | Meaning |
|---|---|
| `authorization_pending` | User hasn't entered the code yet. Keep polling, do not exceed `interval`. |
| `slow_down` | You polled too fast. **Add 5 seconds** to the interval; the error response includes the new `interval` you must use. |
| `expired_token` | Device code expired (>15 min) — request a new device code. |
| `unsupported_grant_type` | `grant_type` missing/wrong. |
| `incorrect_client_credentials` | Wrong/missing client ID (client_secret must NOT be sent). |
| `incorrect_device_code` | Bad device_code. |
| `access_denied` | User clicked Cancel; the code cannot be reused. |
| `device_flow_disabled` | Device flow is not enabled in the app's settings. |

Rate limit: **50 verification-code submissions per hour per application** (browser side).

### 4.3 LIVE VERIFICATION (performed 2026-08-06)
Using the GitHub CLI's public client_id, no client secret:
```
POST https://github.com/login/device/code   client_id=178c6fc778ccc68e1d6a  scope="repo read:org"
→ 200 {"device_code":"0c3dc513…","user_code":"78FA-2E45","verification_uri":"https://github.com/login/device","expires_in":899,"interval":5}

POST https://github.com/login/oauth/access_token  (grant_type=urn:ietf:params:oauth:grant-type:device_code)
→ 200 {"error":"authorization_pending","error_description":"The authorization request is still pending.", "error_uri":"https://docs.github.com/developers/apps/authorizing-oauth-apps#error-codes-for-the-device-flow"}

(immediately again)
→ 200 {"error":"slow_down","error_description":"Too many requests have been made in the same timeframe.","error_uri":"https://docs.github.com","interval":10}
```
**Implementation notes proven by this test:** GitHub returns **HTTP 200 even for errors** — you must
parse the body, not the status code. And `slow_down` really does return a new `interval` (5 → 10).

### 4.4 Does device flow require a registered OAuth App client_id? — YES
`client_id` is marked **Required** for both endpoints, and:
> "Before you can use the device flow to authorize and identify users, you must first enable it in
> your app's settings."
Both OAuth Apps and GitHub Apps support device flow; there is no anonymous/unregistered variant.

### 4.5 GitHub CLI's well-known public client_id
`cli/cli`, `internal/authflow/flow.go` (trunk, verified 2026-08-06):
```go
var (
    // The "GitHub CLI" OAuth app
    oauthClientID = "178c6fc778ccc68e1d6a"
    // This value is safe to be embedded in version control
    oauthClientSecret = "34ddeff2b558a23d38fba8a6de74f086ede1cc0b"
)
...
minimumScopes := []string{"repo", "read:org", "gist"}
```
`pkg/cmd/auth/shared/oauth_scopes.go` checks a token for `repo`, `read:org`, and treats `admin:org`
as satisfying `read:org`.

### 4.6 ToS consideration of reusing gh's client_id — DON'T
There is no clause that names "client_id reuse" specifically, but two policies bear directly on it:
* **GitHub Impersonation policy** (<https://docs.github.com/en/site-policy/acceptable-use-policies/github-impersonation>):
  "You may not misrepresent your identity or your association with another person or organization…
  Using a deceptively similar username, organization name, or **other namespace** … Otherwise posing
  as another individual or organization." Reusing gh's client_id makes GitHub display **"GitHub CLI"**
  on the authorization screen and in the user's authorized-apps list for *your* product's access —
  that is a misrepresentation to the end user, and it makes your traffic indistinguishable from gh's.
* **ToS §H (API Terms)** (<https://docs.github.com/en/site-policy/github-terms/github-terms-of-service>,
  effective 2026-04-27): "Abuse or excessively frequent requests to GitHub via the API may result in
  temporary or permanent suspension of your Account's access to the API… **You may not share API
  tokens to exceed GitHub's rate limitations.**"

**Recommendation:** register a first-party "Homeplate" OAuth App (or GitHub App) and enable device
flow on it. Cost: zero. The client_id is public by design; no secret ships.

### 4.7 Which scopes for which capability
Classic OAuth / PAT-classic scope list: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps>

| Capability | Classic scope(s) | Fine-grained permission | Endpoint(s) |
|---|---|---|---|
| (a) List orgs the user belongs to | `read:org` (or `user`) | — (fine-grained PATs are scoped to one owner) | `GET /user/orgs` — doc: "requires at least `user` or `read:org` scope" |
| (a) List repos the user can admin | `repo` (private) / `public_repo` | Metadata: read | `GET /user/repos?affiliation=owner,organization_member` then filter `permissions.admin == true` |
| (b) Create runner registration token (repo) | `repo` | **Administration: read & write** (repository) | `POST /repos/{o}/{r}/actions/runners/registration-token` |
| (b) Create runner registration token (org) | `admin:org` (+`repo` if private) | **Self-hosted runners: read & write** (organization) | `POST /orgs/{org}/actions/runners/registration-token` |
| (c) Create commit statuses | `repo` **or** `repo:status` | **Commit statuses: read & write** | `POST /repos/{o}/{r}/statuses/{sha}` |
| (c) Create check runs | *not possible with a user token* — GitHub App only | Checks: write (App/fine-grained) | `POST /repos/{o}/{r}/check-runs` |
| (d) Approve a PR review | `repo` (`public_repo` for public) | Pull requests: read & write | `POST /repos/{o}/{r}/pulls/{n}/reviews` with `event: "APPROVE"` |
| (d) Merge a PR | `repo` | **Contents: read & write** | `PUT /repos/{o}/{r}/pulls/{n}/merge` |
| Edit workflow files | `workflow` (in addition to `repo`) | Workflows: write | needed if Homeplate ever commits `.github/workflows/*` |

Minimal practical Homeplate scope set (classic/device flow): **`repo read:org workflow`**, plus
`admin:org` only if you support org-level runners.

### 4.8 Fine-grained PATs and registration tokens — YES, they work
Verbatim from <https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28>:
> "Fine-grained access tokens for 'Create a registration token for a repository'. This endpoint works
> with the following fine-grained token types: GitHub App user access tokens, GitHub App installation
> access tokens, Fine-grained personal access tokens. The fine-grained token must have the following
> permission set: **'Administration' repository permissions (write)**."

Org variant requires **'Self-hosted runners' organization permissions (write)**. (Note the asymmetry:
the *repo* endpoint uses "Administration", the *org* endpoint uses "Self-hosted runners".)

---

## 5. PRICING (for the "$ saved" counter)

Primary source: **"Actions runner pricing"** — <https://docs.github.com/en/billing/reference/actions-minute-multipliers>
(fetched **2026-08-06**). Secondary: **"GitHub Actions billing"** —
<https://docs.github.com/en/billing/concepts/product-billing/github-actions> (same date).

> "GitHub rounds the minutes and partial minutes each job uses **up to the nearest whole minute**."

### 5.1 Standard runners — current per-minute USD rates (2026-08-06)
| Operating system | Billing SKU | Per-minute rate (USD) |
|---|---|---|
| Linux 1-core (x64) | `actions_linux_slim` | **$0.002** |
| **Linux 2-core (x64)** | `actions_linux` | **$0.006** |
| Linux 2-core (arm64) | `actions_linux_arm` | **$0.005** |
| **Windows 2-core (x64)** | `actions_windows` | **$0.010** |
| Windows 2-core (arm64) | `actions_windows_arm` | **$0.010** |
| **macOS 3-core or 4-core (M1 or Intel)** | `actions_macos` | **$0.062** |

### 5.2 x64 larger runners
| OS | SKU | $/min |
|---|---|---|
| Linux Advanced 2-core | `linux_2_core_advanced` | 0.006 |
| Linux 4 / 8 / 16 / 32 / 64 / 96-core | `linux_{n}_core` | 0.012 / 0.022 / 0.042 / 0.082 / 0.162 / 0.252 |
| Windows 4 / 8 / 16 / 32 / 64 / 96-core | `windows_{n}_core` | 0.022 / 0.042 / 0.082 / 0.162 / 0.322 / 0.552 |
| macOS 12-core | `macos_l` | 0.077 |

### 5.3 arm64 larger runners
| OS | SKU | $/min |
|---|---|---|
| Linux 2 / 4 / 8 / 16 / 32 / 64-core | `linux_{n}_core_arm` | 0.005 / 0.008 / 0.014 / 0.026 / 0.050 / 0.098 |
| Windows 2 / 4 / 8 / 16 / 32 / 64-core | `windows_{n}_core_arm` | 0.008 / 0.014 / 0.026 / 0.050 / 0.098 / 0.194 |
| macOS 5-core (M2 Pro) | `macos_xl` | 0.102 |

### 5.4 GPU larger runners
| OS | SKU | $/min |
|---|---|---|
| Linux 4-core | `linux_4_core_gpu` | 0.052 |
| Windows 4-core | `windows_4_core_gpu` | 0.102 |

### 5.5 Multipliers — the exact rule
* **Included/free minutes** are consumed at 1× (Linux) / 2× (Windows) / 10× (macOS).
  Archived GitHub wording (verified via Wayback snapshot 2024-02-29 of
  `docs.github.com/en/billing/managing-billing-for-github-actions/about-billing-for-github-actions`):
  > "Jobs that run on Windows and macOS runners that GitHub hosts consume minutes at **2 and 10 times**
  > the rate that jobs on Linux runners consume. For example, using 1,000 Windows minutes would consume
  > 2,000 of the minutes included in your account. Using 1,000 macOS minutes would consume 10,000
  > minutes included in your account."
  > *Minute multipliers: Linux 1, Windows 2, macOS 10.*
* **Paid per-minute rates are NOT multiplied.** Same archived page, verbatim:
  > "**Note: Minute multipliers do not apply to the per-minute rates shown below.**"
* Current docs (2026) confirm multipliers still exist conceptually — "GitHub Actions usage metrics do
  not apply minute multipliers to the metrics displayed … For more information about minute
  multipliers, see GitHub Actions billing"
  (<https://docs.github.com/en/actions/concepts/billing-and-usage>) — but the multiplier **table has
  been removed** from the billing pages; only the flat per-minute table remains.
* Sanity check on today's numbers: 0.006 : 0.010 : 0.062 ≈ **1 : 1.67 : 10.3**, i.e. the paid rates
  are *not* a clean 1:2:10.

### 5.6 Included free minutes per plan (2026-08-06)
| Plan | Artifact storage | Minutes/month | Cache/repo |
|---|---|---|---|
| GitHub Free | 500 MB | 2,000 | 10 GB |
| GitHub Pro | 1 GB | 3,000 | 10 GB |
| Free for organizations | 500 MB | 2,000 | 10 GB |
| GitHub Team | 2 GB | 3,000 | 10 GB |
| GitHub Enterprise Cloud | 50 GB | 50,000 | 10 GB |

Other facts for the counter:
* **"GitHub Actions usage is free for self-hosted runners and for public repositories that use
  standard GitHub-hosted runners."** → a "$ saved" counter is only honest for **private** repos
  (or for larger runners, which are billed even on public repos).
* Storage overage: shared (artifacts + Packages) **$0.25/GB-month**, Actions cache **$0.07/GB-month**,
  custom images **$0.07/GB-month**.
* GitHub's own worked example: "3,000 Linux minutes at $0.006 = $18; 2,000 Windows minutes at $0.010 = $20; total $38."

### 5.7 Recommended counter formula for Homeplate
```
saved_usd = ceil(job_seconds / 60) * rate[os_class]
rate = {linux_2core: 0.006, windows_2core: 0.010, macos_3or4core: 0.062}
# only count private repositories; hard-code the date the table was verified and show it in the UI
```

---

## 6. CHECKS / STATUS REPLAY

### 6.1 Checks API vs Commit Status API

| | Checks API | Commit Status API |
|---|---|---|
| Create | `POST /repos/{owner}/{repo}/check-runs` | `POST /repos/{owner}/{repo}/statuses/{sha}` |
| Update | `PATCH /repos/{owner}/{repo}/check-runs/{check_run_id}` | (immutable; post a new status with the same `context`) |
| Read | `GET /repos/{o}/{r}/commits/{ref}/check-runs`, `GET /repos/{o}/{r}/check-runs/{id}` | `GET /repos/{o}/{r}/commits/{ref}/statuses`, `GET /repos/{o}/{r}/commits/{ref}/status` |
| **Who may write** | **GitHub Apps only** | Any user/token with **push access** |
| Fine-grained perm | Checks: write | Commit statuses: write |
| Classic scope | n/a (App) | `repo` or `repo:status` |
| Rich output | title, summary, text (markdown), annotations, images, `details_url`, actions | `state`, `target_url`, `description` (short), `context` |
| Limits | 1,000 check runs with the same name per check suite (older auto-deleted) | 1,000 statuses per `(sha, context)` |

### 6.2 Is check-runs App-only? — YES, definitively
Verbatim from the REST description (both docs.github.com and the machine-readable OpenAPI
`api.github.com.deref.json`, `POST /repos/{owner}/{repo}/check-runs`, `.description`):

> **"To create a check run, you must use a GitHub App. OAuth apps and authenticated users are not
> able to create a check suite."**

Source: <https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28>.
So a **device-flow user-to-server OAuth token cannot create check runs**. (A *GitHub App* user access
token obtained via device flow also cannot — the writer identity must be the App installation.)

### 6.3 Fallback for a non-App token
`POST /repos/{owner}/{repo}/statuses/{sha}`:
> "Users with push access in a repository can create commit statuses for a given SHA."

Body schema (from OpenAPI):
```json
{
  "state": "error|failure|pending|success",   // required
  "target_url": "https://…",                  // deep link (Homeplate: local log viewer / gist)
  "description": "short text",
  "context": "homeplate/build"                  // default "default"; case-insensitive
}
```
Commit statuses show in the PR "checks" box and can be required by branch protection, so this is a
fully workable replay channel for Homeplate without becoming a GitHub App.

### 6.4 Can logs/artifacts attach to a check without a workflow run?
* **Checks API (App only):** yes in spirit — `output.text` accepts markdown, `output.annotations`
  (max 50 per request, file/line scoped), `output.images`, and `details_url`. There is **no** binary
  artifact upload on a check run.
* **Status API:** **no** log/artifact attachment at all. Only `target_url` + a short `description`.
  Homeplate must host the log itself (local HTTP viewer, a gist via the `gist` scope, a release asset,
  or a repo comment) and point `target_url` at it.
* The Actions **artifact** and **logs** APIs (`/repos/{o}/{r}/actions/runs/{run_id}/artifacts`,
  `/logs`) are tied to a real `workflow_run`; you **cannot** synthesise a workflow run via the API.
  So "logs attached to a check with no workflow run" = **NO**, in both APIs.

### 6.5 Approve a PR review / merge a PR
```
POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews
  {"commit_id": "<sha>", "body": "…", "event": "APPROVE" | "REQUEST_CHANGES" | "COMMENT", "comments": [...]}
  # omit `event` to create a PENDING review; submit later with
  # POST /repos/{o}/{r}/pulls/{n}/reviews/{review_id}/events
  # scope: repo | fine-grained: Pull requests: write
  # NOTE: you cannot approve your own PR.

PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge
  {"commit_title": "...", "commit_message": "...", "sha": "<head sha guard>", "merge_method": "merge|squash|rebase"}
  # scope: repo | fine-grained: Contents: write
  # 405 if not mergeable, 409 if head sha mismatch. Triggers notifications; subject to secondary rate limits.
```
Sources: <https://docs.github.com/en/rest/pulls/reviews?apiVersion=2022-11-28>,
<https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28>.

---

## 7. GITHUB STATUS (Statuspage v2 API)

Base: `https://www.githubstatus.com/api/v2/`. All endpoints are public JSON, no auth, CORS-friendly.

| Endpoint | Purpose | Size (2026-08-06) |
|---|---|---|
| `GET /api/v2/status.json` | overall indicator only (cheapest poll) | 214 B |
| `GET /api/v2/components.json` | all components + per-component status | 4.2 KB |
| `GET /api/v2/summary.json` | page + components + incidents + maintenances | 19.6 KB |
| `GET /api/v2/incidents/unresolved.json` | open incidents only | 15.4 KB |
| `GET /api/v2/incidents.json` | full incident history (large) | 242 KB |
| `GET /api/v2/scheduled-maintenances/upcoming.json` | upcoming maintenance | 173 B |
| `GET /api/v2/scheduled-maintenances/active.json` | active maintenance | 173 B |

**Actions component identity (stable):** `name` = **`"Actions"`**, `id` = **`"br0l2tvcx85d"`**,
`description` = "Workflows, Compute and Orchestration for GitHub Actions", `position` 7,
`page_id` `kctbh9vrtdwd`. Match on the **id**, not the name.

Full component id list captured 2026-08-06:
`Git Operations 8l4ygp009s5s` · `Webhooks 4230lsnqdsld` · `API Requests brv1bkgrwx7q` ·
`Issues kr09ddfgbfsf` · `Pull Requests hhtssxt0f5v2` · **`Actions br0l2tvcx85d`** ·
`Packages st3j38cctv9l` · `Pages vg70hn9s2tyj` · `Copilot pjmpxvq2cmr2` · `Codespaces h2ftsgbw7kmk` ·
`Copilot AI Model Providers cnnb39dkkk82`

Value domains: `status.indicator` ∈ `none|minor|major|critical`;
`component.status` ∈ `operational|degraded_performance|partial_outage|major_outage|under_maintenance`.

**Real sample fetched 2026-08-06T22:18Z** (GitHub Actions happened to be in a major outage — useful
real-world shape):

```json
// GET https://www.githubstatus.com/api/v2/status.json
{
  "page": {
    "id": "kctbh9vrtdwd",
    "name": "GitHub",
    "url": "https://www.githubstatus.com",
    "time_zone": "Etc/UTC",
    "updated_at": "2026-08-06T22:18:09.811Z"
  },
  "status": { "indicator": "major", "description": "Partial System Outage" }
}
```

```json
// GET https://www.githubstatus.com/api/v2/components.json  -> .components[] where name == "Actions"
{
  "id": "br0l2tvcx85d",
  "name": "Actions",
  "status": "major_outage",
  "created_at": "2019-11-13T18:02:19.432Z",
  "updated_at": "2026-08-06T16:33:31.366Z",
  "position": 7,
  "description": "Workflows, Compute and Orchestration for GitHub Actions",
  "showcase": true,
  "start_date": null,
  "group_id": null,
  "page_id": "kctbh9vrtdwd",
  "group": false,
  "only_show_if_degraded": false
}
```

```json
// GET https://www.githubstatus.com/api/v2/summary.json -> .incidents[0] (truncated)
{
  "id": "qcvjkzcs7j74",
  "name": "Incident with Actions",
  "status": "investigating",
  "created_at": "2026-08-06T15:22:49.029Z",
  "updated_at": "2026-08-06T22:18:09.801Z",
  "impact": "critical",
  "shortlink": "https://stspg.io/rcz3fcm83sff",
  "started_at": "2026-08-06T15:22:49.021Z",
  "page_id": "kctbh9vrtdwd",
  "incident_updates": [
    { "id": "dwhlj0kqvv5p", "status": "investigating",
      "body": "We continue to make progress on the issue affecting GitHub Actions. … A change is also in progress to mitigate issues with existing self-hosted runners that are not picking up jobs. …",
      "created_at": "2026-08-06T22:18:0…" }
  ]
}
```
(Note the incident text itself mentions "existing self-hosted runners that are not picking up jobs" —
a good argument for Homeplate surfacing this endpoint in its UI.)

---

## 8. nektos/act

Verified 2026-08-06 via `api.github.com/repos/nektos/act`, the repo tree at `master`, and
<https://nektosact.com>.

* **License: MIT** (`{"key":"mit","spdx_id":"MIT"}`); `LICENSE` at repo root.
* **Latest release: `v0.2.89`**, published **2026-06-01T03:22:22Z**. ~71.4k stars. Default branch `master`.
* Go module: `module github.com/nektos/act`, `go 1.25.0`.

### 8.1 macOS install
* Homebrew (documented on <https://nektosact.com/installation/index.html>): `brew install act`
* GitHub CLI extension: `gh extension install https://github.com/nektos/gh-act`
* MacPorts, Nix, or the official script:
  `curl --proto '=https' --tlsv1.2 -sSf https://raw.githubusercontent.com/nektos/act/master/install.sh | sudo bash`
* Prebuilt tarballs on the releases page (macOS arm64 + Intel).
* Build from source: `make build` (Go 1.18+ documented; go.mod now says 1.25).
* **Requires Docker Engine API.** "act is currently not supported with podman or other container
  backends (it might work, but it's not guaranteed)" (issue #303).

### 8.2 Key CLI flags (from `cmd/root.go` @ master)
| Flag | Meaning |
|---|---|
| `act <event>` | positional event name; default `push` (or the workflow's only event) |
| `-j, --job <id>` | run a specific job ID |
| `-W, --workflows <path>` | workflow file or directory (default `./.github/workflows/`) |
| `-e, --eventpath <file>` | **path to event JSON payload file** |
| `-l, --list` / `-g, --graph` | list / draw workflows |
| `-P, --platform <label>=<image>` | map a `runs-on` label to a docker image |
| `--matrix k:v` | include only a matrix configuration |
| `-s, --secret k=v`, `--secret-file .secrets` | secrets |
| `--var-file .vars`, `--env`, `--env-file .env`, `--input`, `--input-file` | vars/env/inputs |
| `-n, --dryrun` | validate only, no containers |
| `--validate` / `--strict` | workflow schema validation |
| `-b, --bind`, `-r, --reuse`, `-p, --pull`, `--rebuild` | container/workdir behaviour |
| `--container-architecture linux/amd64` | important on Apple Silicon |
| `--container-daemon-socket`, `--container-options`, `--container-cap-add/drop`, `--userns`, `--network` (default `host`) | container plumbing |
| `--artifact-server-path`, `--artifact-server-addr`, `--artifact-server-port` (default `34567`) | enables the built-in artifact server |
| `--cache-server-path`, `--cache-server-addr`, `--cache-server-port`, `--no-cache-server`, `--cache-server-external-url` | actions/cache emulation |
| `--action-offline-mode`, `--action-cache-path`, `--use-new-action-cache`, `--local-repository` | action fetching/caching |
| `--github-instance` | GHES |
| `--concurrent-jobs` | max parallel jobs (default = CPU count) |
| `-C, --directory`, `-a, --actor`, `--defaultbranch`, `-v/-q`, `--insecure-secrets`, `-w, --watch` | misc |
| config file | `.actrc`, one argument per line, no comments; XDG dir → `$HOME` → cwd → CLI args |

Examples for Homeplate:
```bash
act pull_request -j build -e /tmp/event.json -W .github/workflows/ci.yml \
    -P ubuntu-latest=catthehacker/ubuntu:act-latest --container-architecture linux/amd64
```

### 8.3 `runs-on: ubuntu-latest` mapping — built-in `-P` defaults
`cmd/platforms.go` @ master (verbatim):
```go
platforms := map[string]string{
    "ubuntu-latest": "node:16-buster-slim",
    "ubuntu-22.04":  "node:16-bullseye-slim",
    "ubuntu-20.04":  "node:16-buster-slim",
    "ubuntu-18.04":  "node:16-buster-slim",
}
```
The shipped `.actrc` at repo root is **empty** — the "medium/large image" choice that `act` prompts
for on first run is written into the *user's* `~/.actrc`. Documented image matrix
(<https://nektosact.com/usage/runners.html>):

| GitHub Runner | Micro | Medium | Large |
|---|---|---|---|
| `ubuntu-latest` | `node:16-buster-slim` | `catthehacker/ubuntu:act-latest` | `catthehacker/ubuntu:full-latest` |
| `ubuntu-22.04` | `node:16-bullseye-slim` | `catthehacker/ubuntu:act-22.04` | `catthehacker/ubuntu:full-22.04` |
| `ubuntu-20.04` | `node:16-buster-slim` | `catthehacker/ubuntu:act-20.04` | `catthehacker/ubuntu:full-20.04` |
| `ubuntu-18.04` | `node:16-buster-slim` | `catthehacker/ubuntu:act-18.04` | `catthehacker/ubuntu:full-18.04` |

> "**Default runners are intentionally incomplete.** These default images do not contain all the
> tools that GitHub Actions offers by default in their runners. Many things can work improperly or
> not at all … some software might still not work even if installed properly, since GitHub Actions
> run in fully virtualized machines while act is using Docker containers (e.g. Docker does not
> support running `systemd`)."

**No `macos-*` or `windows-*` default mapping exists.** The documented escape hatch is running the job
directly on the host (no Docker):
```
act -P ubuntu-latest=-self-hosted
act -P windows-latest=-self-hosted
act -P macos-latest=-self-hosted
```
i.e. the image value `-self-hosted` means "run on this host". **This is directly relevant to Homeplate**:
on a Mac, `act -P macos-latest=-self-hosted` runs a macOS job natively without Docker.

### 8.4 Known limitations (from <https://nektosact.com/not_supported.html>)
Planned but currently unsupported / ignored:
* `concurrency` ignored; `run-name` ignored
* **Step summary not processed** — `$GITHUB_STEP_SUMMARY` writes are discarded
* Problem matchers ignored; **annotations ignored**
* Incomplete `github` context
* Run-step cancellation not implemented
* `job.permissions` ignored; `job.timeout-minutes` ignored; `job.continue-on-error` ignored
* `PATH` of container/act must contain node for nodejs actions
* OIDC (`Openid Connect url`) not defined
* `job.environment` ignored; deployment-environment secret scoping unsupported

Explicitly not going to be worked on: Docker context (#583).

Other practical limits:
* **macOS/Windows runners: no images**; only via `-P <label>=-self-hosted` on a matching host.
* **Services / `services:` containers**: supported via Docker, but only on Linux images (act runs
  containers; on `-self-hosted` host mode there is no container network).
* **Matrix**: supported; filter with `--matrix key:value`.
* **Artifacts**: supported **only if you start the built-in artifact server**
  (`--artifact-server-path`); otherwise `actions/upload-artifact` fails. Cache is emulated by the
  built-in cache server (disable with `--no-cache-server`).

### 8.5 Embedding act as a Go library — YES
Module path `github.com/nektos/act`. Top-level package directories at `master`:
```
cmd/                       (the CLI)
pkg/artifactcache  pkg/artifacts  pkg/common  pkg/container  pkg/exprparser
pkg/filecollector  pkg/gh  pkg/lookpath  pkg/model  pkg/runner  pkg/schema  pkg/workflowpattern
```
**There is no `internal/` directory in the repo** — every package Homeplate needs is importable:
* `github.com/nektos/act/pkg/model` — workflow/plan parsing (`model.Plan`, `model.Workflow`)
* `github.com/nektos/act/pkg/runner` — `runner.Config` (Actor, Workdir, EventName, EventPath,
  Platforms map, Secrets, Vars, Token, ContainerArchitecture, ArtifactServerPath, ...),
  `runner.Runner` interface with `NewPlanExecutor(plan) common.Executor`
* `github.com/nektos/act/pkg/common` — `common.Executor` (the `func(ctx) error` execution primitive)
* `github.com/nektos/act/pkg/container`, `pkg/artifacts`, `pkg/artifactcache` — infra
* `github.com/nektos/act/pkg/exprparser` — GitHub expression evaluation, useful standalone

Caveats for embedding: act pulls in the full Docker CLI/moby stack (`github.com/docker/cli v29.x`,
go-git, survey) — expect a large dependency graph and a big binary; and MIT license requires
retaining the copyright notice.

---

## 9. macOS POWER

### 9.1 IOKit sleep assertions — exact API
Header verified locally: `/Library/Developer/CommandLineTools/SDKs/MacOSX26.2.sdk/System/Library/Frameworks/IOKit.framework/Versions/A/Headers/pwr_mgt/IOPMLib.h`

```c
IOReturn IOPMAssertionCreateWithName(
    CFStringRef        AssertionType,   // e.g. kIOPMAssertPreventUserIdleSystemSleep
    IOPMAssertionLevel AssertionLevel,  // kIOPMAssertionLevelOn = 255, Off = 0
    CFStringRef        AssertionName,   // human-readable, MAX 128 CHARACTERS
    IOPMAssertionID   *AssertionID)     // out
    AVAILABLE_MAC_OS_X_VERSION_10_6_AND_LATER;

IOReturn IOPMAssertionRelease(IOPMAssertionID AssertionID);
IOReturn IOPMAssertionCreateWithDescription(...);   // "the preferred API"
IOReturn IOPMAssertionSetProperty(IOPMAssertionID, CFStringRef, CFTypeRef);
IOReturn IOPMCopyAssertionsStatus(CFDictionaryRef *AssertionsStatus);
typedef uint32_t IOPMAssertionID;  enum { kIOPMNullAssertionID = 0 };
typedef uint32_t IOPMAssertionLevel;
enum { kIOPMAssertionLevelOff = 0, kIOPMAssertionLevelOn = 255 };
```
`AssertionName` max length is **128 characters** (documented in the header).

**Assertion type strings — exact values and status:**
| Constant | CFSTR value | Status |
|---|---|---|
| `kIOPMAssertPreventUserIdleSystemSleep` | `"PreventUserIdleSystemSleep"` | **current, use this** |
| `kIOPMAssertionTypePreventUserIdleSystemSleep` | alias → same string | "identical … Please use that instead" |
| `kIOPMAssertPreventUserIdleDisplaySleep` | `"PreventUserIdleDisplaySleep"` | current |
| `kIOPMAssertionTypePreventUserIdleDisplaySleep` | alias → same string | alias |
| `kIOPMAssertionTypePreventSystemSleep` | `"PreventSystemSleep"` | **deprecated in 10.9** — header: *"This assertion is not supported in any OS X releases. Do not use it."* (yet `caffeinate -s` still creates it — see 9.2) |
| `kIOPMAssertionTypeNoIdleSleep` | `"NoIdleSleepAssertion"` | **deprecated in 10.7** |
| `kIOPMAssertionTypeNoDisplaySleep` | `"NoDisplaySleepAssertion"` | **deprecated in 10.7** |
| `kIOPMAssertPreventDiskIdle` | `"PreventDiskIdle"` | current |
| `kIOPMAssertNetworkClientActive` | `"NetworkClientActive"` | current; AC-biased |

Critical header wording for `PreventUserIdleSystemSleep`:
> "The display may dim and idle sleep while `kIOPMAssertPreventUserIdleSystemSleep` is enabled, but
> the system may not idle sleep. **The system may still sleep for lid close, Apple menu, low battery,
> or other sleep reasons.** This assertion has no effect if the system is in Dark Wake."

And for `NetworkClientActive`:
> "IOKit power assertions are **suggestions** and OS X may not honor them under battery, thermal, or
> user circumstances."

### 9.2 `caffeinate` flag → assertion mapping (exact, from Apple source)
`apple-oss-distributions/PowerManagement`, `caffeinate/caffeinate.c`:
```c
AssertionMapEntry assertionMap[] = {
    { kIdleAssertionFlag,       kIOPMAssertionTypePreventUserIdleSystemSleep },  // -i
    { kDisplayAssertionFlag,    kIOPMAssertionTypePreventUserIdleDisplaySleep }, // -d
    { kSystemAssertionFlag,     kIOPMAssertionTypePreventSystemSleep},           // -s
    { kUserActiveAssertionFlag, kIOPMAssertionUserIsActive},                     // -u
    { kDiskAssertionFlag,       kIOPMAssertPreventDiskIdle}};                    // -m
```
`man caffeinate` (verified on macOS 26.6):
* `-i` — prevent the system from **idle** sleeping → `PreventUserIdleSystemSleep`
* `-d` — prevent the **display** from sleeping → `PreventUserIdleDisplaySleep`
* `-s` — prevent the system from sleeping; **"This assertion is valid only when system is running on
  AC power."** → `PreventSystemSleep`
* `-m` — prevent **disk** idle sleep → `PreventDiskIdle`
* `-u` — declare **user active** (turns display on; default 5 s timeout unless `-t`) → `UserIsActive`
* `-t <sec>` timeout, `-w <pid>` hold until pid exits, `caffeinate <utility args...>` holds for the child's lifetime.
The assertion reason string caffeinate uses is `"THE CAFFEINATE TOOL IS PREVENTING SLEEP."`.

Inspect live state with `pmset -g assertions` (real output on this Mac):
```
Assertion status system-wide:
   PreventUserIdleDisplaySleep    0
   PreventSystemSleep             0
   PreventUserIdleSystemSleep     1
   UserIsActive                   1
   NetworkClientActive            0
Listed by owning process:
   pid 343(powerd): [0x…] PreventUserIdleSystemSleep named: "Powerd - Prevent sleep while display is on"
```

### 9.3 THE CLAMSHELL REALITY (authoritative, from kernel + powerd source)

**Kernel decision function** — `apple-oss-distributions/xnu`, `iokit/Kernel/IOPMrootDomain.cpp`:
```cpp
bool IOPMrootDomain::shouldSleepOnClamshellClosed( void )
{
    if (!clamshellExists) return false;
    return !clamshellDisabled
        && !(desktopMode && acAdaptorConnected)
        && !clamshellSleepDisableMask;
}
```
and the lid-close path:
```cpp
} else if (eval_clamshell && clamshellClosed) {
    if (shouldSleepOnClamshellClosed()) {
        privateSleepSystem(kIOPMSleepReasonClamshell);
    } else {
        evaluatePolicy( kStimulusDarkWakeEvaluate );
    }
}
```

**Who can set `clamshellSleepDisableMask`** — `apple-oss-distributions/PowerManagement`,
`pmconfigd/PMAssertions.c`, `setClamshellSleepState()`:
```c
// Check lid sleep preventers on kDeclareUserActivityType and kTicklessDisplayWakeType
//   (counted only if assertion->state & kAssertionLidStateModifier)
// check desktopmode for both
desktop_mode = isDesktopMode();
if (desktop_mode) newState |= kClamshellDisableDesktopMode;
#if (TARGET_OS_OSX && TARGET_CPU_ARM64) || XCTEST
// Check assertion from WindowServer while processing hot plug  (kPreventSleepType)
#endif
if (lidSleepCount) newState |= kClamshellDisableAssertions;
...
IOConnectCallMethod(connect, kPMSetClamshellSleepState, &in, 1, ...);
```
`kAssertionLidStateModifier` is set **only** when an assertion carries the private property
`kIOPMAssertionAppliesOnLidClose` (`"AppliesOnLidClose"`), and `PMAssertions.c` gates that:
```c
value = CFDictionaryGetValue(newAssertionProperties, kIOPMAssertionAppliesOnLidClose);
if (isA_CFBoolean(value) && (value == kCFBooleanTrue)) {
    caller_is_allowed = auditTokenHasEntitlement(token, kIOPMAssertOnLidCloseEntitlement);
    if (!caller_is_allowed) { ERROR_LOG("Pid %d is not privileged to set property %@ …"); return false; }
}
```
with (from `IOKitUser/pwr_mgt.subproj/IOPMLibPrivate.h`):
```c
#define kIOPMAssertOnLidCloseEntitlement  CFSTR("com.apple.private.iokit.assertonlidclose")
// and: "This property is valid only for assertion kIOPMAssertionUserIsActive."
```
`isDesktopMode()` (`pmconfigd/PMDisplay.m`) is set by **WindowServer over XPC** (requires
`kIOPMDisplayServiceEntitlement`) and, on battery, only counts if `gDesktopModeOnBattery`:
```c
__private_extern__ bool isDesktopMode(void) {
    int pwr_src = _getPowerSource();
    if (pwr_src == kBatteryPowered) return (gDesktopMode & gDesktopModeOnBattery);
    else return gDesktopMode;
}
```

**Conclusion (honest):**
* **Closing the lid on AC while holding `caffeinate -s` / `PreventSystemSleep` / `PreventUserIdleSystemSleep`
  does NOT keep an Apple Silicon MacBook running — the machine sleeps and the job dies/stalls.**
  Those assertion types are not consulted by `shouldSleepOnClamshellClosed()`. The IOPMLib header
  says it in plain English: *"The system may still sleep for lid close …"*.
* **What DOES work: closed-display (clamshell) mode with an external display.** `desktopMode &&
  acAdaptorConnected` is exactly the kernel's exemption. Apple's documented requirements (archived
  HT201834, "Use your Mac notebook computer in closed-display mode with an external display",
  <https://web.archive.org/web/20181227143413/https://support.apple.com/en-us/HT201834>):
  > "To use closed-display mode with your Mac notebook, you need: **An AC power adapter; An external
  > keyboard and mouse or trackpad, either USB or wireless; … An external display or projector.**"
  Current Apple wording (<https://support.apple.com/en-us/102555>): "If you use an external keyboard
  and mouse with your Mac notebook, you can close the built-in display after you connect your
  external display."
  The **external display + AC** is what actually sets `desktopMode`; the external keyboard/mouse is
  needed to *wake/interact*, not to keep the machine awake. On **battery**, desktop mode only exempts
  clamshell sleep if `gDesktopModeOnBattery` is also set — i.e. **AC is effectively required**.
* Apple Silicon specific: the `#if (TARGET_OS_OSX && TARGET_CPU_ARM64)` block adds a *WindowServer
  hot-plug* `PreventSleep` path that Intel doesn't have, and clamshell state is not modified in dark
  wake (`if (!isA_FullWake()) … return;`). Net effect for third-party software is the same: **you
  cannot buy the exemption from user space without Apple's private entitlement.**

### 9.4 `sudo pmset disablesleep 1` — real, and it does work
`pmset` source (`apple-oss-distributions/PowerManagement/pmset/pmset.m`):
```c
#define ARG_DISABLESLEEP    "disablesleep"
...
} else if (0 == strncmp(argv[i], ARG_DISABLESLEEP, kMaxArgStringLength)) {
    // Any non-zero value of val (preferably 1) means DISABLE sleep. Zero means ENABLE sleep.
    CFDictionarySetValue(local_system_power_settings, kIOPMSleepDisabledKey,
                         val ? kCFBooleanTrue : kCFBooleanFalse);
    modified |= kModSystemSettings;
}
```
It flows: `pmset` → `IOPMSetSystemPowerSetting(SleepDisabled)` → powerd `PMActivateSystemPowerSettings()`
sets the `SleepDisabled` property on IOPMrootDomain → xnu:
```cpp
} else if (key->isEqualTo(sleepdisabled_string.get())) {      // "SleepDisabled"
    setProperty(key, b);
    pmPowerStateQueue->submitPowerEvent(kPowerEventUserDisabledSleep, (void *) b);
}
...
case kPowerEventUserDisabledSleep:
    userDisabledAllSleep = (kOSBooleanTrue == (OSBoolean *) arg0);
...
bool IOPMrootDomain::checkSystemSleepAllowed(IOOptionBits options, uint32_t sleepReason) {
    if (gSleepDisabledFlag) { err = kPMConfigPreventSystemSleep; break; }
    if (userDisabledAllSleep) { err = kPMUserDisabledAllSleep; break; }   // 1. user-space sleep kill switch
    ...
}
bool IOPMrootDomain::checkSystemSleepEnabled(void) { return checkSystemSleepAllowed(0, 0); }
IOReturn IOPMrootDomain::privateSleepSystem(uint32_t sleepReason) {
    if (!checkSystemSleepEnabled() || !pmPowerStateQueue) return kIOReturnNotPermitted;   // <-- lid close blocked here
    ...
}
```
Because the clamshell path calls `privateSleepSystem(kIOPMSleepReasonClamshell)`, and that returns
`kIOReturnNotPermitted` when `userDisabledAllSleep` is true, **`SleepDisabled=1` does block lid-close
sleep.**

Precise usage notes:
* Correct invocations: `sudo pmset -a disablesleep 1` / `sudo pmset disablesleep 1`; undo with `0`.
  The commonly-copied `sudo pmset -b disablesleep 1` works, but **the `-b` is meaningless**:
  `disablesleep` is stored in *system* power settings (`kModSystemSettings`), not per-power-source.
  `systemSettingKeys[] = { kIOPMSleepDisabledKey, kIOPMDestroyFVKeyOnStandbyKey }`.
* It is **not documented in `man pmset`** — verified on macOS 26.6: `man pmset | grep disablesleep`
  returns nothing. It's a real but unadvertised setting; Apple can change it.
* Requires **root**. It persists across reboots until set back to 0 — a Homeplate that sets it MUST
  restore it (and should restore on crash via a launchd `KeepAlive` cleanup or at next start).
* When set, it shows up as `SleepDisabled 1` under `pmset -g`'s "System-wide power settings".
  (On this Mac it is currently unset, so the line is absent — absence means 0.)
* Side effects: **all** sleep is disabled, including low-power/thermal-motivated demand sleep paths
  that go through `checkSystemSleepAllowed`. Running a closed MacBook with sleep fully disabled is a
  thermal risk; Homeplate should hold it only for the duration of a job and warn the user.
* Per-model/OS caveats — **UNVERIFIED in detail**: I could not empirically test lid-close behaviour on
  a range of Apple Silicon models/OS versions from this environment. The source analysis above is from
  current `xnu`/`PowerManagement` trunk and should hold for macOS 12–26 on both Intel and Apple
  Silicon, but Apple has changed clamshell handling before (note the `kClamshell_WAR_58009435`
  workaround flag in `IOPMrootDomain.cpp`).

**Recommended Homeplate policy on macOS:**
1. Always take `PreventUserIdleSystemSleep` (via `IOPMAssertionCreateWithName`, or shell out to
   `caffeinate -i -w <pid>`) while a job runs — this reliably prevents *idle* sleep.
2. Detect lid state / external display; if the lid is closed with no external display, tell the user
   plainly that jobs will not survive, unless…
3. …the user opts in to `sudo pmset disablesleep 1` (with automatic restore), or plugs in an external
   display + AC (real clamshell mode).

### 9.5 Reading battery % and AC status without cgo

**`pmset -g batt`** — exact format on this machine (macOS 26.6, MacBook Air):
```
Now drawing from 'AC Power'
 -InternalBattery-0 (id=22478947)	100%; charged; 0:00 remaining present: true
```
Grammar:
* Line 1: `Now drawing from '<AC Power|Battery Power>'`
* Line 2+: ` -<SourceName> (id=<n>)\t<pct>%; <state>; <H:MM|(no estimate)> remaining present: <true|false>`
* `<state>` observed values: `charging`, `discharging`, `charged`, `AC attached`, `finishing charge`.
* `pmset -g ps` prints the same thing (`-g ps` and `-g batt` are equivalent for this purpose;
  `man pmset`: "-g with a 'batt' or 'ps' argument will show the state of all attached power sources").
* Robust parse: `AC = strings.Contains(line1, "'AC Power'")`; `pct = regexp` on `(\d+)%`.

**`ioreg -rn AppleSmartBattery`** — real key/value output on this machine:
```
"CurrentCapacity"       = 100        # on Apple Silicon this is ALREADY A PERCENT (0-100)
"MaxCapacity"           = 100        # 100 on Apple Silicon (percent basis), mAh on older Intel
"DesignCapacity"        = 5760       # mAh
"AppleRawCurrentCapacity" = 5246     # mAh, the real raw charge
"ExternalConnected"     = Yes        # <-- AC status
"IsCharging"            = No
"FullyCharged"          = Yes
"TimeRemaining"         = 65535      # 65535 == "not calculable" sentinel
"Voltage"               = 13106      # mV
"CycleCount"            = 150
```
**Trap:** on Apple Silicon `CurrentCapacity`/`MaxCapacity` are percentages (100/100), *not* mAh —
computing `CurrentCapacity/MaxCapacity*100` still gives the right percent, but
`CurrentCapacity/DesignCapacity` does not. Use `AppleRawCurrentCapacity` for true mAh.
`TimeRemaining == 65535` means "unknown/still calculating".

Machine-readable variants (all cgo-free, just `os/exec`):
* `ioreg -arn AppleSmartBattery` → **XML plist** on stdout (parse with `howett.net/plist` or shell to `plutil`).
* `pmset -g batt` → the text above.
* `system_profiler -json SPPowerDataType` → JSON, slower (~1 s) but structured.
Pure-Go option without exec: `github.com/distatus/battery` (uses IOKit via purego/syscall on darwin) —
**UNVERIFIED** for macOS 26 / Apple Silicon; validate before depending on it.

---

## 10. LINUX

### 10.1 Inhibiting sleep — `systemd-inhibit`
Source: `man 1 systemd-inhibit` (<https://man7.org/linux/man-pages/man1/systemd-inhibit.1.html>).

```
systemd-inhibit [OPTIONS...] COMMAND [ARGUMENTS...]
systemd-inhibit [OPTIONS...] --list
```
* `--what=` colon-separated list of: `shutdown`, `sleep`, `idle`, `handle-power-key`,
  `handle-reboot-key`, `handle-suspend-key`, `handle-hibernate-key`, `handle-lid-switch`.
  **Default: `idle:sleep:shutdown`.**
* `--who=` short program name (defaults to the command line), `--why=` reason (defaults to "Unknown reason").
* `--mode=` one of `block` (default, no time limit, only privileged users may override),
  `block-weak` (like block but not enforced against privileged clients or the lock owner's own
  operations), `delay` (bounded delay, limit in `logind.conf(5)`; **only valid for `sleep` and `shutdown`**).
* `--list` lists active inhibitor locks; `--no-ask-password`, `--no-pager`, `--no-legend`.
* The lock is held for the lifetime of the child process and released automatically.

Homeplate equivalents:
| macOS | Linux |
|---|---|
| `caffeinate -i <cmd>` | `systemd-inhibit --what=idle:sleep --who=homeplate --why="CI job running" <cmd>` |
| lid-close survival | `systemd-inhibit --what=handle-lid-switch` (this is the real clamshell equivalent — and unlike macOS it is a **supported, unprivileged** API) |
| `pmset disablesleep 1` | `--what=sleep --mode=block` (or `HandleLidSwitch=ignore` in `/etc/systemd/logind.conf`) |
Full belt-and-braces: `--what=idle:sleep:shutdown:handle-lid-switch --mode=block`.
Programmatic equivalent without shelling out: D-Bus
`org.freedesktop.login1.Manager.Inhibit(what, who, why, mode)` → returns a file descriptor; hold the
fd open to hold the lock (e.g. via `github.com/godbus/dbus/v5`).

### 10.2 AC / battery from `/sys/class/power_supply`
Source: kernel ABI doc <https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-class-power>.

```
/sys/class/power_supply/<supply_name>/type      -> "Battery" | "UPS" | "Mains" | "USB" | "Wireless"
/sys/class/power_supply/<supply_name>/online    -> 0 Offline | 1 Online (Fixed) | 2 Online (Programmable)
/sys/class/power_supply/<supply_name>/status    -> "Unknown" | "Charging" | "Discharging" | "Not charging" | "Full"
/sys/class/power_supply/<supply_name>/capacity  -> 0-100 (percent)
/sys/class/power_supply/<supply_name>/present   -> 0 Absent | 1 Present
```
Typical names: `AC`, `ACAD`, `ADP1`, `AC0` (Mains) and `BAT0`, `BAT1` (Battery). Also available:
`energy_now`/`energy_full`, `charge_now`/`charge_full`, `voltage_now`, `power_now`,
`capacity_level`, `time_to_empty_now`, `cycle_count`, `technology`, `manufacturer`, `model_name`.

Robust Go algorithm (no cgo, no exec):
```go
// AC present?  -> iterate /sys/class/power_supply/*, read "type";
//                 for any type=="Mains" (or "USB"), online=="1" => on AC.
// Battery %    -> for type=="Battery": read "capacity" (0-100);
//                 fall back to energy_now/energy_full*100 or charge_now/charge_full*100.
// Charging?    -> "status" == "Charging" | "Full".
// No battery at all (desktop/VM): /sys/class/power_supply may be empty -> treat as always-on-AC.
```
Notes: values are ASCII with a trailing newline; `capacity` may be missing on some drivers (use the
energy/charge fallback); `online` on a USB-PD supply can legitimately be `2`, so test `!= 0`, not `== 1`.

---

## Source index
* GitHub workflow syntax / `runs-on`: <https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idruns-on>
* Choosing the runner for a job: <https://docs.github.com/en/actions/how-tos/write-workflows/choose-where-workflows-run/choose-the-runner-for-a-job>
* Self-hosted runners in a workflow: <https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/use-in-a-workflow>
* Labels with self-hosted runners: <https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/apply-labels>
* Self-hosted runners reference (routing precedence, OS/arch support, ephemeral): <https://docs.github.com/en/actions/reference/runners/self-hosted-runners>
* Runner groups: <https://docs.github.com/en/actions/concepts/runners/runner-groups>
* Disabling/limiting Actions for an org (incl. disabling standard hosted runners): <https://docs.github.com/en/organizations/managing-organization-settings/disabling-or-limiting-github-actions-for-your-organization>
* Changelog, more control over hosted runners (2026-06-25): <https://github.blog/changelog/2026-06-25-more-control-over-your-github-hosted-runners/>
* Community #20019 (hosted-label trick, 2023): <https://github.com/orgs/community/discussions/20019>
* Community #162274 (it stopped, 2025): <https://github.com/orgs/community/discussions/162274>
* REST — self-hosted runners: <https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28>
* REST — checks/runs: <https://docs.github.com/en/rest/checks/runs?apiVersion=2022-11-28>
* REST — commit statuses: <https://docs.github.com/en/rest/commits/statuses?apiVersion=2022-11-28>
* REST — PR reviews / merge: <https://docs.github.com/en/rest/pulls/reviews?apiVersion=2022-11-28>, <https://docs.github.com/en/rest/pulls/pulls?apiVersion=2022-11-28>
* OpenAPI ground truth: <https://github.com/github/rest-api-description> (`descriptions/api.github.com/dereferenced/api.github.com.deref.json`)
* Device flow: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps>
* OAuth scopes: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps>
* CLI-with-a-GitHub-App tutorial: <https://docs.github.com/en/apps/creating-github-apps/writing-code-for-a-github-app/building-a-cli-with-a-github-app>
* gh CLI client id: <https://github.com/cli/cli/blob/trunk/internal/authflow/flow.go>
* ToS (API Terms §H, eff. 2026-04-27): <https://docs.github.com/en/site-policy/github-terms/github-terms-of-service>
* Impersonation policy: <https://docs.github.com/en/site-policy/acceptable-use-policies/github-impersonation>
* Actions runner pricing: <https://docs.github.com/en/billing/reference/actions-minute-multipliers>
* GitHub Actions billing: <https://docs.github.com/en/billing/concepts/product-billing/github-actions>
* Billing & usage (multipliers mention): <https://docs.github.com/en/actions/concepts/billing-and-usage>
* Archived multiplier table: <https://web.archive.org/web/20240229120346/https://docs.github.com/en/billing/managing-billing-for-github-actions/about-billing-for-github-actions>
* actions/runner releases: <https://api.github.com/repos/actions/runner/releases/latest>
* actions/runner source: <https://github.com/actions/runner> (`src/Runner.Common/Constants.cs`, `src/Runner.Listener/CommandSettings.cs`, `src/Runner.Listener/Configuration/ConfigurationManager.cs`)
* macOS arm64 v2.336.0 regression: <https://github.com/actions/runner/issues/4570>, <https://github.com/actions/runner/issues/4575>, <https://github.com/actions/runner/pull/4572>
* GitHub Status API: <https://www.githubstatus.com/api/v2/>
* nektos/act: <https://github.com/nektos/act>, <https://nektosact.com>, <https://nektosact.com/usage/runners.html>, <https://nektosact.com/not_supported.html>
* IOPMLib.h (local SDK): `/Library/Developer/CommandLineTools/SDKs/MacOSX26.2.sdk/System/Library/Frameworks/IOKit.framework/Versions/A/Headers/pwr_mgt/IOPMLib.h`
* Apple PowerManagement source: <https://github.com/apple-oss-distributions/PowerManagement> (`caffeinate/caffeinate.c`, `pmset/pmset.m`, `pmconfigd/PMAssertions.c`, `pmconfigd/PMDisplay.m`, `pmconfigd/PMSettings.m`)
* IOKitUser private header: <https://github.com/apple-oss-distributions/IOKitUser/blob/main/pwr_mgt.subproj/IOPMLibPrivate.h>
* xnu clamshell/sleep logic: <https://github.com/apple-oss-distributions/xnu/blob/main/iokit/Kernel/IOPMrootDomain.cpp>
* Apple closed-display mode requirements (archived HT201834): <https://web.archive.org/web/20181227143413/https://support.apple.com/en-us/HT201834>
* Apple, connect displays (current): <https://support.apple.com/en-us/102555>
* systemd-inhibit(1): <https://man7.org/linux/man-pages/man1/systemd-inhibit.1.html>
* Linux power_supply sysfs ABI: <https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-class-power>
