# Restructuring the interfaces

A plan for rebuilding the GUI and CLI on firmer foundations **without changing
how either looks**. The visual language — glass panels, gradient headings, the
dark palette — is not the problem and is not up for renegotiation here.

## Where things actually stand

Measured, not estimated:

| | GUI | CLI |
|---|---|---|
| Source | 28 JS files, 7,405 lines, 324 KB | 20 Go files, 4,837 lines |
| Styling | 2,641 lines CSS | ANSI helpers in `render.go` |
| Surfaces | 9 tabs, 9 page fragments | ~30 slash commands |
| Build step | none | `go build` |

Neither is in bad shape. What follows is about specific structural weaknesses,
not a rewrite for its own sake.

## Should this be a browser UI at all?

**Yes, and it should stay one.** This was worth checking rather than assuming,
because "serve a web UI from the binary" is a decision that quietly commits you
to a lot.

The alternatives cost more than they return here:

- **Electron** bundles Chromium: 80–150 MB per app and a browser process per
  window. For a tool whose entire job is to sit next to a compiler on a
  developer's machine, shipping a second browser is hard to justify.
- **Tauri** is genuinely lean (3–10 MB, native WebView) but is a Rust backend.
  Adopting it means either a rewrite or a second runtime alongside Go.
- **A pure TUI** would fit the terminal-first instinct, but the Blueprint tab is
  the argument against: a plan graph, per-node acceptance proof and a diff view
  are not things a terminal renders well, and the current implementation is
  three panes of live-updating structured data.

The strongest evidence is what the closest comparable system does. Hermes Agent
serves its UI on `localhost:8642/ui` with a separate dashboard on `9119`,
localhost-only and without built-in auth, alongside a terminal entry point —
architecturally the same choice DarkCode already made, arrived at independently.

DarkCode's version is arguably the better one: it ships **no** browser and
renders in whatever the user already has open. The cost is one embedded HTTP
server, which is already needed for the API.

So: keep it. The rest of this document is about the code behind it.

## GUI: the actual structural problems

### 1. Load order is the module system

Files are named `00-core.js`, `10-sse.js` … `290-plugins.js` because the number
*is* the dependency declaration. Everything shares one global scope: a function
defined in `100-projects.js` is called from `00-core.js`, and the only thing
making that work is that both are `<script>` tags in the right sequence.

This is not hypothetical fragility. During an audit of this codebase, searching
for a helper's definition turned up only its call sites, which read as "called
but never defined" — a conclusion that was wrong, and only disproved by asking a
running browser. Implicit global coupling makes the code hard to reason about
statically, which is exactly when you most want to.

**Fix:** native ES modules. Browsers have supported `import`/`export` for years
and the UI is served from a local origin, so this needs **no bundler, no
transpiler and no `node_modules`** — it is a change to how files declare their
dependencies, not a build pipeline. Dependencies become explicit, dead exports
become findable, and the numeric prefixes stop carrying meaning.

### 2. Every panel invents its own lifecycle

There is no shared contract for "this tab became visible" or "this tab went
away". Each panel improvises: some poll on an interval, some load once and cache
on a `dataset.loaded` flag, some only react to SSE events. The Blueprint poller
shipped without a `document.hidden` guard that every other poller happened to
have — not because anyone decided that, but because there was nothing to inherit
it from.

**Fix:** one small panel contract — `mount()`, `unmount()`, `refresh()` — with
visibility and polling handled once by the shell. A panel then cannot forget a
guard it never had to write.

### 3. Nine tabs, five of which are first-class

`NAV_META` defines nine consolidated tabs plus seven legacy entries "kept for
command-palette / fallback compatibility". The keyboard tab-cycle in
`150-events.js` lists five. So Cognition, Replay, Changes and Cascade are
clickable but not cyclable, and seven more names resolve to something only via
the palette.

**Fix:** decide what is a tab. The natural five are Nexus (chat + workspace),
Blueprint (plan + proof), Registry (tools), Telemetry (metrics, cascade, replay,
changes) and Config. Everything currently orphaned becomes a section inside one
of those, and the legacy aliases either redirect or are removed.

### 4. Several pollers, one server

The Blueprint alone fetches `/api/plan`, `/api/runs`, `/api/approvals` and
`/api/checkpoints` on its own timer; the file tree, metrics and resource panels
each run their own. Nothing coordinates them.

**Fix:** a single shared poller keyed by resource, with panels subscribing. One
timer, deduplicated requests, and a single place to honour visibility.

## CLI: the same shape, different surface

The CLI is healthier — Go gives it real modules and a compiler — but it has
drifted from the GUI in ways that cost users:

- **Command parity is unwritten.** The GUI has a Cascade tab; the CLI has
  `/pipeline`. The GUI has Blueprint; the CLI has `/plan`. Nobody has stated
  which surface is canonical, so features land on one and reach the other late.
- **`console_*.go` mirrors the tab split** (`console_knowledge`,
  `console_models`, `console_settings` …) without sharing the GUI's handlers.
  The same data is fetched and formatted twice, in two languages.

**Fix:** make the HTTP API the single source of truth and have the CLI consume
it, exactly as the browser does. The CLI keeps its own rendering — a terminal
should look like a terminal — but stops reimplementing the data layer. One
command table then drives both the CLI's dispatcher and the GUI's palette.

## Sequencing

Each phase leaves the tool working; none is a flag day.

1. **Extract design tokens.** Pull the palette, spacing and glass/gradient
   treatments out of 2,641 lines of CSS into custom properties. Nothing looks
   different; the theme becomes something you can restate rather than re-derive.
2. **Convert to ES modules, leaf files first.** Start with panels nothing else
   imports (`280-reqlog`, `290-plugins`, `270-blueprint`), finish with
   `00-core`. Drop the numeric prefixes as each file's dependencies become
   explicit.
3. **Introduce the panel contract** and move visibility/polling into the shell.
   This is where the `document.hidden` class of bug stops being possible.
4. **Consolidate to five tabs**, folding the orphans in as sections.
5. **Add the shared poller**, retiring per-panel timers.
6. **Unify the command table**, then port CLI commands onto the HTTP API.

Phases 1–3 are the ones that pay for themselves; 4–6 are cleanup that gets much
easier once 1–3 are done.

## What this deliberately does not do

- No framework. React or Svelte would mean a build step, a dependency tree and a
  toolchain to keep current, for a UI that is already only 7,000 lines.
- No visual redesign. The theme stays exactly as it is.
- No new dependencies. The project holds at four, and none of the above needs a
  fifth.
