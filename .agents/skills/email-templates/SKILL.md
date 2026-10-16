---
name: email-templates
description: Create, edit, and generate Ogen's email templates (CON-154) end to end — the Maizzle authoring project (../email-templates), the DB-stored seeds in the Go backend, the embedded-default → seed-on-boot → DB-wins lifecycle, the code-owned `variables` docs, the custom [[ ]] Go delimiters that coexist with Maizzle/Tailwind {{ }}, and the Ogen/Harbor brand tokens. Use when the user asks to add/edit an email, change email copy or design, add a welcome/drip/reset template, regenerate/build the templates, adjust the drip schedule, document template variables, or asks how email rendering/seeding works.
tools: Read, Edit, Write, Glob, Grep, Bash
---

# Email Templates Skill

Ogen sends transactional + marketing mail via the CON-154 subsystem (Resend).
There are **two repos** and a one-way flow between them:

```
  ../email-templates  (Maizzle v5 project — AUTHORING)
     emails/<key>.vue  ──npm run build:prod──▶  dist/<key>.html  (minified, inlined)
                                                      │  cp
                                                      ▼
  ogen/src/infra/email/templates/defaults/<key>.html.tmpl   +  <key>.txt.tmpl (hand-written)
                                                      │  go:embed → seed on boot
                                                      ▼
                          email_templates DB row  ──GetByKey+Render──▶  sent mail
```

- **Design/HTML is authored in Maizzle** (`../email-templates`), compiled to
  inlined, email-client-safe HTML. **The Go app never runs Maizzle.**
- The compiled HTML is committed into the backend as the embedded **default
  seed**; the plaintext `.txt.tmpl` is hand-written (Maizzle emits HTML only).
- At runtime the DB row is authoritative; the seed only fills an *absent* key.

## Backend files (`ogen/src/infra/email/templates/`)

- `templates.go` — key constants, the `Data` struct, `//go:embed defaults/*.tmpl`,
  `defaultSpecs` (key → kind + subject + **variables**), the variable-doc
  helpers, `Defaults()`, `SeedDefaults()`.
- `render.go` — `Render()`, the `LeftDelim`/`RightDelim` (`[[` / `]]`) constants.
- `defaults/*.tmpl` — embedded default bodies (`<key>.html.tmpl` + `.txt.tmpl`).

Related: `src/domain/models/email_template.go` (the `EmailTemplate` model incl.
`Variables StringMap`), `src/infra/repository/email_templates.go` (`GetByKey`,
`InsertIfAbsent`, `SyncVariables`), `src/transport/server/email.go` (`initEmail` →
`SeedDefaults`), `src/jobs/queues/send_email.go` (loads `GetByKey` + `Render`),
migrations `20260803000001_email` (+ `..._002_email_template_variables`).

## The authoring project (`../email-templates`)

Maizzle v5 (`@maizzle/framework`), Vue-style SFC templates.

- `emails/<key>.vue` — one template each: `welcome`, `drip_day2/5/7`. Filenames
  match the backend keys.
- `<script setup>` calls `useFont(...)` to load the brand fonts (injects the
  Google-Fonts `<link>` + a `font-<slug>` class).
- Components: `<Layout> <Preheader> <Container> <Section> <Heading> <Text>
  <Button> <Hr>` with **Tailwind** classes (arbitrary values like
  `bg-[#151515]`, `rounded-[3px]` are fine).
- Scripts: `npm run dev` (live preview), `npm run build` (readable),
  **`npm run build:prod`** (`maizzle build --minify` → `dist/*.html`, the
  version copied to the backend).

### Brand tokens (Ogen / Harbor — one design system)

Warm, near-black, ultra-sharp corners. Match these:

- Page bg `#f2f1ef`; card `#ffffff` + `1px solid #e0deda`; radius **3–4px** (sharp).
- Ink/headings `#151515`; body `#57534d`; muted/footer `#8b857c`.
- **Primary CTA**: `bg-[#151515]` white text, **UPPERCASE + `tracking-wide`**,
  `rounded-[3px]`, `font-semibold` — echoes the marketing `.btn`.
- Fonts via `useFont`: **Zalando Sans** (body) + **Zalando Sans Expanded**
  (display/headings), both on Google Fonts, with a system fallback stack.
- Do NOT use the purple `#5e6ad2` (that was a Linear label colour, not the brand).

## Mechanism (backend)

