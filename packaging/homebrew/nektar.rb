# Official Homebrew Formula for nektar (GHCR launcher)
# Publish to tap repo: kryptokrates/homebrew-tap -> Formula/nektar.rb
#   brew install kryptokrates/tap/nektar
class Nektar < Formula
  desc "Verifiable, encrypted AI agent memory and knowledge market on BSV (GHCR launcher)"
  homepage "https://nektar.run"
  url "https://raw.githubusercontent.com/kryptokrates/nektar/v0.3.2/install/nektar"
  version "0.3.2"
  license "MIT"

  def install
    bin.install "nektar"
  end

  def caveats
    <<~EOS
      Start your local nektar agent (uses ghcr.io/kryptokrates/nektar:latest):
        nektar up
        nektar open
        nektar dock
    EOS
  end

  test do
    assert_match "nektar", shell_output("#{bin}/nektar --help")
  end
end
