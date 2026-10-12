// Browser checks for the split view (docking diagram windows) and the narrow-screen
// drawers. Run by TestSplitViewBrowser (devserver_test.go, -tags devui), which passes the
// page URL as argv[2]. Same requirements as diagram-window.js: the `playwright` npm
// package through AGTRACK_PW_DIR, and AGTRACK_CHROME or Playwright's own Chromium.
// Exits non-zero if any check failed.

const { chromium } = require(require.resolve("playwright", { paths: [process.env.AGTRACK_PW_DIR || "."] }));

const url = process.argv[2];
let failed = 0;
const check = (cond, msg) => {
  console.log((cond ? "ok   " : "FAIL ") + msg);
  if (!cond) failed++;
};
const near = (a, b, tol = 1.5) => Math.abs(a - b) <= tol;
const fmt = (b) => (b ? `${Math.round(b.x)},${Math.round(b.y)} ${Math.round(b.width)}x${Math.round(b.height)}` : "none");
const settle = (page) => page.waitForTimeout(300); // drawer transition (0.2s)

async function openPage(browser, width, height, opts = {}) {
  const ctx = await browser.newContext({ viewport: { width, height }, ...opts });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(url);
  await page.waitForSelector(".diagram.is-expandable");
  return { ctx, page, errors };
}

async function openDiagram(page, n) {
  await page.locator('section[data-section="rollout"] .diagram').nth(n).locator(".diagram-expand").click();
  return page.locator(".dgw").filter({ hasText: "Diagram " + (n + 1) + " ·" });
}

