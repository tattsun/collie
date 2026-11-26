# Collie 🐕

Ubuntu サーバにポコっと置ける、**Claude Code / Codex を複数同時に操るための Web UI**。
Tailscale 経由でスマホや別 PC からプロジェクトを作り、エージェントを走らせ、承認し、差分を確認できます。

- **単一バイナリ**（Go、UI 埋め込み・約 8MB）。依存はサーバ上の `claude` / `codex` / `git` だけ
- **軽量フロント**（Preact、初期ロード gzip ≈ 55KB）。ストリーミングはフレーム単位でまとめて描画、長い履歴は遅延描画
- **Claude Code**: `claude -p` の stream-json 制御プロトコル（Agent SDK と同じ）で駆動
- **Codex**: `codex app-server`（JSON-RPC）で駆動
- 承認（Allow / セッション中は許可 / Deny）、`AskUserQuestion` の選択式回答、Plan モードの計画承認、割り込み、権限モード切替、モデル切替
- 承認待ち中にメッセージを打つと「その指示付きで却下」として渡る
- 実行中にメッセージを送ると追加の指示として届く（Claude はキューイング、Codex は steer）
- プロセスはアイドル 30 分で自動停止し、次のメッセージで会話を**再開**（サーバ再起動後も同様）
- **Isolated worktree**: セッションごとに `collie/<id>` ブランチ＋専用チェックアウトを切り、同じリポジトリで並列作業しても衝突しない
- **Changes** タブ: git の差分（worktree セッションは分岐元からの全変更＋コミット一覧）
- **Terminal** タブ: ブラウザから PTY シェル（再接続でスクロールバック復元、スマホ用 Esc/Ctrl/矢印キーバー）
- **プロジェクトごとの隔離**: 各プロジェクトのエージェントとシェルを専用の rootless Podman コンテナで実行。見えるのはそのプロジェクトだけ、リソース上限つき、LAN/tailnet へは出られない。暴走したら「Kill now」で即停止
- **マルチユーザー**: パスキーでログイン、招待リンクでユーザー追加、管理者は複数可。各自のプロジェクト・会話は本人にしか見えず、エージェントは各自の Claude / Codex アカウントで動く
- **グループ**: グループのプロジェクトはメンバー全員で共有。セッションもグループに共有でき、メンバーが一緒に見て指示・承認できる
- **skill・MCP**: 全体共通（管理者）と個人用の 2 層。UI から編集でき、Playwright MCP は最初から有効
- **日本語 / English**: ブラウザの言語で自動選択。アカウント画面・ログイン画面で切り替え可能
- 画像添付（スマホのスクショ等。大きい画像は端末側で縮小してから送信）
- 要対応セッションのバッジ・通知（HTTPS 時）、PWA としてホーム画面に追加可能

## 必要なもの

