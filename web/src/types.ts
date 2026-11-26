export type Agent = 'claude' | 'codex'

export interface Project {
  id: string
  name: string
  path: string
  createdAt: number
  runtime?: '' | 'host' | 'container'
  memory?: string
  cpus?: string
  pids?: number
  git: boolean
  status?: '' | 'cloning' | 'error'
  error?: string
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
  memory?: string
  cpus?: string
  pids?: number
}

export interface ContainerInfo {
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

export type Kind = 'user' | 'text' | 'thinking' | 'tool' | 'approval' | 'question' | 'plan' | 'notice' | 'error' | 'result'

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
  images?: string[]
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
