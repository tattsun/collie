// The demo projects the README media is shot in. The search scenario edits
// recipeBox into recipeBoxAfter() (which is also what the app screenshots
// the agent "shows" are taken from).

const recipes = `export const recipes = [
  { id: 1, title: 'Tomato & Egg Stir-fry', minutes: 10, ingredients: ['tomato', 'egg', 'scallion'] },
  { id: 2, title: 'Lemon Garlic Chicken', minutes: 20, ingredients: ['chicken breast', 'lemon', 'garlic'] },
  { id: 3, title: 'Mushroom Butter Pasta', minutes: 15, ingredients: ['spaghetti', 'shiitake', 'maitake', 'butter'] },
  { id: 4, title: 'Minestrone', minutes: 30, ingredients: ['canned tomatoes', 'onion', 'carrot', 'celery'] },
  { id: 5, title: 'Ginger Pork', minutes: 15, ingredients: ['pork', 'ginger', 'onion'] },
  { id: 6, title: 'Salmon Avocado Bowl', minutes: 10, ingredients: ['salmon', 'avocado', 'rice'] },
]
`

export const recipeBox = {
  'package.json': `{
  "name": "recipe-box",
  "private": true,
  "type": "module",
  "scripts": {
    "test": "node --test"
  }
}
`,
  'README.md': '# Recipe Box\n\nA tiny web app for the recipes we cook at home.\n',
  'index.html': `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Recipe Box</title>
  <link rel="stylesheet" href="src/style.css">
</head>
<body>
  <header>
    <h1>Recipe Box</h1>
  </header>
  <main>
    <ul id="list" class="cards"></ul>
  </main>
  <script type="module" src="src/app.js"></script>
</body>
</html>
`,
  'src/recipes.js': recipes,
  'src/app.js': `import { recipes } from './recipes.js'

const list = document.getElementById('list')

function card(r) {
  const li = document.createElement('li')
  li.className = 'card'
  li.innerHTML = \`<h2>\${r.title}</h2><p>\${r.minutes} min · \${r.ingredients.join(', ')}</p>\`
  return li
}

export function renderList(items) {
  list.replaceChildren(...items.map(card))
}

renderList(recipes)
`,
  'src/style.css': `body { margin: 0; font-family: system-ui, sans-serif; background: #faf7f2; color: #2b2622; }
header { padding: 20px 16px 8px; }
h1 { margin: 0; font-size: 22px; }
.cards { list-style: none; margin: 0; padding: 8px 16px 24px; display: grid; gap: 12px; }
.card { background: #fff; border-radius: 14px; padding: 14px 16px; box-shadow: 0 1px 3px rgb(0 0 0 / 8%); }
.card h2 { margin: 0 0 4px; font-size: 16px; }
.card p { margin: 0; color: #7a7068; font-size: 13px; }
`,
}

// ---- the edits the search-box scenario makes ----

