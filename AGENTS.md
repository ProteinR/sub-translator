# Loka Translator — guide for agents

Translates subtitle/script strings in Lokalise projects with the Anthropic API (Claude). Made for
Kristina's Lokalise translation orders. UI and logs are in Russian.

## How it works
- One macOS app window (`Loka Translator.app`, bundle id `com.proteinr.loka-translator`): the
  "Панель" tab is the control panel served by the Go server on `127.0.0.1:8080`, plus one
  WKWebView tab per worker thread. No Playwright/Chromium.
- Go ↔ page bridge: `webkit_darwin.m/.go` (Cocoa window, `callAsyncJavaScript`), `browser.go`
  (`browserTab.Call(op, args)` runs `window.__loka[op]` from `lokalise.js`).
- `lokalise.js` — everything that touches Lokalise DOM: scan rows (`.row-key[data-id]`,
  `.key-translation[data-is-base]`), scroll the document, open a cell, write via the CodeMirror 5
  API (`.CodeMirror`.CodeMirror.setValue), click `button.save.btn-primary`, confirm the saved
  `data-lokalise-editor-value`. Bump `VERSION` there when changing it.
- `lokalise.go` — collection, retries, batching pipeline; `anthropic.go` — API client; `runner.go`
  — run orchestration; `server.go` — HTTP API; `updater.go` — GitHub Releases updates.
- Data: `~/Library/Application Support/Loka Translator/data` for the bundled app (config with the
  API key, `tm.jsonl`, logs). Never print or commit config files.
- Logs: the panel shows Info and above; the file `data/logs/<date>/<time>.log` also gets Debug lines
  (marked `·`): every scanned row with each target cell's state, scroll steps, every inserted cell,
  rejected translations, Lokalise page JS errors. `[N]` is the worker thread. Use `logOf(ctx)` in
  code that runs per project. «Отправить логи» in the panel (`logsend.go`) zips logs and error
  snapshots for the last hours (keys masked, scripts stripped) and sends them to the Telegram chat
  from the settings, or saves to Downloads.

## Working on it
- Tests: `cd src && go vet ./... && go test ./...`.
- Run: `src/build-app.sh <dir>` and open the app. `LOKA_DEBUG=1` adds `POST /api/debug/eval`
  (`{"tab":1,"url":…}` / `{"tab":1,"js":"return window.__loka.state()"}`, header
  `X-Loka-Request: 1`) to inspect a live Lokalise page; right-click → Inspect Element also works.
- Writing into a real Lokalise project changes a client's data: only when the owner asks.
- Release: `scripts/release.sh X.Y.Z "notes"` from a clean, pushed `main` (tests, version bump,
  universal build, tag, `gh release`). Installed apps offer the update with one button.
