# homebrew-tap

Homebrew tap for [lopen](https://github.com/jaxonwang/lopen) — open remote files
and directories on your local Mac straight from an ssh session.

## Install

```sh
brew tap jaxonwang/tap
brew install lopen
# or, in one line:
brew install jaxonwang/tap/lopen

brew services start lopen
```

Then provision each remote host from your Mac:

```sh
lopen setup <ssh-host>        # an ssh_config alias or user@host
```

## About

`Formula/lopen.rb` tracks tagged releases of the lopen repo. Each release bumps
the `url` and `sha256` in the formula (see the main repo's `RELEASING.md`).
