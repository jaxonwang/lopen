# Releasing lopen

Homebrew builds `lopen` from a tagged source tarball (the formula compiles the
Go daemon and cross-compiles the remote clients — no prebuilt binaries to
upload). To cut a release `vX.Y.Z`:

1. **Bump the version in the formula** and push the change to `main`. Set
   `Formula/lopen.rb`'s `url` to the new tag; leave the `sha256` as-is for now.

2. **Tag and push the tag:**

   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

3. **Get the tarball sha256** for the auto-generated GitHub archive:

   ```sh
   URL="https://github.com/jaxonwang/lopen/archive/refs/tags/vX.Y.Z.tar.gz"
   curl -sL "$URL" | shasum -a 256
   ```

4. **Set the `sha256`** in `Formula/lopen.rb` to that value and commit/push.

5. **Sanity-check** the formula:

   ```sh
   ruby -c Formula/lopen.rb
   brew style Formula/lopen.rb
   ```

6. **Publish the tap.** Copy `Formula/lopen.rb` into the tap repo
   `github.com/jaxonwang/homebrew-tap` at `Formula/lopen.rb` and push:

   ```sh
   cp Formula/lopen.rb /path/to/homebrew-tap/Formula/lopen.rb
   cd /path/to/homebrew-tap && git commit -am "lopen vX.Y.Z" && git push
   ```

7. **Verify a clean install:**

   ```sh
   brew update
   brew install jaxonwang/tap/lopen
   lopend setup <ssh-host>
   brew services start lopen
   lopend doctor
   ```

Until the tap is updated, users can install straight from this repo's formula:

```sh
brew install https://raw.githubusercontent.com/jaxonwang/lopen/main/Formula/lopen.rb
```

## Local formula test (before tagging)

You can validate the formula against an untagged tree with a local tarball and
a throwaway tap:

```sh
git archive --format=tar.gz --prefix=lopen-X.Y.Z/ -o /tmp/lopen-X.Y.Z.tar.gz HEAD
SHA=$(shasum -a 256 /tmp/lopen-X.Y.Z.tar.gz | awk '{print $1}')
brew tap-new local/lopen --no-git
sed -e "s#url \".*\"#url \"file:///tmp/lopen-X.Y.Z.tar.gz\"#" \
    -e "s#sha256 \".*\"#sha256 \"$SHA\"#" \
    Formula/lopen.rb > "$(brew --repository local/lopen)/Formula/lopen.rb"
brew install --build-from-source local/lopen/lopen
brew test local/lopen/lopen
brew uninstall lopen && brew untap local/lopen
```
