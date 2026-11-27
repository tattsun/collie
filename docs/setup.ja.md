# セットアップ

[English](setup.md) | **日本語** · [← README](../README.ja.md)

Linux サーバで、Collie を専用ユーザーの常駐サービスとして HTTPS で動かす手順です。パスキー・通知・PWA はどれも HTTPS が必要です。Mac で動かす場合は [Mac で動かす](#mac-で動かす) を見てください。

サーバには systemd と Podman 5 以降が必要です。`setup-host.sh` は apt・dnf・zypper・pacman でパッケージを入れるので、Debian 13 以降、Ubuntu 26.04 以降、Fedora、RHEL 9/10 とその互換ディストリ（AlmaLinux、Rocky）、openSUSE、Arch で使えます。それ以外では、自分で入れるものの一覧を表示します。

まず、どうやってアクセスしてもらうかを決めます:

| | 開くアドレス | 必要なもの |
|---|---|---|
| **独自ドメイン** | `https://collie.example.com`（どこからでも） | ドメインと、80・443 番ポートをインターネットに開けたサーバ。証明書は Collie が Let's Encrypt から自分で取ります |
| **手持ちのリバースプロキシ** | Caddy・nginx・Cloudflare Tunnel などで公開しているアドレス | 同じマシン上で、すでに HTTPS を出しているプロキシ |
| **Tailscale** | tailnet 内なら `https://<マシン名>.<tailnet>.ts.net`。Funnel を使えばどこからでも | サーバと、使うすべての端末に Tailscale（Funnel なしの場合） |

最初のパスキーを作る前に決めてください。パスキーは作ったときのアドレスでしか使えません（[別のアドレスに移す](#別のアドレスに移す)）。

## 1. ホストの準備（初回のみ、sudo で）

```bash
sudo ./deploy/setup-host.sh            # 独自ドメインなら --public を付ける
```

Podman を入れ、Collie と全エージェントを動かす専用ユーザー `collie` を作ります。エージェントからあなた自身の `~/.ssh` や個人ファイルには触れなくなります。あわせてファイアウォールのルールも入ります。このユーザーのプロセスはインターネットには出られますが、LAN・tailnet・このサーバ自身には接続できません。NAS などに接続させたいときは `--allow 192.168.1.20/32` のように指定します。オプションの一覧は `--help` で確認できます。

`--public` を付けると、root ではない Collie が 80・443 番ポートで待ち受けられるようになります。このマシンのどのユーザーも 80 番以上のポートで待ち受けられるようになるので、Collie 用のサーバで使ってください。ufw か firewalld が有効なら 80・443 番も開けます。クラウドのファイアウォール（セキュリティグループなど）は自分で開けてください。

## 2. Collie の導入（`collie` ユーザーで）

```bash
sudo machinectl shell collie@        # systemd --user が使えるログインセッション
curl https://mise.run | sh           # そのあと ~/.bashrc で mise を有効化
git clone <this repo> && cd collie
mise trust && mise install
mise run install --domain collie.example.com     # または下の表のどれか
```

| | |
|---|---|
| `--domain collie.example.com` | 独自ドメイン |
| `--proxy https://collie.example.com` | 手持ちのリバースプロキシの後ろ（そのアドレスで公開） |
| `--serve` | Tailscale、tailnet 内だけ |
| `--funnel` | Tailscale、tailnet とインターネット |

Collie とエージェント用のコンテナイメージをビルドし、systemd ユーザーサービスとして登録します。イメージの土台（Ubuntu のパッケージ・Podman・mise）は、手元のソースに対応するものが GHCR に公開されていればそれをダウンロードし、なければサーバでビルドします。どちらの場合も初回は数分かかります。

ログ: `journalctl --user -u collie -f`

### 独自ドメイン

最初に Collie を開く前に、ドメインの DNS をこのサーバに向け、80・443 番ポートを開けておいてください。証明書は最初のアクセスで取得します。Let's Encrypt は何度か失敗するとしばらく発行してくれなくなります。80 番は HTTPS へのリダイレクトと Let's Encrypt への応答にだけ使います。

### 手持ちのリバースプロキシ

同じマシンで動かし、WebSocket も含めて `http://127.0.0.1:7878` に転送します。接続元のアドレスは `X-Forwarded-For` で渡してください。Collie はログインの試行回数をアドレスごとに制限するので、渡さないと全員が 1 つの枠を分け合うことになります。Caddy ならこれだけです:

```
collie.example.com {
	reverse_proxy 127.0.0.1:7878
}
```

nginx の場合:

```nginx
location / {
    proxy_pass http://127.0.0.1:7878;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
}
```

Cloudflare Tunnel なら、公開するホスト名の転送先を `http://localhost:7878` にします。

### Tailscale Funnel

初回は `--funnel` が、tailnet で Funnel を許可するためのリンクを表示します。インターネットから外すときは `mise run install --serve` を実行し、`tailscale funnel status` に出てこないことを確認してください。

## 3. 最初の管理者になる

1. インストーラが表示したリンクを開きます。リンクには初期設定用のトークンが入っています。もう一度見たいときは `collie` ユーザーで `collie --print-token` を実行します。パスキーができるまでは、このトークンが唯一の入口です。tailnet や LAN の中にいるだけではログインできません。
2. **Create passkey** を押します。あなたが最初の管理者になり、以後のログインはパスキーだけになります。トークンは使えなくなります。
3. Account → **Agent accounts** で自分の Claude Code / Codex をつなぎます。手順は画面に表示されます。Claude のアカウントは複数登録でき（サブスク・Amazon Bedrock・API キー・カスタム設定）、Project → Settings → **Claude account** でプロジェクトごとに選べます。
4. ほかの人は Admin → Users → **Invite** から招待します。エージェントは各自のアカウントで動くので、利用料もそれぞれの負担です。

## アップデート

Collie は GitHub の新しいリリースを確認します。リリースが出ると、管理者の画面のサイドバー下部に `vX → vY` と表示され、Admin → **Updates** のボタン 1 つで更新できます。同じ画面で **アップデートを自動でインストール** をオンにすると、エージェントが動いておらず、ターミナルも使われていない状態が 10 分続いたところで自動的に更新します。

- サービスが再起動します。実行中のエージェントは止まり、次のメッセージで会話の続きから再開します。開いているブラウザは自動でリロードされます。
- 新しいバージョンが起動しなかったときは、元のバージョンとデータに戻し、何が起きたかを Updates の画面に表示します。失敗したバージョンを自動でもう一度試すことはありません。
- 自分で更新できるのは Linux で、`mise run install` で入れた systemd のユーザーサービスとして動いている場合だけです。この機能より前のビルドで動いているサーバは、一度だけ下の手順で手動で更新してください。

ソースから更新する場合:

```bash
cd collie && git pull
mise install
mise run install --domain collie.example.com    # 初回と同じオプションで
```

イメージの中の Claude Code / Codex だけを更新したいときは、Admin → Containers → **Collie standard** → **Details** → **Rebuild** か、`collie --build-image` を実行します。入れ直すのはエージェントだけで、土台はそのまま残り、Collie の更新と一緒に新しくなります。管理者が追加したエージェント用イメージも、その上に作り直されます。各プロジェクトのコンテナは、中で何も使われていないときに次に再起動したタイミングで、新しいイメージに切り替わります。新しいイメージでの最初の起動には数分かかることがあり（Podman がコンテナのユーザー ID 用にイメージのファイルをコピーするため）、その間セッションにお知らせが表示されます。

## パスキーをなくしたら

- ほかのユーザー: Admin → Users → **Passkey recovery link**
- 自分（管理者）: サーバで `collie` ユーザーとして `collie link` を実行します。`--user NAME` を付ければ、ほかの人のリンクも発行できます。

## 別のアドレスに移す

パスキーは作ったときのアドレスでしか使えません。Tailscale から独自ドメインに移すときなどは:

1. 新しいオプションでインストールし直します（手順 2）。
2. `collie` ユーザーで `collie reset-passkeys --yes` を実行します。全員のパスキーが消え、全員ログアウトされます。
3. 新しいアドレスを開き、初期設定用のトークン（`collie --print-token`）でログインして、すぐにパスキーを作ってください。作るまでは、トークンが唯一の鍵です。
4. ほかの人には、パスキーを追加するリンクを渡します: Admin → Users → **Passkey recovery link**。

`--serve` から `--funnel` に変える場合はアドレスが変わらないので、パスキーはそのまま使えます。

## ホスト実行のプロジェクト（任意）

管理者は、プロジェクトをコンテナではなくホスト上で動かす設定にできます（Project → Settings）。このときエージェントは `collie` ユーザーの CLI とログイン情報を使うので、`collie` ユーザーで CLI を入れてログインしておきます:

```bash
mise use -g node@lts npm:@openai/codex 'npm:@anthropic-ai/claude-code[allow_builds=true]'
claude                      # 一度起動してログイン
codex login --device-auth
```

`allow_builds=true` は外さないでください。付けないと mise が Claude Code のインストールスクリプトを実行せず、`claude` が `native binary not installed` で起動しません。付けずに入れてしまった場合は、`~/.config/mise/config.toml` の該当行に `allow_builds = true` を足して `mise install -f npm:@anthropic-ai/claude-code` を実行します。

サービスはインストール時の `PATH` を使います（mise の shims が先頭）。`PATH` を変えたら `mise run install` をもう一度実行してください。

## Mac で動かす

Mac では、Collie はあなた自身のユーザーの LaunchAgent として動き、エージェントは Mac の上で直接、あなたの `claude` / `codex` のログインのまま動きます。Mac ではまだ隔離コンテナが使えません。コンテナの外でエージェントを動かせるのは管理者だけなので、Mac の Collie はあなた専用です（招待した人はログインはできても、何も始められません）。

```bash
xcode-select --install               # git（入っていなければ）
curl https://mise.run | sh           # そのあと ~/.zshrc で mise を有効化
git clone <this repo> && cd collie
mise trust && mise install
mise run install --serve             # または --domain / --proxy / --funnel（上と同じ）
```

CLI のインストールとログインは [ホスト実行のプロジェクト](#ホスト実行のプロジェクト任意) と同じです。そのあと手順 3 のとおり最初の管理者になります。

- Collie は Mac にログインすると起動し、ログインしている間だけ動きます。置きっぱなしで使う Mac なら、自動ログインをオンにして、スリープしないようにしてください（システム設定 → エネルギー: 自動スリープを防ぐ）。
- プロジェクトはデスクトップ・書類・ダウンロードの外に置いてください。macOS は、フルディスクアクセスを与えない限り、バックグラウンドのプロセスからこれらのフォルダを読ませません。
- `--domain` の場合は、ルータの 80・443 番を Mac に転送します。macOS のファイアウォールがオンなら、聞かれたときに `collie` への接続を許可してください。
- ログ: `tail -f ~/Library/Logs/collie.log`。再起動: `launchctl kickstart -k gui/$(id -u)/io.github.tattsun.collie`
- Mac では Collie は自分で更新できません。上のとおりソースから更新してください。
- セキュリティ上どういうことになるかは[セキュリティ](security.ja.md#守られていないもの)を見てください。