async function splitView(browser) {
  const W = 1400;
  const H = 900;
  const { ctx, page, errors } = await openPage(browser, W, H);
  const outline = page.locator("#outline");
  const rail = page.locator("#rail");
  check((await outline.isVisible()) && (await rail.isVisible()), "wide: outline and notes rail are side by side");
  check(!(await page.locator("#btn-outline").isVisible()), "wide: no Outline drawer button");

  const w1 = await openDiagram(page, 0);
  const float1 = await w1.boundingBox();
  const fit0 = await w1.locator(".dgw-stage").evaluate((e) => e.style.transform);

  // dock right → split view
  await w1.getByRole("button", { name: "Dock right" }).click();
  await settle(page);
  let b1 = await w1.boundingBox();
  let doc = await page.locator("#canvas").boundingBox();
  check(near(b1.width, W * 0.4) && near(b1.x + b1.width, W) && near(b1.y, 56) && near(b1.y + b1.height, H), "dock right fills the right 40% below the top bar (" + fmt(b1) + ")");
  check(doc.width > 300 && doc.x + doc.width <= b1.x + 1, "document and docked window sit side by side (doc " + fmt(doc) + ")");
  check(await page.locator(".dgw-canvas").first().isVisible(), "the docked window's canvas is visible");
  check(!(await outline.isVisible()) && !(await rail.isVisible()), "while docked, outline and notes become closed drawers");
  check(await page.locator("#btn-outline").isVisible(), "while docked, the Outline drawer button shows");
  check((await w1.getByRole("button", { name: "Undock" }).isVisible()) && !(await w1.getByRole("button", { name: "Minimise" }).isVisible()), "docked title bar: Undock instead of Minimise");
  check(await w1.locator(".dgw-grip").isHidden(), "docked window has no resize corner");
  const fit1 = await w1.locator(".dgw-stage").evaluate((e) => e.style.transform);
  check(fit1 !== fit0, "docking re-fits the diagram (" + fit0 + " -> " + fit1 + ")");

  // splitter: drag
  const sp = page.getByRole("separator", { name: /docked right/ });
  check((await sp.getAttribute("aria-valuenow")) === "40", "splitter starts at 40");
  let sb = await sp.boundingBox();
  await page.mouse.move(sb.x + sb.width / 2, sb.y + 200);
  await page.mouse.down();
  await page.mouse.move(W * 0.5, sb.y + 200, { steps: 5 });
  await page.mouse.up();
  b1 = await w1.boundingBox();
  doc = await page.locator("#canvas").boundingBox();
  check((await sp.getAttribute("aria-valuenow")) === "50" && near(b1.width, W * 0.5), "dragging the splitter resizes the window (" + fmt(b1) + ")");
  check(near(doc.x + doc.width, b1.x), "the document follows the splitter");
  const fit2 = await w1.locator(".dgw-stage").evaluate((e) => e.style.transform);
  check(fit2 !== fit1 && (await w1.locator(".dgw-pct").innerText()) !== "", "a fitted diagram re-fits after a splitter move");
  // clamp
  await page.mouse.move(W * 0.5, sb.y + 200);
  await page.mouse.down();
  await page.mouse.move(40, sb.y + 200, { steps: 5 });
  await page.mouse.up();
  check((await sp.getAttribute("aria-valuenow")) === "75", "splitter clamps at 75%");
  // keyboard
  await sp.focus();
  await page.keyboard.press("ArrowRight");
  check((await sp.getAttribute("aria-valuenow")) === "73", "ArrowRight moves the splitter right by 2% (75 -> 73)");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  check((await sp.getAttribute("aria-valuenow")) === "75", "ArrowLeft moves it back, clamped at 75");
  await sp.dblclick();
  check((await sp.getAttribute("aria-valuenow")) === "40", "double-click resets the splitter to 40%");

  // a second window, docked left: both docked, document in the middle
  const w2 = await openDiagram(page, 1);
  const float2 = await w2.boundingBox();
  await w2.getByRole("button", { name: "Dock left" }).click();
  b1 = await w1.boundingBox();
  const b2 = await w2.boundingBox();
  doc = await page.locator("#canvas").boundingBox();
  check(near(b2.x, 0) && near(b2.width, W * 0.4), "dock left fills the left 40% (" + fmt(b2) + ")");
  check(doc.x >= b2.x + b2.width - 1 && doc.x + doc.width <= b1.x + 1 && doc.width > 200, "both sides docked: the document sits in the middle (" + fmt(doc) + ")");
  check((await page.locator(".dgw-splitter").count()) === 2, "one splitter per docked side");

  // docking to an occupied side swaps: Alt+→ with focus in the left window
  await w2.focus();
  await page.keyboard.press("Alt+ArrowRight");
  const r2 = await w2.boundingBox();
  const r1 = await w1.boundingBox();
  check(near(r2.x + r2.width, W) && (await w2.getAttribute("class")).includes("is-docked-right"), "Alt+→ docks the window right");
  check(!(await w1.getAttribute("class")).includes("is-docked") && near(r1.x, float1.x) && near(r1.width, float1.width) && near(r1.height, float1.height), "the window that was on the right goes back to floating at its old geometry (" + fmt(r1) + ")");
  check((await page.locator(".dgw-splitter").count()) === 1 && (await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--dock-left"))) === "", "the left side is empty again");

  // undock restores the floating geometry
  await w2.getByRole("button", { name: "Undock" }).click();
  const u2 = await w2.boundingBox();
  check(near(u2.x, float2.x) && near(u2.y, float2.y) && near(u2.width, float2.width) && near(u2.height, float2.height), "Undock restores the floating position and size (" + fmt(u2) + " vs " + fmt(float2) + ")");
  check(await page.evaluate(() => !document.documentElement.classList.contains("has-docked")), "no docked window: html.has-docked is gone");
  await settle(page);
  check((await outline.isVisible()) && (await rail.isVisible()), "the last undock restores outline and notes rail");

  // Alt+↓ undocks; closing a docked window restores the layout too
  await w2.focus();
  await page.keyboard.press("Alt+ArrowLeft");
  check((await w2.getAttribute("class")).includes("is-docked-left"), "Alt+← docks left");
  await page.keyboard.press("Alt+ArrowDown");
  check(!(await w2.getAttribute("class")).includes("is-docked"), "Alt+↓ undocks");
  await w2.getByRole("button", { name: "Dock right" }).click();
  await w2.getByRole("button", { name: "Close" }).click();
  doc = await page.locator("#canvas").boundingBox();
  check((await page.locator(".dgw-splitter").count()) === 0 && doc.x + doc.width > W - 400, "closing the docked window gives the width back (" + fmt(doc) + ")");

  // shrinking below 900px undocks and hides the dock buttons
  await w1.getByRole("button", { name: "Dock left" }).click();
  await page.setViewportSize({ width: 860, height: H });
  await page.waitForTimeout(100);
  check(!(await w1.getAttribute("class")).includes("is-docked") && (await page.locator(".dgw-splitter").count()) === 0, "below 900px a docked window turns back into a floating window");
  check(!(await w1.getByRole("button", { name: "Dock left" }).isVisible()) && !(await w1.getByRole("button", { name: "Dock right" }).isVisible()), "below 900px the dock buttons are hidden");

  check(errors.length === 0, "split view: no page errors" + (errors.length ? ": " + errors.join("; ") : ""));
  await ctx.close();
}

async function tablet(browser) {
  const { ctx, page, errors } = await openPage(browser, 1100, 800);
  const outline = page.locator("#outline");
  const rail = page.locator("#rail");
  const btnOutline = page.locator("#btn-outline");
  const btnRail = page.locator("#btn-rail");
  const scrim = page.locator("#scrim");
  check(!(await outline.isVisible()) && !(await rail.isVisible()), "1100px: outline and notes start closed");
  const doc = await page.locator("#canvas").boundingBox();
  check(near(doc.x, 0) && near(doc.width, 1100, 20), "1100px: the document takes the full width (" + fmt(doc) + ")");
  check((await btnOutline.isVisible()) && (await btnRail.isVisible()), "1100px: Outline and Notes buttons show");
  check(/\d/.test(await page.locator("#btn-rail-count").innerText()), "the Notes button carries the open-notes count");

  await btnOutline.click();
  await settle(page);
  check((await outline.isVisible()) && (await btnOutline.getAttribute("aria-expanded")) === "true", "Outline opens the outline drawer");
  check(await scrim.isVisible(), "an open drawer has a scrim");
  check(await page.evaluate(() => document.getElementById("outline").contains(document.activeElement)), "focus moves into the outline drawer");
  await btnRail.click();
  await settle(page);
  check((await rail.isVisible()) && !(await outline.isVisible()), "opening Notes closes the outline: one drawer at a time");
  check(await page.evaluate(() => document.getElementById("rail").contains(document.activeElement)), "focus moves into the notes drawer");
  await page.keyboard.press("Escape");
  await settle(page);
  check(!(await rail.isVisible()) && !(await scrim.isVisible()), "Esc closes the drawer");
  check(await page.evaluate(() => document.activeElement && document.activeElement.id === "btn-rail"), "focus returns to the Notes button");
  check((await btnRail.getAttribute("aria-expanded")) === "false", "aria-expanded follows the drawer");

  await btnOutline.click();
  await settle(page);
  const sb = await scrim.boundingBox();
  await page.mouse.click(sb.x + sb.width - 20, sb.y + sb.height / 2);
  await settle(page);
  check(!(await outline.isVisible()), "clicking the scrim closes the drawer");
  await btnOutline.click();
  await settle(page);
  await page.locator('#toc .toc-item[data-slug="rollout"]').click();
  await settle(page);
  check(!(await outline.isVisible()), "choosing a section closes the outline drawer");

  // select text in the document: the Comment button opens the Notes drawer with the composer
  await page.evaluate(() => {
    const p = document.querySelector('section[data-section="rollout"] p');
    p.scrollIntoView({ block: "center" });
    const r = document.createRange();
    r.selectNodeContents(p);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
  });
  const fab = page.locator("#comment-fab");
  await fab.waitFor({ state: "visible", timeout: 3000 }).catch(() => {});
  check(await fab.isVisible(), "selecting text shows the Comment button");
  await fab.click();
  await settle(page);
  check((await rail.isVisible()) && (await page.locator("#comment-dialog").isVisible()), "Comment opens the Notes drawer and the composer");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await settle(page);
  check(!(await rail.isVisible()) && (await page.locator("#comment-dialog").isHidden()), "Esc closes the composer, then the drawer");

  check(errors.length === 0, "tablet: no page errors" + (errors.length ? ": " + errors.join("; ") : ""));
  await ctx.close();
}

async function phone(browser) {
  const { ctx, page, errors } = await openPage(browser, 390, 844, { hasTouch: true, isMobile: true });
  const outline = page.locator("#outline");
  const rail = page.locator("#rail");
  await page.locator("#btn-outline").click();
  await settle(page);
  let b = await outline.boundingBox();
  check(near(b.x, 0) && near(b.width, 390), "390px: the outline drawer is full width (" + fmt(b) + ")");
  await page.keyboard.press("Escape");
  await page.locator("#btn-rail").click();
  await settle(page);
  b = await rail.boundingBox();
  check(near(b.x, 0) && near(b.width, 390), "390px: the notes drawer is full width (" + fmt(b) + ")");
  await page.keyboard.press("Escape");
  await settle(page);

  await page.evaluate(() => {
    const p = document.querySelector('section[data-section="rollout"] p');
    p.scrollIntoView({ block: "center" });
    const r = document.createRange();
    r.selectNodeContents(p);
    window.getSelection().removeAllRanges();
    window.getSelection().addRange(r);
  });
  const fab = page.locator("#comment-fab");
  await fab.waitFor({ state: "visible", timeout: 3000 }).catch(() => {});
  await fab.click();
  await settle(page);
  const sheet = await page.locator("#comment-form").boundingBox();
  check(near(sheet.x, 0) && near(sheet.width, 390) && near(sheet.y + sheet.height, 844), "390px: the composer is a bottom sheet (" + fmt(sheet) + ")");
  await page.keyboard.press("Escape");
  await page.keyboard.press("Escape");
  await settle(page);

  const w = await openDiagram(page, 0);
  b = await w.boundingBox();
  check((await w.getAttribute("class")).includes("is-max") && near(b.x, 8) && near(b.width, 390 - 16), "390px: a floating window opens maximised (" + fmt(b) + ")");
  check(!(await w.getByRole("button", { name: "Dock left" }).isVisible()) && !(await w.getByRole("button", { name: "Dock right" }).isVisible()), "390px: no dock buttons");
  await page.keyboard.press("Escape");
  b = await w.boundingBox();
  check(b.width >= 320 && b.x >= 0 && b.x + b.width <= 390 + 1, "Restore returns it to a window that fits the screen (" + fmt(b) + ")");

  check(errors.length === 0, "phone: no page errors" + (errors.length ? ": " + errors.join("; ") : ""));
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch(process.env.AGTRACK_CHROME ? { executablePath: process.env.AGTRACK_CHROME } : {});
  await splitView(browser);
  await tablet(browser);
  await phone(browser);
  await browser.close();
  process.exit(failed ? 1 : 0);
})().catch((err) => {
  console.error(err);
  process.exit(2);
});
