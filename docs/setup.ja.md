# セットアップ

[English](setup.md) | **日本語** · [← README](../README.ja.md)

Ubuntu 26.04 以降のサーバで、Collie を専用ユーザーの常駐サービスとして動かし、tailnet 内に HTTPS で公開する手順です。

## 1. ホストの準備（初回のみ、sudo で）

```bash
sudo ./deploy/setup-host.sh
```

Podman を入れ、Collie と全エージェントを動かす専用ユーザー `collie` を作ります。エージェントからあなた自身の `~/.ssh` や個人ファイルには触れなくなります。あわせてファイアウォールのルールも入ります。このユーザーのプロセスはインターネットには出られますが、LAN・tailnet・このサーバ自身には接続できません。NAS などに接続させたいときは `--allow 192.168.1.20/32` のように指定します。オプションの一覧は `--help` で確認できます。

## 2. Collie の導入（`collie` ユーザーで）

```bash
sudo machinectl shell collie@        # systemd --user が使えるログインセッション
curl https://mise.run | sh           # そのあと ~/.bashrc で mise を有効化
git clone <this repo> && cd collie
mise trust && mise install
mise run install --serve
```

Collie とエージェント用のコンテナイメージをビルドし（イメージの初回ビルドは数分かかります）、systemd ユーザーサービスとして登録します。`--serve` を付けると `tailscale serve` で `https://<マシン名>.<tailnet>.ts.net/` に公開されます。パスキー・通知・PWA はどれも HTTPS が必要なので、付けておくのがおすすめです。

ログ: `journalctl --user -u collie -f`

## 3. 最初の管理者になる

1. インストーラが表示した URL を開きます。パスキーができるまでは、tailnet 内からならトークンなしで入れます。
2. **Create passkey** を押します。あなたが最初の管理者になり、以後のログインはパスキーだけになります。
3. Account → **Agent accounts** で自分の Claude Code / Codex をつなぎます。手順は画面に表示されます。Claude のアカウントは複数登録でき（サブスク・Amazon Bedrock・API キー・カスタム設定）、Project → Settings → **Claude account** でプロジェクトごとに選べます。
4. ほかの人は Admin → Users → **Invite** から招待します。エージェントは各自のアカウントで動くので、利用料もそれぞれの負担です。

## アップデート

```bash
cd collie && git pull
mise install
mise run install --serve    # 初回と同じオプションで
```

サービスが再起動します。実行中のエージェントは止まり、次のメッセージで会話の続きから再開します。開いているブラウザは自動でリロードされます。イメージの中の Claude Code / Codex だけを更新したいときは `collie --build-image` を実行します。

## パスキーをなくしたら

- ほかのユーザー: Admin → Users → **Passkey recovery link**
- 自分（管理者）: サーバで `collie` ユーザーとして `collie link` を実行します。`--user NAME` を付ければ、ほかの人のリンクも発行できます。

## ホスト実行のプロジェクト（任意）

管理者は、プロジェクトをコンテナではなくホスト上で動かす設定にできます（Project → Settings）。このときエージェントは `collie` ユーザーの CLI とログイン情報を使うので、`collie` ユーザーで CLI を入れてログインしておきます:

```bash
mise use -g node@lts npm:@openai/codex 'npm:@anthropic-ai/claude-code[allow_builds=true]'
claude                      # 一度起動してログイン
codex login --device-auth
```

`allow_builds=true` は外さないでください。付けないと mise が Claude Code のインストールスクリプトを実行せず、`claude` が `native binary not installed` で起動しません。付けずに入れてしまった場合は、`~/.config/mise/config.toml` の該当行に `allow_builds = true` を足して `mise install -f npm:@anthropic-ai/claude-code` を実行します。

サービスはインストール時の `PATH` を使います（mise の shims が先頭）。`PATH` を変えたら `mise run install` をもう一度実行してください。
