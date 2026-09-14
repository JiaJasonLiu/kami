# Islands Architecture

**Captured:** 2026-09-14
**Tags:** #frontend #architecture #hydration #web-components #sparklayer #performance

## One line
Render a page as mostly static HTML, then hydrate only the small interactive bits ("islands") independently — instead of booting one big SPA that owns the whole page.

## Mental model
- **The sea** = static, non-interactive HTML from the host. Fast, cheap, SEO-friendly.
- **The islands** = discrete interactive components dropped into the sea.
- Each island hydrates on its own: own JS, state, lifecycle, failure boundary. One island crashing doesn't sink the others.

## What makes it "islands" (two independent axes)
| Axis | Question | Not what defines islands |
|------|----------|--------------------------|
| Delivery | Where do the bytes come from — CDN, bundle, per-island chunk? | CDN vs self-hosted is irrelevant to islands-ness |
| Runtime isolation | Does each element hydrate/run/fail independently? | **This is the real test** |

- **Islands** = components hydrate independently and in isolation.
- **Monolith-in-disguise** = one script scans the DOM and mounts everything in a single shared pass with global state.

## Why teams use it
- **Faster TTI / less JS** — ship + boot interactivity only where needed.
- **Resilience** — isolated failure boundaries; a broken widget doesn't kill the page.
- **Deferred / lazy hydration** — hydrate on visible, idle, or interaction; below-the-fold work doesn't block first paint.
- **Progressive enhancement** — static content works before (and without) JS.

## Trade-offs
- Shared state across islands is harder (they're deliberately isolated).
- A single shared runtime bundle gives isolation but loses per-island code-splitting.
- More moving parts than a plain SPA if the whole app is genuinely interactive.

## Where you see it
Astro (islands by default), Qwik (resumability — related idea), Marko, and any web-component / custom-element embed (custom elements upgrade independently → natural fit).

## The SparkLayer angle
SparkLayer's injected B2B web-components (spark-pdp, spark-product-card, quick-buy, quantity tables) dropped into a merchant's Shopify theme are islands — *if* they're true custom elements that hydrate independently. Two checks:
1. **Isolation:** if one component's init throws, do the others survive? (Custom elements give this for free; a scan-and-mount script does not.)
2. **Loading:** a single CDN `sparklayer.js` bundle → runtime isolation but NOT per-island/deferred hydration. So "keep TTI low by lazy-hydrating below-the-fold widgets" is an aspiration, not something you already get.

**Takeaway for the team:** name the pattern, then decide whether the deferred-hydration win is worth pursuing for an embed that must coexist politely with the merchant's own theme JS.
