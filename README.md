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
- 画像添付（スマホのスクショ等。大きい画像は端末側で縮小してから送信）
- 要対応セッションのバッジ・通知（HTTPS 時）、PWA としてホーム画面に追加可能

## 必要なもの

- Linux（Ubuntu 22.04+ 推奨）
- `claude`（Claude Code）および/または `codex` CLI が**サーバのユーザーでログイン済み**であること
- `git`
- ビルドする場合: Go 1.22+ / Node.js 20+

## ビルド

```bash
make            # web/ をビルドして bin/collie を生成
# 別アーキテクチャ向け（例: Raspberry Pi / ARM サーバ）
make cross GOARCH=arm64
```

## 起動（お試し）

```bash
./bin/collie
# 2026/.. collie 0.1.0 listening on [::]:7878
# 2026/..   open: http://100.x.y.z:7878/?token=…   ← tailscale
```

表示された URL をスマホで開けばそのデバイスはログイン済みになります（Cookie は約 1 年有効）。
トークンは `~/.collie/token` に保存され、`collie --print-token` でいつでも表示できます。

## サーバに常駐させる（systemd ユーザーサービス）

```bash
make
./deploy/install.sh           # :7878 で待ち受け（トークン認証）
# あるいは
./deploy/install.sh --serve   # 127.0.0.1 で待ち受け、tailscale serve で HTTPS 公開（推奨）
```

- サービスはあなたのユーザーとして動くので、`claude` / `codex` のログイン状態や git の認証情報をそのまま使います
- インストール時の `PATH` をユニットに書き込みます（mise / nvm などで入れた CLI も見つかるように）。PATH を変えたら再実行
- `loginctl enable-linger` でログアウト後・再起動後も動き続けます
- ログ: `journalctl --user -u collie -f`

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
| `--idle-timeout` | | `30m` | アイドルなエージェントプロセスを止めるまでの時間（`0` で無効） |

## セキュリティ

Collie はブラウザから**サーバ上で任意のコマンドを実行できる**ツールです。

- 公開インターネットには出さないでください（Tailscale / LAN 内での利用を想定）
- 既定でトークン認証が有効です。`--no-auth` は信頼できる環境でのみ
- エージェント出力の Markdown は生 HTML を無効化して描画し、さらに CSP でスクリプト実行を禁止しています
- プロジェクトを削除しても**ディスク上のファイルは削除しません**（Collie が作った worktree のみ削除。ブランチは残ります）

## データ

```
~/.collie/
  state.json          プロジェクト・セッション
  sessions/<id>.jsonl 各セッションのタイムライン
  uploads/            添付画像
  worktrees/<id>/     Isolated worktree のチェックアウト
  caps.json           エージェントのモデル一覧キャッシュ
  token               アクセストークン
```

会話そのものは各エージェント（`~/.claude/projects/…`、`~/.codex/sessions/…`）にも保存されており、Collie はそれを使って再開します。

## 開発

```bash
go run . --no-auth --addr 127.0.0.1:7878   # API
cd web && npm run dev                      # http://localhost:5173（/api を 7878 にプロキシ）
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
web/                     Preact + Vite フロントエンド
deploy/install.sh        systemd ユーザーサービス導入
```
