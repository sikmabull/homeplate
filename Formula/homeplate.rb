class Homeplate < Formula
  desc "Bring your runners home: your machine as your GitHub Actions CI"
  homepage "https://github.com/sikmabull/homeplate"
  version "0.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/sikmabull/homeplate/releases/download/v0.1.0/homeplate_0.1.0_darwin-arm64.tar.gz"
      sha256 "bfc888dfbc5b9efc328b0cd5e3a600b3f2cfb3e7ce182af9a25809af1ab6a1ee"
    end
    on_intel do
      url "https://github.com/sikmabull/homeplate/releases/download/v0.1.0/homeplate_0.1.0_darwin-amd64.tar.gz"
      sha256 "ca9760827fe13b278547f8357d0bc1e88ae16b38619968cdfe83535967e9a95f"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/sikmabull/homeplate/releases/download/v0.1.0/homeplate_0.1.0_linux-arm64.tar.gz"
      sha256 "bc07754e1cc09b9be76941a397fb60f7fa4d5716368d119bfd3aa80d39e6abd9"
    end
    on_intel do
      url "https://github.com/sikmabull/homeplate/releases/download/v0.1.0/homeplate_0.1.0_linux-amd64.tar.gz"
      sha256 "77351850c6be1b5dc2186c4b70564846ae730a5c5029db05cba5cc1959f28da6"
    end
  end

  def install
    bin.install "homeplate"
  end

  def caveats
    <<~EOS
      One-shot setup (an AI assistant can run this for you):
        homeplate auto

      Step by step:
        homeplate init     # auth + daemon
        homeplate scan     # find your local GitHub clones
        homeplate adopt    # open the runs-on PR per repo

      Optional, for offline mode:  brew install act
      Optional, lid-closed jobs:   homeplate power setup
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/homeplate --version")
  end
end