- Ubuntu 26.04 以降
- [mise](https://mise.jdx.dev/)（Go / Node.js はプロジェクトの `mise.toml` で固定しているので、別途入れる必要なし）
- `git`
- 隔離実行（推奨）: `podman`（`deploy/setup-host.sh` が入れます）
- ホスト実行のプロジェクトを使う場合: サーバのユーザーでログイン済みの `claude` / `codex`（入れ方は「エージェントのアカウント」）

## ビルド

```bash
mise trust && mise install   # mise.toml の Go / Node を導入（初回のみ）
mise run build               # web/ をビルドして bin/collie を生成
```

その他のタスク（`mise tasks` で一覧）:

| タスク | 内容 |
|---|---|
| `mise run cross arm64` | 別アーキテクチャ向けにビルド（例: Raspberry Pi / ARM サーバ） |
| `mise run test` | Go のテスト・vet とフロントの型チェック |
| `mise run install [--serve]` | ビルドして systemd ユーザーサービスとして導入 |
| `mise run dev` | 開発用 API サーバ（認証なし・:7878） |

## 起動（お試し）

```bash
./bin/collie
# 2026/.. collie 0.1.0 listening on [::]:7878
# 2026/..   open: http://100.x.y.z:7878/?token=…   ← tailscale
```

ターミナルで起動すると、トークン付きの URL が表示されます。スマホで開けばそのデバイスはログイン済みになります（Cookie は約 1 年有効）。
トークンは `~/.collie/token` に保存され、`collie --print-token`（トークンのみ）や `collie --login-url`（ログイン用 URL）でいつでも表示できます。
systemd などで動かしているときは、トークンはログ（journal）に出しません。

## サーバに常駐させる

### 1. ホストの準備（root、初回のみ）

```bash
sudo ./deploy/setup-host.sh      # [--user collie] [--port 7878] [--allow 192.168.1.20/32 ...]
```

- `podman` などを導入し、Collie とエージェント専用のユーザー `collie` を作ります（あなた自身の `~/.ssh` や個人ファイルにはエージェントから触れなくなります）
- そのユーザーのプロセス（コンテナを含む）は **インターネットには出られるが、LAN・tailnet・サーバ自身のアドレス・Collie の API には接続できない** nftables ルールを入れます（`collie-firewall.service`）。NAS など許可したい宛先は `--allow` で追加
- lingering を有効にして、ログアウト後・再起動後も動くようにします

### 2. Collie の導入（`collie` ユーザーで）

```bash
sudo machinectl shell collie@         # systemd --user が使えるログインセッション
curl https://mise.run | sh            # ~/.bashrc で mise を有効化
git clone <this repo> && cd collie
mise trust && mise install
mise run install --serve              # ビルド → エージェント用イメージ作成 → サービス登録 → tailscale serve で HTTPS 公開（推奨）
```

### 3. 最初の管理者になる

1. 表示された `https://<マシン名>.<tailnet>.ts.net/` を開く（初回だけ Tailscale のログイン、またはトークン `collie --print-token` で入れます）
2. 画面上部の **Create passkey** でパスキーを作成 → あなたが最初の管理者になり、以後は**パスキーでしかログインできなく**なります（トークンも Tailscale だけのログインも無効）
3. Account → Agent accounts で自分の Claude / Codex を設定（下記）。他の人は Admin → Users → **Invite** のリンクで招待

### エージェントのアカウント（ユーザーごと）

エージェントは使う人それぞれのアカウントで動きます（課金も各自）。Account → **Agent accounts** から:

- **Claude Code**: 任意のプロジェクトの Terminal で `claude setup-token` → 表示されたトークンを貼り付けて保存
- **Codex**: 任意のプロジェクトの Terminal で `codex login --device-auth`（その人の全プロジェクトで有効）
- **GitHub**: 自分の環境変数に `GH_TOKEN=…` を入れると、push と private リポジトリの clone に使われます

（ホスト実行のプロジェクト＝管理者のみ、はサーバユーザーの `claude` / `codex` / git 認証を使います）

ホスト実行も使う場合は、`collie` ユーザーで CLI を入れてログインしておきます:

```bash
cd ~
mise use -g node@lts npm:@openai/codex 'npm:@anthropic-ai/claude-code[allow_builds=true]'
claude                                # 起動してログイン
codex login --device-auth
```

`allow_builds=true` は必須です。mise の npm バックエンドは既定で依存パッケージのインストールスクリプトを実行しないため、付けないと Claude Code のネイティブバイナリが配置されず、`claude` が `native binary not installed` で起動しません（入れてしまった場合は `~/.config/mise/config.toml` の該当行に `allow_builds = true` を足して `mise install -f npm:@anthropic-ai/claude-code`）。

- サービスは `collie` ユーザーとして動きます
- インストール時の `PATH` をユニットに書き込みます。mise を使っていれば shims を先頭に入れるので、mise で入れた CLI はアップグレード後も見つかり、各プロジェクトの `mise.toml` のバージョンもエージェントからそのまま使われます。PATH を変えたら再実行
- `loginctl enable-linger` でログアウト後・再起動後も動き続けます
- ログ: `journalctl --user -u collie -f`

### アップデート

```bash
cd collie && git pull
mise install
mise run install --serve      # 初回と同じオプションで（--serve なしで入れた場合はなし）
```

- サービスが再起動するので、実行中のエージェントは止まります（会話は次のメッセージで再開）。開いているブラウザは自動でリロードされます
- エージェント用イメージも必要に応じて作り直されます。Claude Code / Codex だけ最新にしたいときは `collie --build-image`
- 既存のプロジェクトは「ホスト実行」のまま残ります。Project → Settings で **Isolated container** に切り替えると、Claude のメモリーと会話はそのプロジェクトの分だけ引き継がれ、旧バージョンで作った worktree セッションもそのまま使えます（Codex の会話は新しいスレッドとして続きます）
- 自分のユーザーで動かしていたものを専用ユーザー構成に移す場合: `sudo ./deploy/setup-host.sh` → 旧サービスを `systemctl --user disable --now collie` で止める → `collie` ユーザーで導入し直し、プロジェクトは `collie` ユーザーが読み書きできる場所へ移して登録し直してください（専用ユーザーからは元ユーザーのホームは見えません）

### `--serve`（tailscale serve）を推奨する理由

`https://<マシン名>.<tailnet>.ts.net/` で HTTPS になり、

- **パスキーが使える**（パスキーは HTTPS のドメインに紐づくため、`http://100.x.x.x` では使えません）
- ブラウザ通知・PWA・クリップボードなど HTTPS 必須の機能が使える
- 最初のパスキーを作るまでは、tailnet のユーザーはトークンなしで入れます（初期設定用）

初回は `sudo tailscale set --operator=$USER` が必要な場合があります。

## 使い方

1. **Projects → New project**（Owner で「自分」か「グループ」を選択）
   - *New folder*: `~/projects/users/<ユーザー名>/<name>`（グループなら `~/projects/groups/<グループ名>/<name>`）を作成（git init 付き）
   - *Clone repo*: 管理者はサーバ上でサーバユーザーの認証を使って `git clone`。メンバーは `https://` のみで、clone は**本人のコンテナ内**で実行（エージェントと同じネットワーク制限。private な GitHub は自分の `GH_TOKEN`）
   - *Existing folder*（管理者のみ）: サーバ上の既存ディレクトリを登録
2. **New** → やりたいことを書き、プロジェクト・エージェント（Claude Code / Codex）・権限モード・モデルを選んで開始
3. 一覧の **Needs you** に承認待ちが上がってくるので、タップして Allow / Deny
4. **Changes** で差分を確認、必要なら **Terminal** で `git push` など

### Home（何でも相談・プロジェクトの取りまとめ）

サイドバー最上段の **Home** は、どのプロジェクトにも属さない会話の場所です。Home エージェントには、ふだんの相談のほか、プロジェクトをまたいだ仕事を頼めます。

- 「今週どのプロジェクトで何やった？」「sample2 の認証まわりどうなってる？」 — 自分のプロジェクト一覧・セッションの状態と会話履歴を見て、コードも読んで答えます
- 「sample2 でテスト直すセッション立てといて」 — プロジェクトにセッションを立てて指示したり、既存のセッションに追加の指示を送ったりします（Collie のツールを MCP で使います）
- 自分ではプロジェクトを編集しません（プロジェクトは読み取り専用で見えています）。編集は Home が立てたセッションが行い、その承認はこれまでどおり **Needs you** に届きます。Bypass / Full access のセッションは Home からは立てられません
- 見えるのは本人が見られるプロジェクトとセッションだけです。Home は本人専用のコンテナで動き、Collie とは本人の Home にだけ渡す UNIX ソケットで話します
- Home から送られたメッセージには、セッション画面で「Home エージェントが送信」と表示されます

## ユーザーとグループ

| | 管理者 | メンバー |
|---|---|---|
| 自分のプロジェクト・セッション | ✓ | ✓ |
| 他人の個人プロジェクト・会話を見る | ✗ | ✗ |
| グループを作る・メンバー管理（グループの owner として） | ✓ | ✓ |
| ユーザー招待・管理者の付与・無効化・削除 | ✓ | ✗ |
| 全コンテナの使用量確認・強制停止 | ✓（中身は見えない） | ✗ |
| 全体共通の skill・MCP・環境変数 | ✓ | ✗ |
| 既存フォルダの登録・ホスト実行・リソース上限の変更 | ✓ | ✗ |

- **パスキー**: 端末ごとに追加可能。別の端末を追加するときは Account → **Add another device** で 1 回限りのリンクを発行。ログインは 30 日有効（使うたびに延長）
- **パスキーを全部なくしたら**: 管理者が Admin → Users → **Passkey recovery link**。管理者自身ならサーバで `collie link`（`--user NAME` で他の人も）
- **グループのプロジェクト**: メンバー全員がファイル・Changes を共有。各メンバーのエージェントは**自分専用のコンテナ**で自分のアカウントで動き、Claude のプロジェクトメモリーはメンバー間で共有
- **共有セッション**: グループのプロジェクトで作ったセッションは既定でグループに共有（作成時やメニューで「Make private」可）。メンバーは会話を見て、指示・承認・中断ができます。エージェントは**セッションを始めた人のアカウント**で動きます（指示した人ではありません）。始めた人がグループを抜けると読み取り専用になります
- ターミナルは開いた本人にしか見えません

## プロジェクトの隔離（コンテナ実行）

新しいプロジェクトは既定で **Isolated container** になります（Project → Settings で切替可）。

- (プロジェクト, ユーザー) ごとに rootless Podman コンテナを1つ立て、その人のエージェント・ターミナルはすべてその中で動きます
- マウントされるのは **そのプロジェクトのディレクトリと、その worktree だけ**。Collie のデータ（トークン等）や他のプロジェクトは見えません。ファイルはサーバユーザーの所有のまま
- メモリ / CPU / プロセス数の上限（既定: 4GB / 半分のコア / 4096。プロジェクトごとに変更可）。上限の適用には systemd のユーザーセッションが必要です（`setup-host.sh` + `install.sh` 構成なら OK。そうでない環境では UI に警告が出ます）
- 暴走したら Project → Settings の **Kill now**（セッション画面のメニューからも可）。コンテナごと即停止し、会話は次のメッセージで再開できます
- しばらく使われないコンテナは自動で停止します
- 箱の中なので、Claude の **Bypass** や Codex の **Full access** で承認なしに任せる運用もしやすくなります
- エージェント用イメージ（Ubuntu 26.04 + Claude Code + Codex + mise + gh + Playwright/Chromium、約 3.4GB）は初回に自動ビルドされます。エージェントの更新はイメージ詳細の「Rebuild」または `collie --build-image`

### skill・MCP・指示（全体共通 + 個人）

ホストの `~/.claude` は渡しません（認証情報と全プロジェクトの会話履歴が入っているため）。代わりに 2 層のプロファイルを使います:

- **全体共通**（管理者が Admin → Global skills & MCP で編集、実体は `~/.collie/shared/`）
- **個人**（各自が Account → My skills & MCP で編集）

エージェント起動時に「全体 + 個人」を合成してコンテナへ読み取り専用で渡します:

| ファイル | 合成のしかた |
|---|---|
| `claude/CLAUDE.md`・`codex/AGENTS.md` | 全体の後に個人を追記 |
| `claude/skills/`・`agents/`・`commands/`・`codex/skills/` | 和集合（同名なら個人が優先） |
| `claude/mcp.json` | MCP サーバを合成（同名なら個人が優先）。初期状態で Playwright MCP が有効 |
| `claude/settings.json` | ディープマージ（個人が優先、配列は結合） |
| `codex/config.toml` | テーブル単位で合成（個人が優先） |
| `gitconfig` | 全体の後に個人（個人の `[user]` でコミット作者が各自になります） |

その上に、リポジトリ内の `.claude/`・`.mcp.json`・`AGENTS.md` が効きます。環境変数は「全体 < 個人 < プロジェクト」の順で上書き。

**メモリー（auto memory）**: 同じプロジェクトの自分のセッション（worktree セッションを含む）で共有されます。グループのプロジェクトではメンバー全員で共有されます。既存プロジェクトをコンテナに切り替えると、最初の管理者（サーバの持ち主）だけはホスト側の Claude Code のメモリーと会話が引き継がれます。

### 権限モード

| エージェント | モード | 内容 |
|---|---|---|
| Claude | Auto（default） | Claude Code の自動判定モード。プランやモデルが対応していなければ Claude Code が Ask first に戻します |
| | Ask first | 編集・コマンド実行前に確認 |
| | Accept edits | ファイル編集は自動、コマンドは確認 |
| | Plan | 計画のみ。計画承認時に Accept edits / Ask first に切替 |
| | Bypass | すべて自動（注意） |
| Codex | Auto（default） | workspace-write サンドボックス、外に出る時だけ確認 |
| | Untrusted | 信頼済み以外のコマンドは確認 |
| | Read only | 読み取りのみ |
| | Full access | サンドボックスなし・確認なし（注意） |

セッション中でも入力欄のモードチップから切り替えられます。

## オプション

| フラグ | 環境変数 | 既定値 | |
|---|---|---|---|
| `--addr` | `COLLIE_ADDR` | `:7878` | 待ち受けアドレス |
| `--root` | `COLLIE_ROOT` | `~/projects` | 新規プロジェクトの作成先 |
| `--data` | `COLLIE_DATA` | `~/.collie` | 状態・履歴・アップロード・worktree |
| `--token` | `COLLIE_TOKEN` | 自動生成 | アクセストークン |
| `--no-auth` | `COLLIE_NO_AUTH=1` | off | 認証なし（信頼できるネットワークのみ） |
| `--claude` / `--codex` | `COLLIE_CLAUDE` / `COLLIE_CODEX` | `claude` / `codex` | 実行ファイル |
| `--idle-timeout` | | `30m` | アイドルなエージェントプロセス / コンテナを止めるまでの時間（`0` で無効） |
| `--podman` | `COLLIE_PODMAN` | `podman` | Podman の実行ファイル |
| `--container-memory` | `COLLIE_CONTAINER_MEMORY` | `4g` | コンテナのメモリ上限の既定値 |
| `--container-cpus` | `COLLIE_CONTAINER_CPUS` | コア数の半分 | コンテナの CPU 上限の既定値 |
| `--container-pids` | | `4096` | コンテナのプロセス数上限の既定値 |
| `--trusted-proxy-uid` | | `0` | Tailscale のアイデンティティヘッダを信用するプロキシの uid（tailscaled = root） |
| `--build-image` | | | エージェント用イメージをビルド（エージェント更新）して終了 |
| `--public-url` | `COLLIE_PUBLIC_URL` | 最初の登録時のホスト | パスキーを紐づける URL（マシン名を変えたときなど） |

サブコマンド（サーバ上で `collie` ユーザーとして実行）:

| コマンド | |
|---|---|
| `collie link [--user NAME]` | パスキーを追加する 1 回限りのリンク（30 分有効）を表示。パスキーを失くしたときの復旧用 |
| `collie users` | ユーザー一覧 |
| `collie make-admin NAME` | 管理者にする |

## セキュリティ

Collie はブラウザから**サーバ上で任意のコマンドを実行できる**ツールです。

- 公開インターネットには出さないでください（Tailscale / LAN 内での利用を想定）
- ログインはパスキーのみ（最初のパスキーができるまでは、初期設定用にトークン / Tailscale のログインで入れます）。`--no-auth` は信頼できる環境でのみ
- 権限チェックはすべてサーバ側で行い、見えないものは「存在しない」として扱います（管理者も他人の会話は見られません）
- グループのプロジェクトは必ずコンテナ実行。ホスト実行のプロジェクトのエージェントを動かせる（指示・承認できる）のは管理者だけです
- プロジェクトのフォルダは互いに入れ子にできません（コンテナから他のプロジェクトが見えないように）
- メンバーはサーバ上の任意フォルダの登録・ホスト実行ができず、clone も `https://` のみで本人のコンテナ内で実行されます（サーバユーザーの SSH 鍵や git 認証は使われず、サーバのループバックや LAN にも届きません）
- API と WebSocket は同一オリジンからのリクエストのみ受け付けます（他サイトからのクロスサイト操作・WebSocket 乗っ取りを拒否）
- `tailscale serve` のアイデンティティヘッダは、接続元ソケットの所有者が tailscaled（root）の場合だけ信用します。同じマシン上の他プロセス（エージェントを含む）がヘッダを偽装しても通りません
- 隔離プロジェクトのコンテナは `--cap-drop all`・`no-new-privileges` で、ホストへの近道（loopback・`host.containers.internal`）を塞いで起動します。さらに `setup-host.sh` のファイアウォールで、サーバ自身のアドレス（LAN・tailnet・グローバル IP）と Collie のポートへの接続を拒否します
- エージェント出力の Markdown は生 HTML を無効化して描画し、さらに CSP でスクリプト実行を禁止しています
- プロジェクトを削除しても**ディスク上のファイルは削除しません**（Collie が作った worktree のみ削除。ブランチは残ります）

## データ

```
~/.collie/
  state.json          プロジェクト・セッション
  sessions/<id>.jsonl 各セッションのタイムライン
  uploads/            添付画像
  auth.json           ユーザー・パスキー・ログインセッション・招待・グループ
  worktrees/<project>/<session>/  Isolated worktree のチェックアウト
  homes/<project>/<user>/  コンテナ内ホーム（~/.claude・会話）
  homes/<project>.env プロジェクト専用の環境変数（秘密情報）
  users/<user>/       個人のプロファイル・合成済みプロファイル・環境変数・Codex（~/.codex）
  projmem/<project>/  グループプロジェクトで共有する Claude のメモリー
  shared/             全体共通のプロファイル・環境変数
  caps.json           エージェントのモデル一覧キャッシュ
  token               アクセストークン
```

会話そのものは各エージェント（`~/.claude/projects/…`、`~/.codex/sessions/…`）にも保存されており、Collie はそれを使って再開します。

## 開発

```bash
mise run dev                 # API（認証なし・127.0.0.1:7878）
cd web && npm run dev        # http://localhost:5173（/api を 7878 にプロキシ）
```

構成:

```
main.go                  起動・フラグ
internal/agent/          claude.go（stream-json）・codex.go（app-server JSON-RPC）
internal/session/        セッション実行管理・タイムライン・WebSocket 配信
internal/store/          state.json / JSONL 永続化
internal/server/         HTTP API・WebSocket・認証・静的配信
internal/term/           PTY ターミナル
internal/gitutil/        worktree・diff
internal/sandbox/        (プロジェクト, ユーザー) 単位の Podman コンテナ・プロファイル合成・エージェント用イメージ（image/Containerfile）
internal/auth/           ユーザー・パスキー（WebAuthn）・ログインセッション・招待・グループ
deploy/setup-host.sh     ホスト準備（専用ユーザー・podman・nftables）
web/                     Preact + Vite フロントエンド
deploy/install.sh        systemd ユーザーサービス導入
mise.toml                ツールチェーン（Go / Node）とビルドタスク
```
