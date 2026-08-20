# UI Design Conventions — RowBot

How to redesign or improve a page in this codebase. Written after reworking
`web/templates/dashboard-server.html` and `web/templates/partials/channel-region.html`
(see git history for the before/after). The goal here isn't generic visual
taste — it's "make this look like it belongs in RowBot," which mostly means
finding and reusing patterns that already exist rather than inventing new
ones.

## 1. Survey before designing anything

Before writing a single class, read the other pages that already look good:

- `web/templates/profile.html` — the reference page. Multiple independent
  `card bg-base-100 shadow-xl` sections stacked in a `space-y-6` wrapper,
  each self-contained.
- `web/templates/landing.html` — modal pattern reference.

Grep for the component you're about to use before assuming its class name or
usage is novel — `grep -rn "badge\|stat\|divider\|modal" web/templates/`. If
a pattern already exists twice, it's a convention; match it instead of
inventing a third variant. Consistency across pages beats a locally "nicer"
one-off.

## 2. The established row pattern

The single most reusable unit in this codebase (see the "Connections" card
in `profile.html`, lines ~17-49) is:

```html
<div class="card bg-base-100 shadow-xl">
  <div class="card-body">
    <div class="flex items-center justify-between py-2">
      <div class="flex items-center gap-3">
        <svg ... class="shrink-0 text-primary">...</svg>
        <div>
          <p class="font-medium">{{ label }}</p>
          <p class="text-sm text-base-content/60">{{ status text }}</p>
        </div>
      </div>
      {{ badge or button/form on the right }}
    </div>
  </div>
</div>
```

Icon + label/status on the left, a single badge or action on the right.
Reuse this for anything that's fundamentally "here's a thing, here's its
state, here's the one action you can take on it" — connection status,
registration status, feature toggles, etc.

## 3. Page structure: stacked cards, not one mega-card

Don't cram unrelated concerns into one `card-body` as a flat stack of `<p>`
tags (that was the original bug on the guild dashboard page). Split by
concern into separate cards inside a `space-y-6` wrapper — one card per
"thing the user is looking at or acting on." Within a card, use
`<div class="divider my-1"></div>` to separate a primary status row from
secondary/admin-only controls underneath it, rather than starting a whole
new card for a closely-related sub-section.

Page width: `max-w-3xl` for a single-column stack of cards.
`max-w-5xl`/wider is for pages with an actual multi-column grid — don't use
a wide container just because a page has a lot of content if it's still
fundamentally a single vertical list.

## 4. Icons

Inline SVG only, no icon library/dependency (matches the stdlib-first,
justify-new-dependencies rule in `CLAUDE.md`). Convention, copied from
`profile.html`:

```html
<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24"
     fill="none" stroke="currentColor" stroke-width="2"
     stroke-linecap="round" stroke-linejoin="round"
     class="shrink-0 text-primary">
  ...paths...
</svg>
```

24×24, `stroke="currentColor"`, `stroke-width="2"`, colored via a text-color
utility class (`text-primary` for the app's accent, or something like
`text-blue-600` if a row needs to visually distinguish itself, per the
existing Concept2 icon). Always `shrink-0` when it sits in a `flex` row next
to text, or it'll compress on narrow viewports. Pick a shape that's a
recognizable metaphor for the thing it labels (e.g. a hash/pound icon for a
Discord channel, a people icon for registration/membership) — feathericons
(feathericons.com) is a reasonable free source of matching-style outline
icons if you need to find a new one; keep the same stroke-based style rather
than mixing in filled icons.

## 5. daisyUI components: what to use for what

- **`badge`** — for a single-word/short status, not a sentence. Variants
  actually used in this codebase and their meaning:
  - `badge badge-outline` — neutral/plain (e.g. "manager" role tag, a plain
    count). This is the safe default.
  - `badge badge-success badge-outline` — positive/configured/done state.
  - `badge badge-ghost` — muted/absent/not-set state.
  - **Avoid `badge-neutral badge-outline`** — on this app's dark `business`
    theme it renders almost invisible (very low contrast border+text against
    the dark background). Verified by screenshot; don't reuse it. Plain
    `badge-outline` was the fix.
