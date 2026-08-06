package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// entrypointScript configures an EPHEMERAL runner and hands off to run.sh.
//
// Ephemeral is the whole isolation story: GitHub hands this registration
// exactly one job, then the runner exits and the container is destroyed. There
// is no second job that could observe leftover state.
//
// The registration token arrives via a read-only bind mount rather than an
// environment variable, because `docker inspect` exposes env to any local user
// while the mount disappears with the container.
const entrypointScript = `#!/usr/bin/env bash
set -euo pipefail

cd /home/runner/actions-runner

# Preferred path: a just-in-time config minted server-side for exactly this
# job. No secret is involved and no config.sh step is needed.
if [ -f /run/homeplate/jitconfig ]; then
  JIT="$(cat /run/homeplate/jitconfig)"
  shred -u /run/homeplate/jitconfig 2>/dev/null || rm -f /run/homeplate/jitconfig
  echo "homeplate: starting with JIT config (ephemeral, exactly one job)"
  exec ./run.sh --jitconfig "${JIT}"
fi

TOKEN="$(cat /run/homeplate/token)"

CONFIG_ARGS=(
  --unattended
  --ephemeral
  --disableupdate
  --replace
  --url "${HOMEPLATE_URL}"
  --token "${TOKEN}"
  --name "${HOMEPLATE_NAME}"
  --labels "${HOMEPLATE_LABELS}"
  --work /home/runner/_work
)

if [ -n "${HOMEPLATE_GROUP:-}" ]; then
  CONFIG_ARGS+=(--runnergroup "${HOMEPLATE_GROUP}")
fi

# Never echo the token, even under set -x.
./config.sh "${CONFIG_ARGS[@]}" 2>&1 | sed -e "s/${TOKEN}/***/g"

# The registration token is valid for an hour and could register more
# runners; the job must not be able to read it once configuration is done.
shred -u /run/homeplate/token 2>/dev/null || rm -f /run/homeplate/token

echo "homeplate: runner configured as ephemeral; awaiting exactly one job"

# run.sh exits after a single job because the runner is ephemeral.
exec ./run.sh
`

// dockerfileTemplate builds a minimal Ubuntu image carrying the official
// runner. Users can override the base image via config; the requirements are
// glibc, bash, git, and the .NET runtime dependencies the runner needs.
const dockerfileTemplate = `# syntax=docker/dockerfile:1
FROM %s

ENV DEBIAN_FRONTEND=noninteractive \
    RUNNER_MANUALLY_TRAP_SIG=1 \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1

# Dependencies: the first group is what actions/runner itself needs (.NET),
# the second is the baseline toolchain nearly every workflow assumes exists.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git jq unzip zip tar gzip xz-utils \
      libicu-dev liblttng-ust-dev libkrb5-3 zlib1g libssl-dev \
      build-essential python3 python3-pip python3-venv sudo locales \
    && locale-gen en_US.UTF-8 \
    && rm -rf /var/lib/apt/lists/*

ENV LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8

# Non-root by default. The runner refuses to configure as root anyway, and a
# job that wants to install packages uses passwordless sudo inside the
# throwaway container - which cannot touch the host.
RUN useradd -m -u 1001 -s /bin/bash runner \
    && echo "runner ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/runner \
    && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /home/runner/actions-runner /home/runner/_work /run/homeplate \
    && chown -R runner:runner /home/runner

COPY --chown=runner:runner actions-runner.tar.gz /tmp/actions-runner.tar.gz
COPY --chown=runner:runner entrypoint.sh /home/runner/entrypoint.sh

USER runner
WORKDIR /home/runner/actions-runner
RUN tar xzf /tmp/actions-runner.tar.gz && rm -f /tmp/actions-runner.tar.gz \
    && chmod +x /home/runner/entrypoint.sh

ENTRYPOINT ["/home/runner/entrypoint.sh"]
`

// ImageBuilder assembles the Homeplate runner image.
type ImageBuilder struct {
	Docker    *Docker
	Cache     *Cache
	BaseImage string
	// ContainerArch is the runner platform for the CONTAINER, which on Apple
	// Silicon is linux-arm64 even though the host is osx-arm64.
	ContainerArch string
}

// ImageTag is the deterministic tag for a (runner version, base image) pair, so
// changing the base image rebuilds rather than silently reusing a stale image.
func ImageTag(runnerVersion, baseImage string) string {
	sum := sha256.Sum256([]byte(baseImage))
	return fmt.Sprintf("homeplate/runner:%s-%s", runnerVersion, hex.EncodeToString(sum[:])[:8])
}

// containerRunnerPlatform maps the container architecture to a runner tarball.
func containerRunnerPlatform(ctx context.Context, d *Docker) string {
	out, err := exec.CommandContext(ctx, d.Bin, "info", "--format", "{{.Architecture}}").Output()
	arch := strings.TrimSpace(string(out))
	if err != nil || arch == "" {
		arch = "x86_64"
	}
	switch arch {
	case "aarch64", "arm64":
		return "linux-arm64"
	default:
		return "linux-x64"
	}
}

// Ensure builds the runner image if it is not already present, and returns its
// tag. Building is idempotent and cheap on repeat runs thanks to layer caching.
func (b *ImageBuilder) Ensure(ctx context.Context, runnerVersion string, log io.Writer) (string, error) {
	if b.BaseImage == "" {
		b.BaseImage = "ubuntu:22.04"
	}
	tag := ImageTag(runnerVersion, b.BaseImage)
	if b.Docker.ImageExists(ctx, tag) {
		return tag, nil
	}

	plat := b.ContainerArch
	if plat == "" {
		plat = containerRunnerPlatform(ctx, b.Docker)
	}
	rel := ResolveRelease(runnerVersion, plat)

	buildDir := filepath.Join(b.Cache.Dir, "image", runnerVersion+"-"+plat)
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", err
	}

	// The tarball lands in the build context so Docker can COPY it, avoiding a
	// network fetch inside the build (which would break on an offline machine
	// that already has the tarball cached).
	tarballDest := filepath.Join(buildDir, "actions-runner.tar.gz")
	if _, err := os.Stat(tarballDest); err != nil {
		src := b.Cache.TarballPath(rel)
		if _, err := os.Stat(src); err != nil {
			fmt.Fprintf(log, "homeplate: downloading actions/runner %s for %s\n", runnerVersion, plat)
			if err := download(ctx, rel.URL, src); err != nil {
				return "", fmt.Errorf("download runner for container: %w", err)
			}
		}
		if err := copyFile(src, tarballDest); err != nil {
			return "", err
		}
	}

	if err := os.WriteFile(filepath.Join(buildDir, "entrypoint.sh"), []byte(entrypointScript), 0o755); err != nil {
		return "", err
	}
	dockerfile := fmt.Sprintf(dockerfileTemplate, b.BaseImage)
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}

	fmt.Fprintf(log, "homeplate: building clean-room image %s (one time, ~2 min)\n", tag)
	cmd := exec.CommandContext(ctx, b.Docker.Bin, "build", "-t", tag, buildDir)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build runner image: %w", err)
	}
	return tag, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
