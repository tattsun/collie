// The canned conversations fake-claude.mjs plays, picked by the prompt.
// A step is one of:
//   { think | say: text }                       streamed thinking / reply
//   { tool, input, output?, apply?, exec?, ask?, ms? }
// input / output / apply may be functions of the session's cwd. `exec`
// runs the command for real and uses its output; `ask` makes the tool wait
// for the user's approval in Collie.
import { execSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { searchEdits } from './repos.mjs'

const catN = (s) =>
  s
    .replace(/\n$/, '')
    .split('\n')
    .map((l, i) => `${String(i + 1).padStart(6)}\t${l}`)
    .join('\n')

const read = (file) => ({
  tool: 'Read',
  input: (cwd) => ({ file_path: path.join(cwd, file) }),
  output: (cwd) => catN(fs.readFileSync(path.join(cwd, file), 'utf8')),
  ms: 300,
})

const edit = (file, [from, to]) => ({
  tool: 'Edit',
  input: (cwd) => ({ file_path: path.join(cwd, file), old_string: from, new_string: to }),
  apply: (cwd) => {
    const p = path.join(cwd, file)
    fs.writeFileSync(p, fs.readFileSync(p, 'utf8').replace(from, to))
  },
  output: (cwd) => `The file ${path.join(cwd, file)} has been updated successfully.`,
})

const write = (file, content) => ({
  tool: 'Write',
  input: (cwd) => ({ file_path: path.join(cwd, file), content }),
  apply: (cwd) => {
    fs.mkdirSync(path.dirname(path.join(cwd, file)), { recursive: true })
    fs.writeFileSync(path.join(cwd, file), content)
  },
  output: (cwd) => `File created successfully at: ${path.join(cwd, file)}`,
})

const bash = (command, description, more = {}) => ({ tool: 'Bash', input: { command, description }, ...more })

const run = (cwd, command) => {
  try {
    return execSync(command, { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, NO_COLOR: '1' } })
  } catch (e) {
    return (e.stdout || '') + (e.stderr || '')
  }
}

const media = process.env.DEMO_MEDIA || '/tmp/collie-demo/media'

// ---- recipe-box: add a search box (the one the README walks through) ----

const search = [
  { think: 'The list is drawn by renderList in src/app.js, and ingredients are an array in recipes.js. If the filter is a pure function outside the DOM code it is easy to test.' },
  { say: "Let me look at how the list is rendered and what a recipe looks like." },
  read('src/app.js'),
  read('src/recipes.js'),
  { say: "I'll put the filter in `src/filter.js` (matching titles and ingredients) and re-render the list on every keystroke." },
  write('src/filter.js', searchEdits.filter),
  edit('src/app.js', searchEdits.app),
  edit('src/app.js', searchEdits.app2),
  edit('index.html', searchEdits.html),
  edit('src/style.css', searchEdits.css),
  write('test/filter.test.js', searchEdits.test),
  { say: 'Added tests — running them now.' },
  bash('npm test', 'Run the test suite', {
    ask: { suggestions: [{ type: 'addRules', rules: [{ toolName: 'Bash', ruleContent: 'npm test:*' }], behavior: 'allow', destination: 'session' }] },
    output: (cwd) => run(cwd, 'npm test --silent 2>&1'),
    ms: 900,
  }),
  { say: 'All 4 pass. Checking it on a phone-sized screen too.' },
  { tool: 'mcp__playwright__browser_navigate', input: { url: 'http://localhost:5173/' }, output: '### Page\n- Page URL: http://localhost:5173/\n- Page Title: Recipe Box', ms: 600 },
  { tool: 'mcp__playwright__browser_type', input: { element: 'search box', ref: 'e7', text: 'tomato' }, output: 'Typed "tomato"', ms: 500 },
  { tool: 'mcp__playwright__browser_take_screenshot', input: { filename: 'search-tomato.png' }, output: 'Took the viewport screenshot and saved it as search-tomato.png', ms: 500 },
  {
    tool: 'mcp__collie__show_media',
    input: {
      title: 'The recipe list with the new search box (390px wide)',
      files: [
        { path: `${media}/app-all.png`, caption: 'A: before typing' },
        { path: `${media}/app-tomato.png`, caption: 'B: "tomato" (matches ingredients too)' },
      ],
    },
  },
  {
    say: [
      'The recipe list now has a search box.',
      '',
      '- Matches recipe titles **and ingredients**; several words narrow it down (AND)',
      '- Filters as you type, and says "No recipes found" when nothing matches',
      '- The filter is a pure function in `src/filter.js`, with 4 tests in `test/filter.test.js`',
      '',
      'The diff is in the **Changes** tab.',
    ].join('\n'),
  },
]

