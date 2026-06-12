class Airlock < Formula
  desc "Agent coordination daemon for macOS and Linux — locks, notes, events over MCP"
  homepage "https://airlock-web.vercel.app"
  url "https://github.com/adamorad/airlock/archive/refs/tags/v2.0.0.tar.gz"
  sha256 "PLACEHOLDER_FILLED_AFTER_TAG"
  version "2.0.0"
  license "MIT"
  depends_on "go" => :build
  def install
    system "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", bin/"airlock", "."
  end
  def caveats
    <<~EOS
      Start Airlock and register it as a service:
        airlock install-service
      Then point your MCP client at http://127.0.0.1:27183
      On Linux a bearer token is required — see ~/.airlock/token
    EOS
  end
  test do
    assert_match "airlock", shell_output("#{bin}/airlock version")
  end
end
