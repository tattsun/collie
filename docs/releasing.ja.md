# リリースの作り方

[English](releasing.md) | **日本語** · [← README](../README.ja.md)

メンテナ向けです。サーバが更新に使うのは、プロジェクトの鍵で署名されたリリースだけです。署名するのは Release ワークフローだけです。

## 最初に 1 回: 署名鍵

```bash
mise run release-key
git add internal/update/release.pub && git commit -m "Add the release signing key"
```

公開鍵がソースに入り、秘密鍵がリポジトリの secret `COLLIE_SIGNING_KEY` に入ります。`gh` に、リポジトリの secret を書き込める権限が必要です。手元に残る秘密鍵のファイルは、オフラインの場所に保管するか削除してください。コミットはしないでください。

公開鍵の入っていないビルドはリリースを検証できません。このコミットより後のビルドを、一度だけ手動で導入してください。

## リリースのたびに

1. main 上のコミットにタグを打って push します。エージェントに頼んでも構いません。

   ```bash
   git tag v0.4.0 origin/main && git push origin v0.4.0
   ```

2. ワークフローは自分で実行します。Actions → **Release** → **Run workflow** で、タグに `v0.4.0` を入れて実行します。スマホのブラウザからでもでき、PC なら `gh workflow run release.yml -f tag=v0.4.0` でも実行できます。

ワークフローは、タグが main 上にあるかを確かめてから、amd64 と arm64 のビルド、テスト、署名、リリースの公開まで行います。`internal/sandbox/image/base/Containerfile` が変わっていれば、エージェント用イメージの土台を GHCR（`ghcr.io/<owner>/collie-agent-base`）に公開し、リリースに固定します。このファイルを変えずに Ubuntu の更新を取り込みたいときは、実行時に **Rebuild the agent base image** にチェックを入れます。`v0.4.0-rc.1` のような接尾辞つきのタグはプレリリースになり、サーバはそこへは更新しません。

2 をわざと手動にしています。タグの push でワークフローが動く作りにすると、push できる人やエージェントなら誰でも、全サーバに入るリリースを出せてしまいます。これが前提にしているトークンの権限については、[セキュリティ](security.ja.md#アップデート)を見てください。

## 注意点

- GHCR は、`collie-agent-base` パッケージを初回に非公開で作ることがあります。その状態ではサーバが pull できないので、パッケージの設定で公開に変えてください。
- 鍵を替えるときは、新しい公開鍵を `internal/update/release.pub` に追記し、古い鍵で署名したリリースを 1 回出してから、secret を差し替えます。