- **`divider`** — separates sections *within* one card. Use `my-1` to keep
  it tight; the default margin reads as too much whitespace inside a card
  that already has `card-body` padding.
- **`modal`/`dialog`** — see `channel-region.html` or the `add-server-modal`
  in `profile.html` for the exact markup (native `<dialog>`, a `modal-box`,
  a `method="dialog"` form wrapping the ✕ close button positioned
  `absolute right-2 top-2`, and a `modal-backdrop` form). This is daisyUI's
  own documented modal idiom — don't deviate from it.
  - Opening a modal must go through a class hook (`.js-*`) plus a listener
    in `web/static/js/app.js`, **never** an inline `onclick="..."` attribute
    — this app's CSP (`internal/http/middleware/securityheaders.go`) is
    `script-src 'self'` with no `unsafe-inline`/nonce, so inline handlers
    get silently blocked by the browser. If the element the listener needs
    to bind to can be replaced by an htmx swap (e.g. `hx-swap="outerHTML"`),
    bind the listener via delegation on `document` instead of directly on
    the element — a direct binding won't survive the swap.
- **`stats`/`stat`** — not currently used anywhere in this app. If a page
  needs to headline a single big number (unlike the small inline
  "Registered users: N" count here, which stayed a plain label+badge pair
  because it's secondary info, not the page's main point), it's a
  reasonable daisyUI component to introduce — but note it'd be the first
  use in the codebase, so make sure it's really warranted before adding a
  new pattern instead of reusing badges/text.

## 6. Verify visually before calling it done

Don't ship a redesign purely by reading the template diff — render it and
look. This repo has no persistent browser tooling assumption, but you can
always do this locally with what's on a normal dev machine:

1. Render the real template with real data through a throwaway Go test
   using this app's own test helpers (see `newProfileRealTemplateRenderer`
   and `serveProfileRequest` in `internal/http/handlers/profile_test.go`) —
   write the response body to a file. This exercises the actual
   `html/template` execution, not a hand-copied approximation, so template
   bugs (unclosed tags, bad conditionals) show up too.
2. Copy the current `web/static/css/dist/app.css` (run `make tailwind`
   first if it doesn't exist) next to that HTML file so classes resolve.
3. Serve that directory with `python3 -m http.server <port>`.
4. Screenshot with headless Chrome, no extension/MCP required:
   ```
   "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
     --headless --disable-gpu --hide-scrollbars \
     --window-size=900,700 --force-device-scale-factor=2 \
     --screenshot=/path/to/out.png \
     http://localhost:<port>/rendered.html
   ```
5. Actually look at the image (read it back in) before declaring success.
   This is exactly how the `badge-neutral badge-outline` contrast bug in §5
   was caught — it was invisible in the template source and only obvious in
   the rendered screenshot.

Render every meaningfully different state (admin vs. member, configured vs.
not, registered vs. not, etc.), not just the happy path you had in mind
while writing the markup — different `{{if}}` branches can produce
different, unintentionally-broken layouts.

## 7. After any template/CSS change

- `make tailwind` if you touched classes (new component variants must
  actually compile — Tailwind v4 here scans `web/templates/**/*.html` via
  `@source` in `web/static/css/app.css`, but the compiled
  `web/static/css/dist/app.css` is gitignored and only regenerates on
  `make tailwind`, so a stale local copy can hide a class that doesn't
  actually exist).
- `go build ./... && go vet ./... && go test ./...` — template syntax
  errors and handler-data mismatches surface here even if you can't run a
  browser.
- Check `internal/http/handlers/profile_test.go` (or whichever test covers
  the page) for string-match assertions on exact rendered output
  (`id="..."`, `hx-post="..."`, etc.) that your markup changes might have
  moved or renamed.