Three stages — **author → seed → render** — plus one delimiter trick.

### 1. DB is the source of truth (copy) — code owns `variables`

`email_templates` columns: `key` PK, `subject`, `html`, `text`, `kind`,
`variables` (jsonb), `version`, `updated_at`. At runtime the row is
authoritative for the **copy** (subject/html/text) — editable without a
redeploy (CON-154 D3). `variables` is the exception: it's **code-owned
metadata** (see below), kept in sync by the seeder.

### 2. Seed on boot — "embedded defaults, DB wins" (+ variables sync)

`Defaults()` reads each embedded `defaults/<key>.{html,txt}.tmpl` and pairs it
with its in-code subject + kind + variables (`defaultSpecs`). On **every** boot,
`initEmail` → `SeedDefaults` loops the defaults and, per template:

```
repo.InsertIfAbsent(t)   // INSERT ... ON CONFLICT (key) DO NOTHING  → copy: DB wins
repo.SyncVariables(...)  // UPDATE ... SET variables = ?             → docs: code wins
```

- **Copy** (`subject/html/text`): `DO NOTHING` — a fresh DB gets the default;
  an existing/edited row is never clobbered. So a newer default in a later build
  does not overwrite an operator's copy edit. (`SeedDefaults` returns the count
  newly inserted — 0 on redeploy, N on first boot.)
- **`variables`**: always refreshed from code, so the docs track the code even
  for rows seeded before a variable existed. Copy is untouched by this UPDATE.

Idempotent, safe every boot — the same env→DB, DB-wins idiom the secrets store
uses (`MigrateFromEnv`).

### 3. Render at send time

`send_email` loads the current row with `GetByKey(templateKey)` and calls
`Render(row, data)`. It reads the DB **per send** (a background worker, not a
request path), so a copy edit takes effect on the **next** send with no redeploy
or re-enqueue — including already-scheduled day-5/day-7 drips. `Render` fills a
single `Data` struct (`Name`, `WorkspaceName`, `AppURL`, `UnsubscribeURL`);
`subject`/`text` via `text/template`, `html` via `html/template` (contextual
auto-escaping). `variables` is **not** used at render time — it's documentation.

### 4. The `[[ ]]` delimiter trick

Maizzle/Tailwind expressions use `{{ }}`, and so does Go's `html/template` by
default. Go templates are parsed with **custom delimiters** (`render.go`):

```go
const ( LeftDelim = "[["; RightDelim = "]]" )
// template.New(name).Delims(LeftDelim, RightDelim).Parse(src)
```

- `[[ .Name ]]` → a **runtime variable**, interpolated by Go at send time.
- `{{ anything }}` → **literal text**, passed through untouched (it's Maizzle's;
  e.g. `{{ new Date().getFullYear() }}` is resolved at *build* time and gone).

`TestRenderCustomDelimsPassThroughBraces` locks this: `Hi [[ .Name ]] {{ keep }}`
with `Name=Zed` → `Hi Zed {{ keep }}`.

## The `variables` attribute

`EmailTemplate.Variables` is a `StringMap` (jsonb) documenting each template's
runtime placeholders — **key = the `[[ .Key ]]` variable name, value = a human
explanation**. It's surfaced for the future template editor and as
self-documentation; it is **not** consumed at render time.

- **Source of truth**: the `defaultSpecs` in `templates.go`, built by the
  `transactionalVars()` / `marketingVars()` helpers. Keep it aligned with the
  `Data` struct fields and the `[[ .Var ]]` actually used in the bodies.
- **Sync**: `SeedDefaults` calls `repo.SyncVariables(key, vars)` every boot, so
  the docs always reflect the code (unlike copy, which is operator-owned).
- **Do not** edit `variables` in the DB — it's overwritten on the next boot.

## Task: edit an email's copy (no code change)

No template management API yet — edit the DB row directly:

```sql
UPDATE email_templates
SET subject = '...', html = '...', text = '...', version = version + 1, updated_at = now()
WHERE key = 'welcome';
```

The next send renders it (no redeploy). Keep the `[[ .Field ]]` vars intact.
Re-seeding won't revert it (copy is `DO NOTHING`). For a bigger design change,
edit the Maizzle source and regenerate (next task) — but note that only reseeds
databases where the key is still absent.

## Task: add or redesign a template

