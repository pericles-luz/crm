// Browser-smoke for the AI panel cooldown UI (SIN-62318).
//
// Drives the Go fixture under cmd/aipanel-e2e-fixture, which mounts the
// production renderers (aipanel.LiveButton, aipanel.CooldownFragment) and
// the production rate-limit middleware behind an in-memory token-bucket
// limiter (capacity 1, refill = AIPANEL_COOLDOWN_MS).
//
// Acceptance criteria covered (see issue body):
//   1. spam-click flow → 429 swap, bar shrinks, live button recoverable
//   2. prefers-reduced-motion: reduce → no animation, label still rendered
//   3. accessibility → keyboard focus + native disabled semantics
//   4. no CLS during the swap

import { test, expect, type Page, type Response } from "@playwright/test";

const COOLDOWN_MS = Number(process.env.AIPANEL_COOLDOWN_MS || 1500);
const SWAP_SETTLE_MS = 250; // htmx swap + a small slack for paint

const liveButton = (page: Page) => page.locator("button.ai-panel-regenerate#ai-panel-regenerate");
const cooldownButton = (page: Page) => page.locator("button.ai-panel-cooldown#ai-panel-regenerate");
const cooldownBar = (page: Page) => page.locator("button.ai-panel-cooldown .ai-panel-cooldown__bar");
const cooldownLabel = (page: Page) => page.locator("button.ai-panel-cooldown .ai-panel-cooldown__label");

async function waitForRegen(page: Page): Promise<Response> {
  return page.waitForResponse(
    (r) => r.url().endsWith("/regen") && r.request().method() === "POST",
  );
}

