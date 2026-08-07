# Releasing Homeplate

## What is live today (v0.1.0)

| Channel | Command | Status |
|---|---|---|
| Homebrew tap | `brew install sikmabull/tap/homeplate` | **live** (repo: sikmabull/homebrew-tap) |
| GitHub release | https://github.com/sikmabull/homeplate/releases/tag/v0.1.0 | **live** (4 platforms + checksums) |
| go install | `go install github.com/homeplate-ci/homeplate/cmd/homeplate@latest` | **live** |
| npm | `npm install -g homeplate` | **package ready in npm/, needs one-time auth** |

## Publishing npm (one-time)

The package is prepared in `npm/` (postinstall downloads the release binary and
verifies its SHA-256). The name `homeplate` is unclaimed. One command after
logging in:

```bash
npm login                                   # or: npm adduser (browser flow)
cd npm && npm publish --access public
```

Grab the name NOW even before a marketing push — unclaimed names are squatting
targets. If you'd rather CI publish it: add an NPM_TOKEN granular access token
as a repo secret and run `npm publish` from the release workflow.

## Cutting the next release

```bash
git tag v0.2.0 && git push origin v0.2.0
```

.github/workflows/release.yml runs tests, builds darwin/linux × arm64/amd64,
creates the GitHub release with tarballs + checksums, and regenerates the tap
formula (needs a TAP_TOKEN repo secret: fine-grained PAT, contents:write on
sikmabull/homebrew-tap).

## After releasing

- Bump `version` and `CHECKSUMS` in `npm/install.js` and republish npm.
- The site install command (`site/app/page.jsx`, `site/app/docs/page.jsx`)
  needs no change — it points at the tap, which follows releases.
