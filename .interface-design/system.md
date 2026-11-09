# AQAR v2 Interface Design System — "Editorial Property Chronicle & Observatory"

## 1. Intent & Product World
- **Human & Context:** A real-estate quant, market researcher, or infrastructure operator opening the observatory to monitor 24/7 Kingdom-wide property ingestion across 57 rent and sale categories, spot immediate price cuts/hikes, inspect regulatory/structural property monographs, and trigger zero-downtime Git deployments.
- **Core Verbs:** *Audit* crawl coverage and archive integrity, *spot* seller price revisions (`old_price` → `new_price` spreads), *compare* category liquidity (`active` vs `removed` absorption and `SAR/m²` valuation), and *inspect* 115-field property histories.
- **Feel:** An authoritative economic research publication and cadastral chronicle (akin to *Financial Times* statistical supplements, *The Economist* data desks, and sovereign land registries). High-contrast, typographically disciplined, calm, and information-dense.

## 2. Domain Exploration
- **Domain Vocabulary:** Cadastral Registry, Parcel & Plot (`m²`), Facade Orientation (North/East/South/West), Absorption & Turnover, Asking-to-Revised Spread, REGA / FAL License, Sovereign Archive, Edition Stamp, AQZ1 Dictionary Compression.
- **Color World:**
  - *Light Mode ("Riyadh Broadsheet / Warm Newsprint"):* Sunlit limestone & salmon-newsprint paper (`#F6F1E9`), elevated vellum card (`#FBF8F3`), sunken archive well (`#EFE8DC`), deep iron-gall ink (`#141311`), walnut secondary ink (`#4A463F`), sandstone muted caption (`#7A7367`), oxidized terracotta accent (`#B84A27`), and color-blind safe directional semantics: Cobalt Blue (`#1D5D9B` for price cuts / buyer opportunities, paired with `▼`) and Ochre Amber (`#B45309` for price hikes / warnings, paired with `▲`).
  - *Dark Mode ("Midnight Press Room / Obsidian Slate"):* Deep press-stone canvas (`#111110`), elevated zinc-vellum plate (`#181715`), recessed input well (`#0B0B0A`), parchment primary ink (`#F2ECE1`), warm ash secondary (`#A8A196`), muted pewter (`#6E685F`), burnt sienna accent (`#E06D44`), Sky Cobalt (`#58A6FF` for price cuts `▼`), and Warm Gold (`#E3B341` for price hikes `▲`).
- **Signature Element:** **The Diverging Price-Revision Dumbbell & Cadastral Monograph** — every price change is rendered as a scaled horizontal dumbbell vector (`Old SAR ●───► New SAR`) paired with a directional delta badge (`▼ -12.5%`), and clicking any listing opens a full **115-Field Cadastral Monograph** that decodes Saudi-specific domain attributes (compass street direction, street width in meters, FAL license, WhatsApp buyer-lead velocity, and chronological version diff essays).
- **Rejected Defaults:**
  1. *Rejected:* Row of 10 identical KPI boxes at the top → *Replaced with:* **The Broadsheet Masthead & Archive Proportion Strip** (hero figures with inline active/removed/updated proportion bar and live edition telemetry).
  2. *Rejected:* Generic line/donut charts → *Replaced with:* **Category Valuation & Absorption Horizon Table** (horizontal stacked active/removed bars + `SAR/m²` lollipop markers) and **Diverging Price-Spread Dumbbells**.
  3. *Rejected:* Cold blue/purple SaaS dark mode with glowing neon borders → *Replaced with:* **Borders-only editorial hairline grid** (`1px solid var(--chronicle-rule)` and double-rule section headers) in warm newsprint (Light) and obsidian press-ink (Dark).

## 3. Typography & Hierarchy Scale (Ratio 1.25)
- **Display / Editorial Headlines (`--font-editorial`):** `'Newsreader', 'Instrument Serif', 'Playfair Display', Georgia, 'Times New Roman', serif` — optical tight tracking (`-0.02em`), weights `600–700`.
- **UI & Bilingual Arabic/English Body (`--font-sans`):** `'Segoe UI', -apple-system, BlinkMacSystemFont, 'Noto Sans Arabic', Tahoma, sans-serif` — clean legibility for Arabic (`dir="auto"`) property titles and addresses.
- **Figures, Codes & Tabular Data (`--font-mono`):** `'JetBrains Mono', 'Cascadia Mono', 'SFMono-Regular', Consolas, monospace` with mandatory `font-variant-numeric: tabular-nums` on all prices, counters, percentages, and timestamps.
- **Type Scale:**
  - `--text-xs`: `11px` (tracked uppercase section kickers, `letter-spacing: 0.08em`, weight `600`)
  - `--text-sm`: `12.5px` (table cells, metadata, log lines)
  - `--text-base`: `14px` (body copy, controls, Arabic titles)
  - `--text-lg`: `18px` (sub-ledger figures, category headers)
  - `--text-xl`: `24px` (secondary hero metrics)
  - `--text-2xl`: `34px` (masthead primary archive hero figure)

## 4. Depth, Spacing & Layout Grid
- **Depth Strategy:** **Borders-Only Editorial Hairlines** (`1px solid var(--chronicle-rule)` for cell/column divisions, `2px solid var(--chronicle-ink)` top rules for major sections). No fuzzy drop shadows.
- **Spacing Base Unit:** `4px` (`4px, 8px, 12px, 16px, 24px, 32px`).
- **Border Radius Scale:** Crisp editorial geometry — `2px` for badges/tags, `4px` for buttons/inputs/cards.
- **Accessibility & Color-Blind Safety:**
  - Dual-coded directional indicators: Price drops use **Cobalt Blue + `▼` symbol**; Price hikes use **Amber/Terracotta + `▲` symbol**; Errors use **Crimson + `✕`**. Never relies on red vs. green alone.
  - Full Light Mode ("Day Edition") and Dark Mode ("Night Edition") toggle in the masthead.
  - Minimum `40px` touch target height on interactive controls and responsive collapse on mobile/tablet viewports.
