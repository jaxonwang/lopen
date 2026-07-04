# Tap copy of the lopen formula. Keep this IDENTICAL to Formula/lopen.rb in the
# main repo (github.com/jaxonwang/lopen); this file is what lives in the tap repo
# github.com/jaxonwang/homebrew-tap. See RELEASING.md for the sync procedure.
class Lopen < Formula
  desc "Open remote files and directories on your local Mac from an SSH session"
  homepage "https://github.com/jaxonwang/lopen"
  url "https://github.com/jaxonwang/lopen/archive/refs/tags/v0.1.1.tar.gz"
  sha256 "ab2772c50ffcb6ed90d2bb01a4be504b8c8310f106d6c49441b2546da70cf180"
  license "MIT"

  depends_on :macos
  depends_on "python@3.12"

  def install
    # Daemon + the script shipped to remote hosts live in libexec.
    libexec.install "lopend/lopend.py"
    libexec.install "bin/lopen" => "remote-lopen"
    libexec.install "cli/lopen" => "lopen"
    chmod 0755, libexec/"lopend.py"
    chmod 0755, libexec/"remote-lopen"
    chmod 0755, libexec/"lopen"

    # A small bash wrapper on PATH that sets LOPEN_LIBEXEC (so the CLI finds its
    # companions) and HOMEBREW_PREFIX (for log-path resolution + brew mode).
    (bin/"lopen").write <<~EOS
      #!/bin/bash
      export LOPEN_LIBEXEC="#{libexec}"
      export HOMEBREW_PREFIX="#{HOMEBREW_PREFIX}"
      exec "#{formula_opt_bin("python@3.12")}/python3.12" "#{libexec}/lopen" "$@"
    EOS
    chmod 0755, bin/"lopen"
  end

  service do
    run [formula_opt_bin("python@3.12")/"python3.12", opt_libexec/"lopend.py"]
    keep_alive true
    log_path var/"log/lopen.log"
    error_log_path var/"log/lopen.log"
  end

  def caveats
    <<~EOS
      Start the local daemon with:
        brew services start lopen

      Then provision each remote host you ssh into:
        lopen setup <ssh-host>       # an ssh_config alias or user@host

      lopen relies on your terminal's OSC 52 clipboard support (iTerm2, WezTerm,
      kitty, Alacritty, ...). Terminal.app does NOT support OSC 52 writes.

      The `lopen start|stop|status|restart` subcommands auto-detect Homebrew and
      delegate to `brew services`.
    EOS
  end

  test do
    assert_match "usage", shell_output("#{bin}/lopen --help 2>&1")
  end
end