// ---- recipe-box: dark mode (waits on a question) ----

const darkMode = [
  { say: 'Two quick questions before I start.' },
  {
    tool: 'AskUserQuestion',
    ask: {},
    input: {
      questions: [
        {
          question: 'How should dark mode be switched on?',
          header: 'Switching',
          multiSelect: false,
          options: [
            { label: 'Follow the OS', description: 'prefers-color-scheme only, no button' },
            { label: 'Toggle button', description: 'A button in the header; the choice is remembered' },
            { label: 'Both', description: 'Follow the OS by default, the button overrides it' },
          ],
        },
        {
          question: 'Which palette for the cards?',
          header: 'Palette',
          multiSelect: false,
          options: [
            { label: 'Warm, just darker', description: 'Keep the beige tones, darkened' },
            { label: 'Neutral grays', description: 'Quieter, gray-based cards' },
          ],
        },
      ],
    },
    output: 'User has answered your questions.',
  },
  { say: 'Got it — going with that.' },
]

// ---- shop-api: idempotent webhooks (still running when shot) ----

const webhook = [
  { think: 'Stripe resends the same event.id after a timeout or a 5xx. Record processed ids and reject duplicates in the same transaction as the order update.' },
  { say: 'Let me find the webhook handler.' },
  { tool: 'Grep', input: { pattern: 'stripe.Event', path: 'internal' }, output: 'internal/webhook/handler.go:14:\t\tvar ev stripe.Event', ms: 400 },
  read('internal/webhook/handler.go'),
  { say: 'Stripe redelivers an event with the same `event.id` when we answer slowly, so I will record handled ids in `processed_events` and skip duplicates inside the same transaction as the order update.' },
  write(
    'internal/db/migrations/0007_processed_events.sql',
    'CREATE TABLE processed_events (\n  event_id   TEXT PRIMARY KEY,\n  type       TEXT NOT NULL,\n  created_at TIMESTAMPTZ NOT NULL DEFAULT now()\n);\n',
  ),
  edit('internal/webhook/handler.go', [
    '\t\tswitch ev.Type {',
    '\t\tfresh, err := claimEvent(r.Context(), db, ev.ID, string(ev.Type))\n\t\tif err != nil {\n\t\t\thttp.Error(w, "retry later", http.StatusInternalServerError)\n\t\t\treturn\n\t\t}\n\t\tif !fresh { // a retry of an event we already applied\n\t\t\tw.WriteHeader(http.StatusOK)\n\t\t\treturn\n\t\t}\n\t\tswitch ev.Type {',
  ]),
  bash('go test ./internal/webhook/...', 'Run the webhook tests', { output: 'ok  \texample.com/shop-api/internal/webhook\t0.412s', ms: 3_600_000 }),
]

// ---- blog: pagination fix (finished) ----

const pagination = [
  { say: 'Looking for why page 2 onwards comes out empty.' },
  read('src/lib/paginate.ts'),
  { say: "`slice`'s second argument is an end index, not a count, so page 2 is `slice(10, 10)` — always empty." },
  edit('src/lib/paginate.ts', ['items.slice(i * pageSize, pageSize)', 'items.slice(i * pageSize, (i + 1) * pageSize)']),
  { say: 'Fixed: every page now shows its 10 posts.' },
]

const fallback = [{ say: '(demo) Sure.' }]

export function pickScenario(prompt) {
  const steps =
    (/search/i.test(prompt) && search) ||
    (/dark/i.test(prompt) && darkMode) ||
    (/webhook/i.test(prompt) && webhook) ||
    (/page/i.test(prompt) && pagination) ||
    fallback
  return { steps }
}
