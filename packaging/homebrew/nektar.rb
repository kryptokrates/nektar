class Nektar < Formula
  desc "Verifiable, encrypted AI agent memory & knowledge market anchored on BSV (v0.4 Pure-Go)"
  homepage "https://nektar.run"
  version "0.4.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/kryptokrates/nektar/releases/download/v#{version}/nektar-darwin-arm64.tar.gz"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/kryptokrates/nektar/releases/download/v#{version}/nektar-linux-amd64.tar.gz"
    end
    on_arm do
      url "https://github.com/kryptokrates/nektar/releases/download/v#{version}/nektar-linux-arm64.tar.gz"
    end
  end

  def install
    bin.install "nektar"
  end

  def caveats
    <<~EOS
      Quickstart (zero Docker, starts in <50ms on http://127.0.0.1:18420):
        nektar serve         # start daemon + Svelte Dashboard + MCP + L0 LLM Proxy
        nektar dock          # auto-wire Claude Code, Cursor, VS Code, Hermes, OpenClaw, Codex
        nektar open          # open Dashboard with 1-click pairing
        nektar hub up --auto-ip  # promote to a public HTTPS Hub Node
    EOS
  end

  test do
    assert_match "nektar", shell_output("#{bin}/nektar version")
  end
end
