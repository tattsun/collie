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
- 画像添付（スマホのスクショ等。大きい画像は端末側で縮小してから送信）
- 要対応セッションのバッジ・通知（HTTPS 時）、PWA としてホーム画面に追加可能

## 必要なもの

- Ubuntu 26.04 以降
- [mise](https://mise.jdx.dev/)（Go / Node.js はプロジェクトの `mise.toml` で固定しているので、別途入れる必要なし）
- `git`
- 隔離実行（推奨）: `podman`（`deploy/setup-host.sh` が入れます）
- ホスト実行のプロジェクトを使う場合: サーバのユーザーでログイン済みの `claude` / `codex`

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
# （--serve なしなら :7878 でトークン認証）
```

エージェントのログイン（`collie` ユーザーで一度だけ）:

```bash
mise use -g npm:@openai/codex npm:@anthropic-ai/claude-code
codex login --device-auth             # 全プロジェクトのコンテナで共有されます
claude setup-token                    # 表示されたトークンを Collie の Project → Settings → Shared environment に
                                      # CLAUDE_CODE_OAUTH_TOKEN=… として保存
```

- サービスはそのユーザーとして動くので、`claude` / `codex` のログイン状態や git の認証情報をそのまま使います
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

- tailnet のユーザーは **トークン入力なし**で入れる（Tailscale のアイデンティティヘッダで認証。Funnel 経由のアクセスは対象外）
- ブラウザ通知・PWA・クリップボードなど HTTPS 必須の機能が使える

初回は `sudo tailscale set --operator=$USER` が必要な場合があります。

## 使い方

1. **Projects → New project**
   - *New folder*: `~/projects/<name>` を作成（git init 付き）
   - *Clone repo*: サーバ上で `git clone`（サーバユーザーの SSH 鍵/認証を使用）
   - *Existing folder*: サーバ上の既存ディレクトリを登録
2. **New** → やりたいことを書き、プロジェクト・エージェント（Claude Code / Codex）・権限モード・モデルを選んで開始
3. 一覧の **Needs you** に承認待ちが上がってくるので、タップして Allow / Deny
4. **Changes** で差分を確認、必要なら **Terminal** で `git push` など

## プロジェクトの隔離（コンテナ実行）

新しいプロジェクトは既定で **Isolated container** になります（Project → Settings で切替可）。

- プロジェクトごとに rootless Podman コンテナを1つ立て、そのプロジェクトのエージェント・ターミナルはすべてその中で動きます
- マウントされるのは **そのプロジェクトのディレクトリと、その worktree だけ**。Collie のデータ（トークン等）や他のプロジェクトは見えません。ファイルはサーバユーザーの所有のまま
- メモリ / CPU / プロセス数の上限（既定: 4GB / 半分のコア / 4096。プロジェクトごとに変更可）。上限の適用には systemd のユーザーセッションが必要です（`setup-host.sh` + `install.sh` 構成なら OK。そうでない環境では UI に警告が出ます）
- 暴走したら Project → Settings の **Kill now**（セッション画面のメニューからも可）。コンテナごと即停止し、会話は次のメッセージで再開できます
- しばらく使われないコンテナは自動で停止します
- 箱の中なので、Claude の **Bypass** や Codex の **Full access** で承認なしに任せる運用もしやすくなります
- エージェント用イメージ（Ubuntu 26.04 + Claude Code + Codex + mise + gh + Playwright/Chromium、約 3.4GB）は初回に自動ビルドされます。エージェントの更新はイメージ詳細の「Rebuild」または `collie --build-image`

### 共通プロファイル（全プロジェクト共通の設定）

ホストの `~/.claude` は渡しません（認証情報と全プロジェクトの会話履歴が入っているため）。代わりに `~/.collie/shared/` を全コンテナに読み取り専用で配ります:

```
~/.collie/shared/
  claude/CLAUDE.md  claude/skills/  claude/agents/  claude/commands/
  claude/mcp.json      ← 初期状態で Playwright MCP が有効
  claude/settings.json
  codex/AGENTS.md  codex/skills/  codex/config.toml   ← 同じく Playwright MCP
  gitconfig            ← コンテナ内の ~/.gitconfig
  env                  ← 全コンテナに渡す環境変数（CLAUDE_CODE_OAUTH_TOKEN など。UI からも編集可）
```

設定の優先順位は「共通プロファイル < リポジトリ内の `.claude/`・`.mcp.json`・`AGENTS.md`」。プロジェクト専用の秘密情報（そのリポジトリだけの `GH_TOKEN` など）は Project → Settings → Secrets に。

**メモリー（auto memory）**: エージェントのホームはプロジェクト単位なので、同じプロジェクトの全セッション（worktree セッションを含む）でメモリーが共有されます。既存プロジェクトをコンテナに切り替えると、ホスト側の Claude Code のメモリーと会話は初回に引き継がれます（Codex の会話は引き継がれません）。

### 権限モード

| エージェント | モード | 内容 |
|---|---|---|
| Claude | Ask first（default） | 編集・コマンド実行前に確認 |
| | Accept edits | ファイル編集は自動、コマンドは確認 |
| | Plan | 計画のみ。計画承認時に Accept edits / Ask first に切替 |
| | Auto | Claude Code の自動判定モード |
| | Bypass | すべて自動（注意） |
| Codex | Auto | workspace-write サンドボックス、外に出る時だけ確認 |
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

## セキュリティ

Collie はブラウザから**サーバ上で任意のコマンドを実行できる**ツールです。

- 公開インターネットには出さないでください（Tailscale / LAN 内での利用を想定）
- 既定でトークン認証が有効です。`--no-auth` は信頼できる環境でのみ
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
  worktrees/<project>/<session>/  Isolated worktree のチェックアウト
  homes/<project>/    隔離プロジェクトのコンテナ内ホーム（~/.claude・~/.codex・メモリー・会話）
  homes/<project>.env プロジェクト専用の環境変数（秘密情報）
  shared/             共通プロファイル
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
internal/sandbox/        プロジェクト単位の Podman コンテナ・エージェント用イメージ（image/Containerfile）
deploy/setup-host.sh     ホスト準備（専用ユーザー・podman・nftables）
web/                     Preact + Vite フロントエンド
deploy/install.sh        systemd ユーザーサービス導入
mise.toml                ツールチェーン（Go / Node）とビルドタスク
```
