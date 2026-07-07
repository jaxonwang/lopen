class Lopen < Formula
  desc "Open remote files and directories on your local Mac from an SSH session"
  homepage "https://github.com/jaxonwang/lopen"
  url "https://github.com/jaxonwang/lopen/archive/refs/tags/v0.3.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "MIT"

  depends_on "go" => :build
  depends_on :macos

  def install
    # Build the macOS daemon/CLI. No external modules, so the build is offline.
    ldflags = "-s -w"
    system "go", "build", *std_go_args(ldflags: ldflags, output: bin/"lopend"), "./cmd/lopend"

    # Cross-compile the remote (Linux) client for both architectures and stage
    # it with the lssh wrapper in libexec; `lopend setup` pushes these to hosts.
    %w[amd64 arm64].each do |arch|
      ENV["GOOS"] = "linux"
      ENV["GOARCH"] = arch
      ENV["CGO_ENABLED"] = "0"
      system "go", "build", "-trimpath", "-ldflags", ldflags,
             "-o", libexec/"lopen-linux-#{arch}", "./cmd/lopen"
    end
    ENV.delete("GOOS")
    ENV.delete("GOARCH")

    libexec.install "scripts/lssh"
    chmod 0755, libexec/"lssh"

    # `lopend setup` finds the pushable assets via LOPEN_LIBEXEC. Wrap the
    # daemon binary so the env var is always set, however lopend is invoked.
    (bin/"lopend").rename(libexec/"lopend")
    (bin/"lopend").write <<~EOS
      #!/bin/bash
      export LOPEN_LIBEXEC="#{libexec}"
      exec "#{libexec}/lopend" "$@"
    EOS
    chmod 0755, bin/"lopend"
  end

  service do
    run [opt_bin/"lopend", "run"]
    keep_alive true
    log_path var/"log/lopen.log"
    error_log_path var/"log/lopen.log"
  end

  def caveats
    <<~EOS
      Enroll each remote host you ssh into (from your Mac):
        lopend setup <ssh-host>        # an ssh_config alias or user@host

      Then start the local daemon:
        brew services start lopen

      Check configuration and enrolled hosts anytime with:
        lopend doctor

      lopen requires passwordless ssh from your Mac to each enrolled host, and
      forwards a loopback TCP port (default 47654) on the remote. Nothing needs
      to be preinstalled on remote hosts; `lopend setup` pushes the client.
    EOS
  end

  test do
    assert_match "usage", shell_output("#{bin}/lopend --help 2>&1")

    # doctor reads a config and reports enrolled hosts.
    (testpath/"config.json").write <<~JSON
      { "hosts": [ { "label": "devbox", "dest": "example.com" } ] }
    JSON
    out = shell_output("#{bin}/lopend doctor -config #{testpath}/config.json 2>&1")
    assert_match "devbox", out
    assert_match "hosts: 1", out

    # The pushable remote assets are staged in libexec.
    assert_path_exists libexec/"lopen-linux-amd64"
    assert_path_exists libexec/"lopen-linux-arm64"
    assert_path_exists libexec/"lssh"
  end
end