test.describe("AI panel cooldown UI", () => {
  test.beforeEach(async ({ page, request }) => {
    // The fixture's bucket is keyed by a constant identity tuple, so
    // every test would otherwise inherit whatever state the previous
    // one left. Hit the test-only reset endpoint (loopback-gated by the
    // fixture itself) to drop both buckets — that removes the only
    // source of inter-test flake without forcing every spec to wait
    // out a full cooldown window.
    const reset = await request.post("/test/reset");
    expect(reset.status()).toBe(204);

    await page.goto("/");
    await expect(liveButton(page)).toBeVisible();
  });

  test("spam-click triggers 429 + cooldown swap, bar animates, live button recovers", async ({
    page,
  }) => {
    // First click: bucket has a token, server returns 200 + live button.
    const okResponsePromise = waitForRegen(page);
    await liveButton(page).click();
    const okResponse = await okResponsePromise;
    expect(okResponse.status()).toBe(200);
    await expect(liveButton(page)).toBeVisible();

    // Second click: bucket is empty, middleware returns 429 + cooldown
    // fragment, htmx swaps it in via outerHTML.
    const denyResponsePromise = waitForRegen(page);
    await liveButton(page).click();
    const denyResponse = await denyResponsePromise;
    expect(denyResponse.status()).toBe(429);
    expect(denyResponse.headers()["retry-after"]).toMatch(/^\d+$/);

    // The swap target id parity is enforced server-side; assert the
    // disabled fragment is now in the DOM at the same id.
    await expect(cooldownButton(page)).toBeVisible();
    await expect(liveButton(page)).toHaveCount(0);
    await expect(cooldownButton(page)).toBeDisabled();
    await expect(cooldownButton(page)).toHaveAttribute("aria-disabled", "true");
    await expect(cooldownLabel(page)).toContainText(/Próxima geração em \d+ s/);

    // SIN-62319: under the F29 CSP (style-src 'self' 'nonce-{N}') the
    // browser drops every inline style attribute, so the renderer must
    // not encode the cooldown duration there. The fragment instead emits
    // `data-cooldown-bucket="N"` (a per-second integer 1..60 or
    // "overflow"), and a static stylesheet pairs each bucket with a
    // --cooldown-duration value. Assert (a) no inline style survives,
    // (b) the bucket attribute is in the expected shape, and (c) the
    // stylesheet-driven --cooldown-duration on the animated bar is the
    // ceil-rounded second matching the server's Retry-After.
    const inlineStyle = await cooldownButton(page).getAttribute("style");
    expect(inlineStyle === null || inlineStyle === "").toBeTruthy();

    const bucket = await cooldownButton(page).getAttribute("data-cooldown-bucket");
    expect(bucket).toMatch(/^(?:[1-9]|[1-5]\d|60|overflow)$/);

    const declaredCooldownMs = await cooldownBar(page).evaluate((el) => {
      const v = getComputedStyle(el as HTMLElement)
        .getPropertyValue("--cooldown-duration")
        .trim();
      const sMatch = v.match(/^([\d.]+)s$/);
      if (sMatch) return Number(sMatch[1]) * 1000;
      const msMatch = v.match(/^([\d.]+)ms$/);
      return msMatch ? Number(msMatch[1]) : 0;
    });
    expect(declaredCooldownMs).toBeGreaterThan(0);
    expect(declaredCooldownMs).toBeLessThanOrEqual(COOLDOWN_MS + 1000);

    // The animated bar shrinks across the cooldown window. The element
    // uses `transform: scaleX(...)` (not width), so we read the
    // computed transform matrix; entry 0 is the x-scale factor and
    // moves from 1.0 → 0.0 over `--cooldown-duration`.
    const scaleAtStart = await cooldownBar(page).evaluate((el) => {
      const t = getComputedStyle(el as HTMLElement).transform;
      const m = t.match(/matrix\(([^)]+)\)/);
      return m ? Number(m[1].split(",")[0]) : NaN;
    });
    expect(scaleAtStart).toBeGreaterThan(0.5);

    await page.waitForTimeout(Math.floor(declaredCooldownMs / 2));

    const scaleMid = await cooldownBar(page).evaluate((el) => {
      const t = getComputedStyle(el as HTMLElement).transform;
      const m = t.match(/matrix\(([^)]+)\)/);
      return m ? Number(m[1].split(",")[0]) : NaN;
    });
    expect(scaleMid).toBeLessThan(scaleAtStart);

    // After Retry-After elapses the server returns the live state again.
    // The shipped cooldown fragment does not auto-recover today (the
    // <button disabled> swallows clicks), so the fixture host page
    // exposes a "Manual refresh" affordance — clicking it issues a GET
    // /refresh that returns the live button. That is the smallest
    // server-round-trip that proves recovery.
    await page.waitForTimeout(declaredCooldownMs + 200);
    await page.locator("#manual-refresh").click();
    await expect(liveButton(page)).toBeVisible();
    await expect(cooldownButton(page)).toHaveCount(0);
  });

  test("prefers-reduced-motion: reduce collapses the bar without animation, keeps the label", async ({
    browser,
  }) => {
    const ctx = await browser.newContext({ reducedMotion: "reduce" });
    const page = await ctx.newPage();
    try {
      await page.goto("/");
      await expect(liveButton(page)).toBeVisible();

      // Trip the limiter: first call consumes the token, second is denied.
      const okPromise = waitForRegen(page);
      await liveButton(page).click();
      await okPromise;
      const denyPromise = waitForRegen(page);
      await liveButton(page).click();
      const deny = await denyPromise;
      expect(deny.status()).toBe(429);
      await expect(cooldownButton(page)).toBeVisible();

      // Under prefers-reduced-motion the bar must use the static
      // collapsed transform (scaleX(0)) and no `animation-name`.
      await page.waitForTimeout(SWAP_SETTLE_MS);
      const bar = cooldownBar(page);
      const animationName = await bar.evaluate((el) =>
        getComputedStyle(el).animationName,
      );
      expect(animationName === "none" || animationName === "").toBeTruthy();

      const transform = await bar.evaluate((el) => getComputedStyle(el).transform);
      // matrix(0, 0, 0, 1, 0, 0) is scaleX(0); accept either the
      // unprefixed `none → matrix` form or any matrix with a 0 in the
      // x-scale slot. Practical compatibility check.
      expect(transform === "matrix(0, 0, 0, 1, 0, 0)" || /matrix\(0,/.test(transform)).toBeTruthy();

      // Caption still renders and is human-readable.
      await expect(cooldownLabel(page)).toContainText(/Próxima geração em \d+ s/);
      await expect(cooldownButton(page)).toBeDisabled();
    } finally {
      await ctx.close();
    }
  });

  test("keyboard focus + native disabled semantics survive the swap", async ({
    page,
  }) => {
    // Focus the live button via keyboard before the swap so we can
    // observe focus behaviour after the outerHTML replacement.
    await liveButton(page).focus();
    await expect(liveButton(page)).toBeFocused();

    // Click via keyboard to consume the token, then again to trip 429.
    const okPromise = waitForRegen(page);
    await page.keyboard.press("Enter");
    await okPromise;
    await liveButton(page).focus();
    const denyPromise = waitForRegen(page);
    await page.keyboard.press("Enter");
    const deny = await denyPromise;
    expect(deny.status()).toBe(429);
    await expect(cooldownButton(page)).toBeVisible();

    // The native <button disabled> communicates the state to assistive
    // tech; aria-disabled doubles the signal for HTMX-aware screen
    // readers. Both must be present.
    await expect(cooldownButton(page)).toBeDisabled();
    await expect(cooldownButton(page)).toHaveAttribute("aria-disabled", "true");
    await expect(cooldownButton(page)).toHaveAttribute("aria-live", "polite");

    // The visible label is what a sighted user reads; assistive tech
    // gets the same text via the button's accessible name.
    const accessibleName = await cooldownButton(page).evaluate(
      (el) => (el as HTMLElement).innerText.trim(),
    );
    expect(accessibleName).toMatch(/Próxima geração em \d+ s/);

    // Focus survives the swap or recovers to a sane target. htmx moves
    // focus to the swapped element when the previously-focused element
    // is replaced; assert the cooldown button (same id) is focused, or
    // at minimum that focus is on the body — never on a stale node.
    const focusedTag = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      return { tag: el?.tagName ?? "", id: el?.id ?? "" };
    });
    const onSwapTarget =
      focusedTag.id === "ai-panel-regenerate" && focusedTag.tag === "BUTTON";
    const onBody = focusedTag.tag === "BODY" || focusedTag.tag === "";
    expect(onSwapTarget || onBody).toBeTruthy();
  });

  test("layout shift score is ~0 across the swap (no CLS)", async ({ page }) => {
    // Wire a PerformanceObserver before the click so we capture every
    // shift entry produced by the swap. The shipped CSS gives both
    // states the same outer box; the observed score should be 0.
    await page.evaluate(() => {
      (window as unknown as { __cls?: number }).__cls = 0;
      const po = new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          // hadRecentInput=false ensures we don't count user-initiated
          // shifts (clicks). The HTMX swap is programmatic.
          const e = entry as PerformanceEntry & {
            value?: number;
            hadRecentInput?: boolean;
          };
          if (!e.hadRecentInput && typeof e.value === "number") {
            (window as unknown as { __cls: number }).__cls += e.value;
          }
        }
      });
      po.observe({ type: "layout-shift", buffered: true });
    });

    const okPromise = waitForRegen(page);
    await liveButton(page).click();
    await okPromise;
    const denyPromise = waitForRegen(page);
    await liveButton(page).click();
    const deny = await denyPromise;
    expect(deny.status()).toBe(429);
    await expect(cooldownButton(page)).toBeVisible();
    await page.waitForTimeout(SWAP_SETTLE_MS);

    const cls = await page.evaluate(
      () => (window as unknown as { __cls: number }).__cls,
    );
    // 0.0 in well-behaved layouts. The 0.05 ceiling is the "Good" CLS
    // threshold per Core Web Vitals — anything under it is shippable
    // and well below "Needs improvement" (0.1).
    expect(cls).toBeLessThan(0.05);
  });
});