export const searchEdits = {
  filter: `// Recipes whose title or ingredients contain every word of the query.
export function filterRecipes(recipes, query) {
  const words = query.trim().toLowerCase().split(/\\s+/).filter(Boolean)
  if (words.length === 0) return recipes
  return recipes.filter((r) => {
    const text = [r.title, ...r.ingredients].join(' ').toLowerCase()
    return words.every((w) => text.includes(w))
  })
}
`,
  app: [
    `import { recipes } from './recipes.js'

const list = document.getElementById('list')
`,
    `import { recipes } from './recipes.js'
import { filterRecipes } from './filter.js'

const list = document.getElementById('list')
const q = document.getElementById('q')
const empty = document.getElementById('empty')
`,
  ],
  app2: [
    `  list.replaceChildren(...items.map(card))
}

renderList(recipes)
`,
    `  list.replaceChildren(...items.map(card))
  empty.hidden = items.length > 0
}

q.addEventListener('input', () => renderList(filterRecipes(recipes, q.value)))
renderList(recipes)
`,
  ],
  html: [
    `    <h1>Recipe Box</h1>
  </header>
  <main>
    <ul id="list" class="cards"></ul>
`,
    `    <h1>Recipe Box</h1>
    <input id="q" type="search" placeholder="Search recipes or ingredients" autocomplete="off">
  </header>
  <main>
    <ul id="list" class="cards"></ul>
    <p id="empty" hidden>No recipes found</p>
`,
  ],
  css: [
    `.card p { margin: 0; color: #7a7068; font-size: 13px; }
`,
    `.card p { margin: 0; color: #7a7068; font-size: 13px; }
#q { box-sizing: border-box; width: 100%; margin-top: 12px; padding: 10px 14px; font-size: 16px;
  border: 1px solid #e4ddd3; border-radius: 12px; background: #fff; }
#q:focus { outline: 2px solid #e07a3f; border-color: transparent; }
#empty { padding: 24px 16px; text-align: center; color: #7a7068; }
`,
  ],
  test: `import { test } from 'node:test'
import assert from 'node:assert/strict'
import { filterRecipes } from '../src/filter.js'
import { recipes } from '../src/recipes.js'

const titles = (q) => filterRecipes(recipes, q).map((r) => r.title)

test('empty query returns every recipe', () => {
  assert.equal(filterRecipes(recipes, '  ').length, recipes.length)
})

test('matches titles', () => {
  assert.deepEqual(titles('pasta'), ['Mushroom Butter Pasta'])
})

test('matches ingredients', () => {
  assert.deepEqual(titles('tomato'), ['Tomato & Egg Stir-fry', 'Minestrone'])
})

test('every word must match', () => {
  assert.deepEqual(titles('onion ginger'), ['Ginger Pork'])
})
`,
}

// The recipe box with the search box in place.
export function recipeBoxAfter() {
  const f = { ...recipeBox }
  const sub = (s, [a, b]) => s.replace(a, b)
  f['src/filter.js'] = searchEdits.filter
  f['src/app.js'] = sub(sub(f['src/app.js'], searchEdits.app), searchEdits.app2)
  f['index.html'] = sub(f['index.html'], searchEdits.html)
  f['src/style.css'] = sub(f['src/style.css'], searchEdits.css)
  f['test/filter.test.js'] = searchEdits.test
  return f
}

export const apiServer = {
  'go.mod': 'module example.com/shop-api\n\ngo 1.27\n\nrequire github.com/stripe/stripe-go/v82 v82.1.0\n',
  'main.go': `package main

import (
	"log"
	"net/http"

	"example.com/shop-api/internal/webhook"
)

func main() {
	http.Handle("POST /webhooks/stripe", webhook.Handler(db()))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
`,
  'internal/webhook/handler.go': `package webhook

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/stripe/stripe-go/v82"
)

// Handler applies Stripe events to orders.
func Handler(db *sql.DB) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev stripe.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		switch ev.Type {
		case "checkout.session.completed":
			if err := markPaid(r.Context(), db, ev); err != nil {
				http.Error(w, "retry later", http.StatusInternalServerError)
				return
			}
		case "charge.refunded":
			if err := refund(r.Context(), db, ev); err != nil {
				http.Error(w, "retry later", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})
}
`,
}

export const blog = {
  'package.json': '{\n  "name": "blog",\n  "private": true,\n  "scripts": { "build": "astro build" }\n}\n',
  'src/pages/posts/[page].astro': `---
import { paginate } from '../../lib/paginate'
export async function getStaticPaths() {
  const posts = await Astro.glob('../../content/*.md')
  return paginate(posts, { pageSize: 10 })
}
---
`,
  'src/lib/paginate.ts': `export function paginate<T>(items: T[], { pageSize }: { pageSize: number }) {
  const pages = Math.ceil(items.length / pageSize)
  return Array.from({ length: pages }, (_, i) => ({
    params: { page: String(i + 1) },
    props: { items: items.slice(i * pageSize, pageSize), page: i + 1, pages },
  }))
}
`,
}