1. **Author in Maizzle** (`../email-templates/emails/<key>.vue`). Match the brand
   tokens above. Use `[[ .Field ]]` for runtime vars; let Tailwind/Maizzle
   `{{ }}` be. Marketing templates **must** include a footer
   `<a href="[[ .UnsubscribeURL ]]">Unsubscribe</a>`.
2. **Build**: `npm run build:prod` → `dist/<key>.html` (minified, inlined).
   Preview with `npm run dev` first.
3. **Copy to the backend**: `cp ../email-templates/dist/<key>.html
   src/infra/email/templates/defaults/<key>.html.tmpl`. Hand-write the plaintext
   `defaults/<key>.txt.tmpl` (with the same `[[ .Field ]]` vars).
4. **Register** in `templates.go`:
   - Add a key const (e.g. `KeyPasswordReset = "password_reset"`).
   - Add a `defaultSpec{Key, Kind, Subject, Variables}` — `Kind` is
     `EmailKindTransactional` or `EmailKindMarketing`; `Subject` may use
     `[[ .Field ]]`; `Variables` uses `transactionalVars()` / `marketingVars()`
     (or a bespoke `StringMap` documenting every `[[ .Var ]]` the body uses).
5. **Data fields**: if the template needs a variable not on `Data` (`Name`,
   `WorkspaceName`, `AppURL`, `UnsubscribeURL`), add it to the `Data` struct and
   populate it in `send_email.go`'s `Process`, and document it in the template's
   `Variables`.
6. **Kind semantics**: `marketing` needs the unsubscribe footer and is
   suppression-gated (blocked by any suppression); `transactional` sends even to
   marketing-unsubscribed addresses (blocked only by an `all`-scope suppression).
   The job builds `UnsubscribeURL` + `List-Unsubscribe` headers only for
   `marketing`.

## Task: trigger a new lifecycle email

Rendering is only half — something must **enqueue** a `send_email` job. See
`src/jobs/queues/jobs.go`:

- `EnqueueWelcomeEmailTx` / `EnqueueDripTx` are the models. Add
  `Enqueue<X>Tx(ctx, tx, userID, tenantID)` inserting a `SendEmailTask`
  (`TemplateKey`, `EmailKind`, `IdempotencyKey`) via `InsertTx` so the mail
  commits atomically with the triggering DB write.
- Immediate: default `ScheduledAt`. Delayed/drip: `river.InsertOpts{ScheduledAt:
  now.Add(offset)}` (see `dripSchedule`).
- `IdempotencyKey` = `"<key>:<userID>"` (Resend `Idempotency-Key` + the
  `email_logs` partial-unique backstop → a retried enqueue can't double-send).
- The drip cadence (day 2/5/7) is the in-code `dripSchedule` table — one edit.

## Gotchas

1. **Copy is `DO NOTHING`; `variables` is synced.** Don't switch `InsertIfAbsent`
   to an upsert (it would clobber operator copy). Don't edit `variables` in the
   DB (the seeder overwrites it) — change `defaultSpecs`.
2. **Use `[[ ]]`, not `{{ }}`, for variables.** `{{ }}` is Maizzle's and passes
   through as literal text; `{{ .Name }}` in a stored body will NOT interpolate.
3. **Only reference fields on `Data`.** `html/template` execute errors are
   terminal in the send job (logged `failed`, no retry) — a typo'd field kills
   that send. Add a render test case for a new template.
4. **Subjects are templates too** (`text/template`, same `[[ ]]` delimiters).
5. **HTML auto-escapes; plaintext does not.** Don't hand-escape.
6. **Editing a row affects in-flight drips** — the row is read per send.
7. **Regenerate with `build:prod`, not `build`** — the backend seeds should be
   the minified, production output.

## Checklist (adding a template)

- Maizzle `emails/<key>.vue` authored + `npm run build:prod` clean.
- `dist/<key>.html` copied to `defaults/<key>.html.tmpl`; `<key>.txt.tmpl`
  hand-written.
- `templates.go`: key const + `defaultSpec` (kind + subject + **variables**);
  `Data` fields added + populated in `send_email.go` if new.
- Enqueue path in `jobs.go` (+ consumer interface / call site) if it's a new
  trigger.
- Render test case for the new template; `go test ./src/infra/email/... ./src/jobs/...`
  green (a real seed + the `variables` jsonb round-trip are exercised by any
  `pgtest`-backed suite, e.g. `src/infra/repository` email tests).
```
