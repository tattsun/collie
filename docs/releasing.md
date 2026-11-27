# Releasing

**English** | [日本語](releasing.ja.md) · [← README](../README.md)

For maintainers. Servers update only to releases signed with the project's key, and only the Release workflow signs.

## Once: the signing key

```bash
mise run release-key
git add internal/update/release.pub && git commit -m "Add the release signing key"
```

This puts the public key into the source and the private key into the repository secret `COLLIE_SIGNING_KEY`. `gh` must be allowed to write the repository's secrets. Keep the private key file it leaves somewhere offline, or delete it, and never commit it.

Builds without the public key can't verify releases. Install a build made after this commit by hand once.

## Each release

1. Tag a commit on main and push the tag. An agent can do this for you.

   ```bash
   git tag v0.4.0 origin/main && git push origin v0.4.0
   ```

2. Run the workflow yourself: Actions → **Release** → **Run workflow**, with the tag `v0.4.0`. This works from a phone browser too, or from a PC with `gh workflow run release.yml -f tag=v0.4.0`.

The workflow checks that the tag is on main, builds for amd64 and arm64, runs the tests, signs and publishes the release. A tag with a suffix such as `v0.4.0-rc.1` becomes a pre-release, which servers don't update to.

Step 2 is manual on purpose. If pushing a tag started the workflow, anyone or any agent with push access could publish a release that every server installs. See [Security](security.md#updates) for the token permissions this relies on.

## Gotchas

- In a private repository each run uses about ten minutes of GitHub Actions time. Once the month's included minutes are used up, runs don't start until the account has a payment method and an Actions budget above zero.
- To replace the key, add the new public key to `internal/update/release.pub` and publish one release signed with the old key. Then replace the secret.
