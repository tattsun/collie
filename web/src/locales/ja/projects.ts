// Projects, project settings, new session page and sidebar.
const ja: Record<string, string> = {
  // ---- ProjectSettings: agent image
  'Agent image': 'エージェント用イメージ',
  Image: 'イメージ',
  Status: '状態',
  'Building…': 'ビルド中…',
  'Build failed': 'ビルド失敗',
  Ready: '待機中',
  Built: '作成済み',
  'Not built yet': '未ビルド',
  'Rebuild (update agents)': '再ビルド（エージェントを更新）',
  'Building agent image…': 'エージェント用イメージをビルド中…',
  'Image build failed': 'イメージのビルドに失敗しました',

  // ---- ProjectSettings: where agents run
  'Agents in this project are working. Saving restarts them in the new environment. Continue?':
    'このプロジェクトのエージェントが作業中です。保存すると新しい環境で再起動します。続行しますか？',
  'Saved — agents use the new settings on their next message': '保存しました。エージェントは次のメッセージから新しい設定を使います',
  'Kill everything running in “{name}” right now (all members)?\n\nAgents and shells are terminated immediately. Conversations can be resumed afterwards.':
    '「{name}」で実行中のものをすべて今すぐ強制停止しますか？（全メンバー対象）\n\nエージェントとシェルは即座に終了します。会話は後から再開できます。',
  Killed: '強制停止しました',
  Stopped: '停止しました',
  'Where agents run': 'エージェントの実行場所',
  'Isolated container': '隔離コンテナ',
  'Directly on the server': 'ホスト実行',
  'Each member’s agents and terminals run in their own rootless container for this project: only this folder is visible, with resource limits and no access to your LAN or tailnet.':
    '各メンバーのエージェントとターミナルは、このプロジェクト専用の rootless コンテナで動きます。見えるのはこのフォルダだけで、リソース上限が適用され、LAN や tailnet にはアクセスできません。',
  'Agents run as the server user with full access to its files and network. Use only for projects you trust.':
    'エージェントはサーバのユーザーとして動き、そのファイルとネットワークにフルアクセスできます。信頼できるプロジェクトでのみ使ってください。',
  'Resource limits are not enforced on this server: Collie is not running under a systemd user session (see deploy/install.sh).':
    'このサーバではリソース上限が適用されません。Collie が systemd のユーザーセッション下で動いていません（deploy/install.sh を参照）。',
  Memory: 'メモリ',
  CPUs: 'CPU',
  Processes: 'プロセス数',
  '{value} (default)': '{value}（デフォルト）',
  none: 'なし',
  Save: '保存',
  'Isolated: your agents run in your own container for this project (limits {memory} memory, {cpus} CPUs).':
    '隔離: あなたのエージェントは、このプロジェクト用の自分専用コンテナで動きます（リソース上限: メモリ {memory}、CPU {cpus}）。',
  'Runs directly on the server.': 'サーバ上で直接動きます。',
  'Admins manage this setting.': 'この設定は管理者が管理します。',

  // ---- ProjectSettings: containers
  Containers: 'コンテナ',
  'None yet — they start with the first agent or terminal.': 'まだありません。最初のエージェントまたはターミナルの起動時に開始します。',
  You: '自分',
  '{n} procs': '{n} プロセス',
  stopped: '停止中',
  'Limits are configured but not enforced on this host.': 'リソース上限は設定されていますが、このホストでは適用されていません。',
  'Image: {status}': 'イメージ: {status}',
  'building…': 'ビルド中…',
  'build failed': 'ビルド失敗',
  ready: '準備完了',
  'built on first use': '初回使用時にビルド',
  details: '詳細',
  'Stop everything': 'すべて停止',
  'Kill now': '今すぐ強制停止',
  'Containers also stop by themselves when nothing has used them for a while.': 'コンテナはしばらく使われないと自動的に停止します。',

  // ---- ProjectSettings: secrets, skills & MCP
  'Project secrets': 'プロジェクトのシークレット',
  'KEY=VALUE lines exported in this project’s containers (for every member) — e.g. {code} scoped to this repository.':
    'このプロジェクトのコンテナで export される KEY=VALUE 形式の行です（全メンバー共通）。例: このリポジトリ専用の {code}。',
  'KEY=VALUE lines exported in this project’s containers — e.g. {code} scoped to this repository.':
    'このプロジェクトのコンテナで export される KEY=VALUE 形式の行です。例: このリポジトリ専用の {code}。',
  'Saved — applies to agents and shells started from now on': '保存しました。これから起動するエージェントとシェルに反映されます',
  'Skills & MCP': 'skill・MCP',
  'Agents get the global profile (Admin → Global skills & MCP) plus your own ({link}), and whatever the repository itself contains ({dir}, {mcp}, {agents}).':
    'エージェントには、グローバルプロファイル（管理 → グローバル skill・MCP）と自分のプロファイル（{link}）、さらにリポジトリ自体に含まれる設定（{dir}、{mcp}、{agents}）が適用されます。',
  'Agents get the global profile plus your own ({link}), and whatever the repository itself contains ({dir}, {mcp}, {agents}).':
    'エージェントには、グローバルプロファイルと自分のプロファイル（{link}）、さらにリポジトリ自体に含まれる設定（{dir}、{mcp}、{agents}）が適用されます。',
  'Account → My skills & MCP': 'アカウント → 自分の skill・MCP',

  // ---- Projects: new project form
  'New folder': '新規フォルダ',
  'Clone repo': 'リポジトリをclone',
  'Existing folder': '既存フォルダ',
  'Repository URL': 'リポジトリの URL',
  "Cloned with the server user's git credentials.": 'サーバのユーザーの git 認証情報で clone します。',
  'https:// only; private GitHub repositories use the GH_TOKEN in your account environment.':
    'https:// のみ対応です。private な GitHub リポジトリには、アカウントの環境変数の GH_TOKEN が使われます。',
  'Path on the server': 'サーバ上のパス',
  '{home}/my-app or /srv/code/app': '{home}/my-app または /srv/code/app',
  Name: '名前',
  '(optional)': '（任意）',
  'derived from the URL': 'URL から自動設定',
  'Initialize a git repository': 'git リポジトリを初期化',
  Owner: '所有者',
  'Me (personal)': '自分（個人）',
  'Group: {name}': 'グループ: {name}',
  'Shared with every member of the group.': 'グループの全メンバーと共有されます。',
  'Run agents in an isolated container (recommended)': 'エージェントを隔離コンテナで実行（推奨）',
  'Its own sandbox with resource limits, only this folder visible, no LAN/tailnet access.':
    'リソース上限付きの専用サンドボックスです。見えるのはこのフォルダだけで、LAN/tailnet にはアクセスできません。',
  'Needs podman on the server.': 'サーバに Podman が必要です。',
  Clone: 'clone',
  'Add project': 'プロジェクトを追加',
  'Create project': 'プロジェクトを作成',

  // ---- Projects: list and project page
  Back: '戻る',
  Projects: 'プロジェクト',
  'New project': '新規プロジェクト',
  'Project not found.': 'プロジェクトが見つかりません。',
  'All projects': 'すべてのプロジェクト',
  'Remove project “{name}” from Collie?\n\nIts {n} session(s) are deleted. Files in {path} are NOT touched.':
    'プロジェクト「{name}」を Collie から削除しますか？\n\nこのプロジェクトのセッション {n} 件は削除されます。{path} 内のファイルには一切手を加えません。',
  isolated: '隔離',
  host: 'ホスト実行',
  cloning: 'clone 中',
  Session: 'セッション',
  Sessions: 'セッション',
  Changes: '変更',
  Terminal: 'ターミナル',
  Settings: '設定',
  'No sessions in this project yet.': 'このプロジェクトにはまだセッションがありません。',
  'Remove project': 'プロジェクトを削除',

  // ---- NewSession
  'Welcome to Collie': 'Collie へようこそ',
  'Create a project, then start Claude Code or Codex sessions on it from any device.':
    'プロジェクトを作成すると、どの端末からでも Claude Code や Codex のセッションを始められます。',
  'Create a project': 'プロジェクトを作成',
  'What should we work on?': '何に取り組みますか？',
  'Describe a task for {agent}…': '{agent} に頼むタスクを入力…',
  'Attach image': '画像を添付',
  'Start session': 'セッションを開始',
  Project: 'プロジェクト',
  'Claude Code plugins installed for every member’s agents in this project. Plugins enabled in the repository’s {file} are installed too.':
    'このプロジェクトで、メンバー全員のエージェントに入れる Claude Code のプラグインです。リポジトリの {file} で有効にしたプラグインも入ります。',
  'Claude Code plugins installed for your agents in this project. Plugins enabled in the repository’s {file} are installed too.':
    'このプロジェクトでエージェントに入れる Claude Code のプラグインです。リポジトリの {file} で有効にしたプラグインも入ります。',
  'What your agents get here': 'このプロジェクトで自分のエージェントに入るもの',
  'Installed into your container before Claude starts; changes apply to agents started from now on.': 'Claude の起動前に自分のコンテナへインストールします。変更は、これから起動するエージェントに反映されます。',
  '{name} (cloning…)': '{name}（clone 中…）',
  'Share with {name}': '{name} に共有',
  'Share with the group': 'グループに共有',
  'Members can follow and drive this session. It runs with your agent accounts.':
    'メンバーがこのセッションを閲覧・操作できます。実行にはあなたのエージェントのアカウントが使われます。',
  Agent: 'エージェント',
  "`{agent}` was not found on the server's PATH.": 'サーバの PATH に `{agent}` が見つかりません。',
  Permissions: '権限モード',
  Model: 'モデル',
  Default: 'デフォルト',
  Effort: '推論レベル',
  'Isolated worktree': '隔離 worktree',
  'Work on a new branch in its own checkout, so parallel sessions never collide.':
    '専用のチェックアウトで新しいブランチを作って作業するため、並行するセッション同士が衝突しません。',
  'Requires a git repository.': 'git リポジトリが必要です。',
  'collie/<id> from {base}': '{base} から collie/<id> を作成',
  Personal: '個人',
  Group: 'グループ',

  // ---- Sidebar
  'Needs your input': '入力待ち',
  'Needs you · {n}': '要対応 · {n}',
  'Create a project to get started.': 'まずはプロジェクトを作成しましょう。',
  'No sessions yet.': 'まだセッションがありません。',
  'New session': '新規セッション',
  'Hide archived ({n})': 'アーカイブを隠す（{n}）',
  'Show archived ({n})': 'アーカイブを表示（{n}）',
  '{busy} active · {n}': '{busy} 件稼働中 · {n}',
  online: 'オンライン',
  offline: 'オフライン',
  connecting: '接続中…',
  New: '新規',
  'Account menu': 'アカウントメニュー',
  '{name} — account': '{name} — アカウント',
  Groups: 'グループ',
  Admin: '管理',
  'Sign out': 'ログアウト',
  Notifications: '通知',
  'Notifications on': '通知オン',
  'Notifications off': '通知オフ',
}
export default ja
