#!/usr/bin/env node
// A scripted stand-in for `claude -p` that speaks the stream-json protocol
// Collie drives (see internal/agent/claude.go). It plays canned
// conversations from scenarios.mjs so the README screenshots and the demo
// video can be re-shot without a Claude account and look the same every time.
//
// Collie starts it in place of claude: `collie --claude tools/demo/fake-claude.mjs`.
import { spawn } from 'node:child_process'
import readline from 'node:readline'
import { pickScenario } from './scenarios.mjs'

const argv = process.argv.slice(2)
const arg = (k) => {
  const i = argv.indexOf(k)
  return i >= 0 ? argv[i + 1] : undefined
}
const model = arg('--model') && arg('--model') !== 'default' ? arg('--model') : 'claude-opus-5-5'
const speed = Number(process.env.DEMO_SPEED || 1)
const CTX = 200_000

const out = (o) => process.stdout.write(JSON.stringify(o) + '\n')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms / speed))

let cost = 0
let ctxUsed = 24_000
let turnStarted = 0
let running = null // the scenario being played
let interrupted = false
const answers = new Map() // our can_use_tool request id -> resolve

// ---- stdin: control requests, user messages, answers to our requests ----

readline.createInterface({ input: process.stdin }).on('line', (line) => {
  let o
  try {
    o = JSON.parse(line)
  } catch {
    return
  }
  if (o.type === 'control_request') return onControl(o.request_id, o.request || {})
  if (o.type === 'control_response') {
    const r = o.response || {}
    const done = answers.get(r.request_id)
    if (done) {
      answers.delete(r.request_id)
      done(r.response || {})
    }
    return
  }
  if (o.type === 'user') {
    const text = (o.message?.content || []).filter((c) => c.type === 'text').map((c) => c.text).join('\n')
    play(text)
  }
})

function reply(id, response) {
  out({ type: 'control_response', response: { subtype: 'success', request_id: id, response } })
}

function onControl(id, req) {
  switch (req.subtype) {
    case 'initialize':
      return reply(id, {
        commands: [
          { name: 'compact', description: 'Clear conversation history but keep a summary in context' },
          { name: 'review', description: 'Review a pull request' },
          { name: 'init', description: 'Initialize a new CLAUDE.md file with codebase documentation' },
        ],
        models: [
          { value: 'default', displayName: 'Default (recommended)', description: 'Opus 5.5' },
          { value: 'sonnet', displayName: 'Sonnet', description: 'Sonnet 5.5 · fast for everyday tasks' },
          { value: 'haiku', displayName: 'Haiku', description: 'Haiku 5.5 · fastest' },
        ],
      })
    case 'get_context_usage':
      return reply(id, { totalTokens: ctxUsed, maxTokens: CTX })
    case 'get_usage': {
      const at = (h) => new Date(Date.now() + h * 3600_000).toISOString()
      return reply(id, {
        rate_limits_available: true,
        subscription_type: 'max',
        rate_limits: {
          five_hour: { utilization: 34, resets_at: at(2.4) },
          seven_day: { utilization: 58, resets_at: at(70) },
        },
      })
    }
    case 'interrupt':
      interrupted = true
      for (const [rid, done] of answers) {
        answers.delete(rid)
        done({ behavior: 'deny', message: 'Interrupted' })
      }
      return reply(id, {})
    default: // set_permission_mode, set_model, …
      return reply(id, {})
  }
}

// ---- playing a scenario ----

async function play(prompt) {
  if (running) return // a real claude would queue it; the demo never needs that
  interrupted = false
  turnStarted = Date.now()
  out({ type: 'system', subtype: 'init', model, session_id: arg('--session-id') || arg('--resume'), cwd: process.cwd() })
  running = pickScenario(prompt)
  let turns = 0
  try {
    for (const step of running.steps) {
      if (interrupted) break
      turns++
      await run(step)
    }
  } finally {
    running = null
  }
  cost += 0.12 + turns * 0.03
  out({
    type: 'result',
    subtype: 'success',
    is_error: false,
    duration_ms: Date.now() - turnStarted,
    num_turns: turns,
    total_cost_usd: cost,
    modelUsage: { [model]: { contextWindow: CTX } },
  })
}

