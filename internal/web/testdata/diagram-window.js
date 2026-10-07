// Browser checks for the diagram window (assets/diagram.js). Run by
// TestDiagramWindowBrowser (devserver_test.go, -tags devui), which passes the page URL as
// argv[2]. Needs the `playwright` npm package, found through AGTRACK_PW_DIR, and a Chromium:
// AGTRACK_CHROME names the executable, otherwise Playwright's own download is used.
// Exits non-zero on the first failed check.

const fs = require("fs");
const os = require("os");
const path = require("path");
const { chromium } = require(require.resolve("playwright", { paths: [process.env.AGTRACK_PW_DIR || "."] }));

const url = process.argv[2];
let failed = 0;
const check = (cond, msg) => {
  console.log((cond ? "ok   " : "FAIL ") + msg);
  if (!cond) failed++;
};
const transform = (page) => page.locator(".dgw-stage").evaluate((e) => e.style.transform);

(async () => {
  const browser = await chromium.launch(process.env.AGTRACK_CHROME ? { executablePath: process.env.AGTRACK_CHROME } : {});
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 }, acceptDownloads: true, permissions: ["clipboard-read", "clipboard-write"] });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(url);
  await page.waitForSelector(".diagram.is-expandable");

  const diagrams = page.locator('section[data-section="rollout"] .diagram');
  const dlg = page.locator(".dgw").first();

  // open, and re-open focuses the existing window
  await diagrams.nth(0).click({ position: { x: 5, y: 5 } });
  check((await page.locator(".dgw").count()) === 1, "click opens one window");
  check((await page.locator(".dgw-title").innerText()) === "Diagram 1 · 4 Rollout", "title carries diagram number and section");
  await diagrams.nth(0).click({ position: { x: 5, y: 5 } });
  check((await page.locator(".dgw").count()) === 1, "second click focuses the existing window");

  // minimise, chip, restore
  await dlg.getByRole("button", { name: "Minimise" }).click();
  check(!(await dlg.isVisible()) && (await page.locator(".dgw-chip").count()) === 1, "minimise hides the window and adds a dock chip");
  await page.locator(".dgw-chip").click();
  check((await dlg.isVisible()) && (await page.locator(".dgw-chip").count()) === 0, "chip restores the window");

  // maximise, Esc, close
  await dlg.getByRole("button", { name: "Maximise" }).click();
  const max = await dlg.boundingBox();
  check(max.x === 8 && max.y === 8 && max.width === 1384 && max.height === 884, "maximise fills the viewport minus 8px");
  await page.keyboard.press("Escape");
  check((await dlg.boundingBox()).width < 1384 && (await dlg.isVisible()), "Esc restores from maximise and does not close");

  // pan and zoom change the transform
  const fitted = await transform(page);
  const cv = await dlg.locator(".dgw-canvas").boundingBox();
  await page.mouse.move(cv.x + cv.width / 2, cv.y + cv.height / 2);
  await page.mouse.wheel(0, -300);
  const zoomed = await transform(page);
  check(zoomed !== fitted, "wheel zoom changes the transform");
  await page.mouse.down();
  await page.mouse.move(cv.x + cv.width / 2 + 40, cv.y + cv.height / 2 + 30, { steps: 3 });
  await page.mouse.up();
  check((await transform(page)) !== zoomed, "dragging the canvas pans");

  // source view
  await dlg.getByRole("button", { name: "Source" }).click();
  const src = await dlg.locator(".dgw-source").innerText();
  check(src.startsWith("flowchart TD") && src.includes("Write to new store"), "source view shows the Mermaid source");
  await dlg.getByRole("button", { name: "Copy" }).click();
  check((await page.evaluate(() => navigator.clipboard.readText())).trim() === src.trim(), "Copy puts the source on the clipboard");
  await page.keyboard.press("Escape");
  check(await dlg.locator(".dgw-canvas").isVisible(), "Esc leaves the source view");

  // save PNG, on the diagram with HTML labels
  const [download] = await Promise.all([page.waitForEvent("download"), dlg.getByRole("button", { name: "Save PNG" }).click()]);
  const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "agtrack-dgw-")), "out.png");
  await download.saveAs(file);
  const png = fs.readFileSync(file);
  check(download.suggestedFilename() === "auth-rollout-diagram-1.png", "PNG file name " + download.suggestedFilename());
  check(png.length > 1000 && png.subarray(1, 4).toString() === "PNG", "PNG is a non-empty image/png (" + png.length + " bytes)");

  // close
  await dlg.getByRole("button", { name: "Close" }).click();
  check((await page.locator(".dgw").count()) === 0, "close removes the window");
  check(errors.length === 0, "no page errors" + (errors.length ? ": " + errors.join("; ") : ""));

  await browser.close();
  process.exit(failed ? 1 : 0);
})().catch((err) => {
  console.error(err);
  process.exit(2);
});
