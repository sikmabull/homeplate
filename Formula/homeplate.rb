class Homeplate < Formula
  desc "Bring your runners home: your machine as your GitHub Actions CI"
  homepage "https://github.com/homeplate-ci/homeplate"
  version "0.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/homeplate-ci/homeplate/releases/download/v#{version}/homeplate_darwin_arm64.tar.gz"
      # sha256 filled in by the release workflow
    end
    on_intel do
      url "https://github.com/homeplate-ci/homeplate/releases/download/v#{version}/homeplate_darwin_amd64.tar.gz"
    end
  end
  on_linux do
    on_arm do
      url "https://github.com/homeplate-ci/homeplate/releases/download/v#{version}/homeplate_linux_arm64.tar.gz"
    end
    on_intel do
      url "https://github.com/homeplate-ci/homeplate/releases/download/v#{version}/homeplate_linux_amd64.tar.gz"
    end
  end

  def install
    bin.install "homeplate"
  end

  def caveats
    <<~EOS
      Get everything running in one shot:
        homeplate auto

      Or step by step:
        homeplate init     # auth (device flow or --pat) + daemon
        homeplate scan     # find your local GitHub clones
        homeplate adopt    # open the runs-on PR per repo

      Optional, for offline mode:  brew install act
      Optional, lid-closed jobs:   homeplate power setup
    EOS
  end

  test do
    assert_match "homeplate", shell_output("#{bin}/homeplate --help")
  end
end
