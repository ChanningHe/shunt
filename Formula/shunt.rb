class Shunt < Formula
  desc "Local Claude Code router: subagents go to a third-party provider"
  homepage "https://github.com/ChanningHe/shunt"
  head "https://github.com/ChanningHe/shunt.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-X shunt/internal/cli.Version=#{version}")
  end

  service do
    run [opt_bin/"shunt", "serve"]
    keep_alive true
    log_path var/"log/shunt.log"
    error_log_path var/"log/shunt.log"
  end

  def caveats
    <<~EOS
      Start the router, then point Claude Code at it:
        brew services start shunt
        shunt on

      To stop, turn routing off first; with routing on and the router down,
      all of Claude Code fails:
        shunt off
        brew services stop shunt

      The configuration lives in ~/.config/shunt. The service does not see
      XDG_CONFIG_HOME or SHUNT_CONFIG_DIR from your shell.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/shunt --version")
    assert_match "usage: shunt", shell_output("#{bin}/shunt --help")
  end
end
