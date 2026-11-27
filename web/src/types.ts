export type Agent = 'claude' | 'codex'

export interface Project {
  id: string
  name: string
  path: string
  createdAt: number
  ownerUser?: string
  ownerGroup?: string
  createdBy?: string
  runtime?: '' | 'host' | 'container'
  memory?: string
  cpus?: string
  pids?: number
  /** the containers may run Podman inside (admins decide) */
  nested?: boolean
  /** what the containers can connect to: '' the internet, 'allowlist' only netAllow's hosts */
  network?: '' | 'allowlist'
  netAllow?: string[]
  /** agent image: '' = the server's default, 'standard' = Collie's, else a custom image's id */
  image?: string
  git: boolean
  status?: '' | 'cloning' | 'error'
  error?: string
  /** the user's Home: chats outside any project, coordinating the projects */
  home?: boolean
}

/** Release updates (admins only). */
export interface UpdateStatus {
  current: string
  enabled: boolean
  repo?: string
  latest?: string
  notes?: string
  url?: string
  published?: number
  available: boolean
  canApply: boolean
  why?: string
  devBuild?: boolean
  auto: boolean
  checkedAt?: number
  checkError?: string
  phase?: '' | 'downloading' | 'preparing' | 'waiting' | 'restarting'
  error?: string
  busy: number
  last?: { from: string; to: string; ok: boolean; error?: string; at: number }
}

export interface SandboxStatus {
  available: boolean
  limits?: boolean
  image?: string
  ready?: boolean
  building?: boolean
  error?: string
  log?: string
  shared?: string
  /** admin-defined images on top of Collie's standard one */
  images?: CustomImageStatus[]
  /** the image projects run unless they pick one ('' = standard) */
  defaultImage?: string
  memory?: string
  cpus?: string
  pids?: number
  /** always allowed on a network allowlist (the agents' own services) */
  netAgents?: string[]
  /** a starting list for a new allowlist */
  netSuggested?: string[]
}

/** A destination a project's containers asked for recently. */
export interface NetEntry {
  host: string
  port: number
  /** why it was refused: not on the list, or at a private / local address */
  blocked?: 'list' | 'address'
  count: number
  last: number
}

export interface CustomImageStatus {
  id: string
  name: string
  /** a build exists that projects can run */
  ready: boolean
  /** ...built from the current definition and standard image */
  current: boolean
  building?: boolean
  error?: string
  log?: string
}

/** A custom image's definition (admins). */
export interface CustomImageDef {
  id: string
  name: string
  containerfile: string
  updatedAt?: number
}

export interface ContainerInfo {
  userId?: string
  projectId?: string
  running?: boolean
  exists?: boolean
  mem?: string
  cpu?: string
  pids?: string
  limitsEnforced?: boolean
}

export type Status = 'starting' | 'running' | 'waiting' | 'idle' | 'stopped' | 'error'

export interface Session {
  id: string
  projectId: string
  agent: Agent
  title: string
  model?: string
  effort?: string
  mode: string
  cwd: string
  worktree?: boolean
  branch?: string
  base?: string
  agentRef?: string
  status: Status
  unread?: boolean
  preview?: string
  costUsd?: number
  ctxUsed?: number
  ctxMax?: number
  archived?: boolean
  createdAt: number
  updatedAt: number
  owner?: string
  shared?: boolean
  /** work the agent has running in the background, past the end of its turn */
  tasks?: BgTask[]
}

export interface BgTask {
  id: string
  /** the agent's task type: local_bash, local_agent, monitor... */
  kind?: string
  title: string
  summary?: string
  /** the tool call that started it (a timeline item) */
  toolId?: string
  started: number
}

export interface Tool {
  name: string
  input?: any
  output?: string
  diff?: string
  status: 'running' | 'ok' | 'error' | 'denied' | 'interrupted'
}

export interface Option {
  id: string
  label: string
  style?: 'primary' | 'danger'
}

export interface QOption {
  label: string
  description?: string
}

export interface Question {
  id: string
  header?: string
  question: string
  options?: QOption[]
  multi?: boolean
  other?: boolean
  secret?: boolean
}

export interface Ask {
  tool?: string
  input?: any
  title?: string
  body?: string
  bodyKind?: 'md' | 'code' | 'diff'
  options?: Option[]
  questions?: Question[]
  state: 'pending' | 'answered' | 'cancelled'
  answer?: string
}

export interface PlanStep {
  step: string
  status: 'pending' | 'inProgress' | 'completed'
}

export interface Result {
  durationMs?: number
  costUsd?: number
  turns?: number
  isError?: boolean
  status?: string
}

export type Kind = 'user' | 'text' | 'thinking' | 'tool' | 'approval' | 'question' | 'plan' | 'notice' | 'error' | 'result' | 'media'

/** A file the agent showed the user (show_media), stored as an upload. */
export interface Media {
  id: string
  mime: string
  caption?: string
  name?: string
}

export interface Item {
  id: string
  kind: Kind
  ts: number
  text?: string
  parent?: string
  done?: boolean
  tool?: Tool
  ask?: Ask
  plan?: PlanStep[]
  result?: Result
  /** upload names: attached to a user message, or images in a tool's result */
  images?: string[]
  media?: Media[]
  by?: string
  via?: 'home'
}

export interface Command {
  name: string
  description?: string
  hint?: string
}

export interface ModelOpt {
  id: string
  label: string
  description?: string
}

export interface Caps {
  commands?: Command[]
  models?: ModelOpt[]
}

/** One rate-limit window of an agent account's plan. */
export interface UsageWindow {
  /** five_hour | seven_day | seven_day_opus… (Claude); primary | secondary (Codex) */
  id: string
  label?: string
  /** share used, 0-100 (can run past 100) */
  pct: number
  /** unix ms */
  resetsAt?: number
  /** window length in minutes, when known */
  mins?: number
  status?: string
}

/** An agent account's plan usage, as last reported by one of my sessions. */
export interface AgentUsage {
  windows: UsageWindow[]
  /** Claude: allowed | allowed_warning | rejected */
  status?: string
  plan?: string
  updatedAt: number
}

export interface ModeInfo {
  id: string
  label: string
}

export interface TermInfo {
  id: string
  cwd: string
  title: string
  owner?: string
  project?: string
  isolated?: boolean
  createdAt: number
}

export interface FileChange {
  path: string
  status: string
  diff: string
  binary?: boolean
}

export interface Changes {
  repo: boolean
  branch?: string
  files: FileChange[]
  commits?: string[]
  base?: string
}

export interface UserInfo {
  id: string
  name: string
  displayName: string
  admin?: boolean
}

export interface GroupInfo {
  id: string
  name: string
  members: Record<string, 'owner' | 'member'>
  myRole?: 'owner' | 'member' | ''
}

export interface ProfileFile {
  path: string
  size: number
  dir?: boolean
}

/** Claude Code plugins of one level (Claude Code's own settings keys). */
export interface MarketplaceSource {
  source: string // github | git | url (directory: from a repository)
  repo?: string
  url?: string
  path?: string
}

export interface PluginSet {
  enabledPlugins?: Record<string, boolean>
  extraKnownMarketplaces?: Record<string, { source: MarketplaceSource }>
}

export type PluginLevel = 'global' | 'group' | 'personal' | 'project' | 'repo'

export interface EffectivePlugin {
  id: string
  enabled: boolean
  from: PluginLevel
}

export interface CatalogPlugin {
  id: string
  name: string
  description?: string
}
