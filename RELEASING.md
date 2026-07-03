# Releasing lopen

Homebrew installs from a tagged source tarball. To cut a release `vX.Y.Z`:

1. **Tag and push the tag** on `github.com/jaxonwang/lopen`:

   ```sh
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

2. **Get the tarball sha256** for the auto-generated GitHub archive:

   ```sh
   URL="https://github.com/jaxonwang/lopen/archive/refs/tags/vX.Y.Z.tar.gz"
   curl -sL "$URL" | shasum -a 256
   ```

3. **Update both formulae** — set the matching `url` (bump the version) and
   replace the `sha256` with the value from step 2, in BOTH:
   - `Formula/lopen.rb` (this repo)
   - `tap/Formula/lopen.rb` (this repo; keep it identical)

   The two files must stay byte-for-byte identical except for the tap copy's
   leading comment.

4. **Sanity-check** the formulae parse and (if `brew` is available) pass style:

   ```sh
   ruby -c Formula/lopen.rb
   ruby -c tap/Formula/lopen.rb
   brew style Formula/lopen.rb
   ```

5. **Publish the tap.** Copy `tap/Formula/lopen.rb` into the tap repo
   `github.com/jaxonwang/homebrew-tap` (at `Formula/lopen.rb`) and push:

   ```sh
   cp tap/Formula/lopen.rb   /path/to/homebrew-tap/Formula/lopen.rb
   cp tap/README.md          /path/to/homebrew-tap/README.md   # if changed
   cd /path/to/homebrew-tap && git commit -am "lopen vX.Y.Z" && git push
   ```

6. **Verify a clean install:**

   ```sh
   brew install jaxonwang/tap/lopen
   brew services start lopen
   lopen doctor
   ```

Until the tap repo exists, users can install straight from this repo's formula:

```sh
brew install https://raw.githubusercontent.com/jaxonwang/lopen/main/Formula/lopen.rb
```
