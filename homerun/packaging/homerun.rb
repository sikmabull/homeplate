# Homebrew formula for Homerun.
#
# Publish to a tap (homerun-ci/homebrew-tap) and users get:
#   brew install homerun-ci/tap/homerun
#
# Replace SHA placeholders with the values from `make dist` -> dist/checksums.txt.
class Homerun < Formula
  desc "Run your GitHub Actions on your own machine at $0 per minute"
  homepage "https://github.com/homerun-ci/homerun"
  version "0.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/homerun-ci/homerun/releases/download/v#{version}/homerun-#{version}-darwin-arm64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_ARM64_SHA256"
    end
    on_intel do
      url "https://github.com/homerun-ci/homerun/releases/download/v#{version}/homerun-#{version}-darwin-amd64.tar.gz"
      sha256 "REPLACE_WITH_DARWIN_AMD64_SHA256"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/homerun-ci/homerun/releases/download/v#{version}/homerun-#{version}-linux-arm64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_ARM64_SHA256"
    end
    on_intel do
      url "https://github.com/homerun-ci/homerun/releases/download/v#{version}/homerun-#{version}-linux-amd64.tar.gz"
      sha256 "REPLACE_WITH_LINUX_AMD64_SHA256"
    end
  end

  # Docker is required for clean-room Linux jobs but is a cask / external
  # install, so it is checked by `homerun doctor` rather than declared here.
  # act is optional: it is only needed for offline mode (Engine B).
  depends_on "git"

  def install
    bin.install Dir["homerun-*"].first => "homerun"
  end

  def caveats
    <<~EOS
      Get started:
        homerun init      authenticate, pick repos, install the daemon
        homerun doctor    check Docker, power, connectivity, label routing

      Offline mode (Engine B) additionally needs nektos/act:
        brew install act

      Homerun installs a launchd agent that starts at login. Remove it with:
        homerun service uninstall
    EOS
  end

  test do
    assert_match "homerun", shell_output("#{bin}/homerun --help")
    system "#{bin}/homerun", "--version"
  end
end