let msgSeq = 0
function message(blocks) {
  ctxUsed += 1800 + Math.floor(Math.random() * 1500)
  return {
    id: `msg_demo_${process.pid}_${++msgSeq}`,
    type: 'message',
    role: 'assistant',
    model,
    content: blocks,
    usage: { input_tokens: 6, cache_read_input_tokens: ctxUsed - 600, cache_creation_input_tokens: 400, output_tokens: 194 },
  }
}

async function stream(kind, text) {
  const msg = message([])
  out({ type: 'stream_event', event: { type: 'message_start', message: { ...msg, content: [] } } })
  out({ type: 'stream_event', event: { type: 'content_block_start', index: 0, content_block: { type: kind, [kind]: '' } } })
  const delta = kind === 'text' ? 'text_delta' : 'thinking_delta'
  for (let i = 0; i < text.length; ) {
    if (interrupted) break
    const n = 2 + Math.floor(Math.random() * 5)
    out({ type: 'stream_event', event: { type: 'content_block_delta', index: 0, delta: { type: delta, [kind]: text.slice(i, i + n) } } })
    i += n
    await sleep(28)
  }
  out({ type: 'stream_event', event: { type: 'content_block_stop', index: 0 } })
  out({ type: 'assistant', message: { ...msg, content: [{ type: kind, [kind]: text }] }, parent_tool_use_id: null })
}

let reqSeq = 0
function ask(tool, input, suggestions) {
  const id = `demo-${process.pid}-${++reqSeq}`
  return new Promise((resolve) => {
    answers.set(id, resolve)
    out({
      type: 'control_request',
      request_id: id,
      request: { subtype: 'can_use_tool', tool_name: tool, input, permission_suggestions: suggestions || [] },
    })
  })
}

async function run(step) {
  if (step.wait) return sleep(step.wait)
  if (step.think) return stream('thinking', step.think)
  if (step.say) return stream('text', step.say)
  if (step.tool) return runTool(step)
}

async function runTool(step) {
  const id = `toolu_demo_${process.pid}_${++msgSeq}`
  const cwd = process.cwd()
  let input = typeof step.input === 'function' ? step.input(cwd) : step.input
  const msg = message([])
  out({ type: 'stream_event', event: { type: 'message_start', message: { ...msg, content: [] } } })
  out({ type: 'stream_event', event: { type: 'content_block_start', index: 0, content_block: { type: 'tool_use', id, name: step.tool, input: {} } } })
  await sleep(350)
  out({ type: 'assistant', message: { ...msg, content: [{ type: 'tool_use', id, name: step.tool, input }] }, parent_tool_use_id: null })

  if (step.ask) {
    const r = await ask(step.tool, input, step.ask.suggestions)
    if (r.behavior !== 'allow') {
      result(id, r.message || "The user doesn't want to proceed with this tool use.", true)
      interrupted = true
      return
    }
    input = r.updatedInput || input
  }
  if (step.tool === 'mcp__collie__show_media') return result(id, await showMedia(input))
  await sleep(step.ms ?? 700)
  if (step.apply) step.apply(cwd)
  result(id, typeof step.output === 'function' ? step.output(cwd) : step.output || '')
}

function result(id, content, isError = false) {
  out({
    type: 'user',
    message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: id, content, is_error: isError }] },
    parent_tool_use_id: null,
  })
}

// show_media goes through Collie's real tool (the MCP server named in
// --mcp-config), so the gallery in the chat is the real one.
async function showMedia(input) {
  const cfg = JSON.parse(arg('--mcp-config') || '{}').mcpServers?.collie
  if (!cfg) return 'Collie tools are not available'
  const p = spawn(cfg.command, cfg.args || [], { stdio: ['pipe', 'pipe', 'inherit'] })
  const lines = readline.createInterface({ input: p.stdout })
  const pending = new Map()
  lines.on('line', (l) => {
    try {
      const m = JSON.parse(l)
      pending.get(m.id)?.(m)
    } catch {}
  })
  let n = 0
  const call = (method, params) =>
    new Promise((resolve) => {
      const id = ++n
      pending.set(id, resolve)
      p.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n')
    })
  await call('initialize', { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'fake-claude', version: '0' } })
  p.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n')
  const r = await call('tools/call', { name: 'show_media', arguments: input })
  p.stdin.end()
  p.kill()
  return (r.result?.content || []).map((c) => c.text).join('\n') || JSON.stringify(r.error || r.result)
}
