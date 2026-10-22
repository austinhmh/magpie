const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: ReClaude source is opt-in without a Claude OAuth sign-in`, async (suite) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    suite.after(() => browser.close());
    for (const language of ["en", "zh", "zh-TW", "ja", "de"]) {
      for (const width of [420, 1100]) {
        await suite.test(`${language} ${width}`, async () => {
          const context = await browser.newContext({ viewport: { width, height: 900 }, reducedMotion: "reduce" });
          try {
            const page = await context.newPage();
            page.setDefaultTimeout(5000);
            const errors = [];
            const posts = [];
            page.on("pageerror", (error) => errors.push(error.message));
            let enabled = false;
            let daemonReady = true;
            let failNextSave = false;
            await page.route("**/*", async (route) => {
              const request = route.request();
              const url = new URL(request.url());
              const json = (value) => route.fulfill({ json: value });
              if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:${JSON.stringify(language)},theme:"light",web:false};` });
              if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
              if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang: language, theme: "light" } });
              if (url.pathname === "/api/providers/reclaude") {
                posts.push(request.postDataJSON());
                if (failNextSave) {
                  failNextSave = false;
                  return route.fulfill({ status: 500, json: { error: "fixture save failure" } });
                }
                enabled = posts.at(-1).on;
              }
              if (url.pathname === "/api/providers" || url.pathname === "/api/providers/reclaude") {
                return json({ reclaude: { on: enabled, ready: enabled && daemonReady, why: enabled && !daemonReady ? "ReClaude daemon is not running and reachable" : "" }, gateway: {}, presets: [], excluded: [], providers: enabled ? [
                  { id: "claude", name: "ReClaude", icon: "claudecode-color", account: { agent: "claude", user: "ReClaude", plan: "ReClaude", reclaude: true, logins: [{ user: "ReClaude", active: true, on: true }] }, models: [], agents: [] },
                ] : [] });
              }
              if (url.pathname === "/api/settings") return json({ theme: "light", lang: language });
              if (url.pathname === "/api/usage/quotas") return json([]);
              if (url.pathname === "/api/plugins") return json({ plugins: [] });
              if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
              if (url.pathname.startsWith("/api/")) return json({});
              const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
              const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
              return route.fulfill({ body: await fs.readFile(file), contentType });
            });
            await page.goto("http://magpie.test/?view=providers");
            const toggle = page.locator("#reclaudeSwitch");
            await toggle.waitFor();
            assert.equal(await toggle.getAttribute("aria-pressed"), "false");
            assert(await page.locator("#reclaudeSource").isVisible(), "source must show without a provider");
            await toggle.click();
            await page.waitForFunction(() => document.querySelector("#reclaudeSwitch")?.getAttribute("aria-pressed") === "true");
            assert.deepEqual(posts, [{ on: true }]);
            assert.equal(await toggle.isEnabled(), true, "a successful save must re-enable the switch");
            failNextSave = true;
            const failedSave = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/providers/reclaude" && response.status() === 500);
            await toggle.click();
            await failedSave;
            await page.waitForFunction(() => !document.querySelector("#reclaudeSwitch")?.disabled);
            assert.equal(await toggle.getAttribute("aria-pressed"), "true", "a failed save must keep the confirmed source");
            await toggle.click();
            await page.waitForFunction(() => document.querySelector("#reclaudeSwitch")?.getAttribute("aria-pressed") === "false" && !document.querySelector("#reclaudeSwitch").disabled);
            assert.equal(await page.locator('#providers .provider[data-id="claude"]').count(), 0);
            await toggle.click();
            await page.waitForFunction(() => document.querySelector("#reclaudeSwitch")?.getAttribute("aria-pressed") === "true" && !document.querySelector("#reclaudeSwitch").disabled);
            assert.deepEqual(posts, [{ on: true }, { on: false }, { on: false }, { on: true }]);
            assert.equal(await page.locator("#reclaudeSource .name").textContent() === "Use local ReClaude for Claude models", language === "en");
            await page.locator('#providers .provider[data-id="claude"]').click();
            assert.equal(await page.locator(".accts .acc.add").count(), 0, "no OAuth sign-in action");
            assert.equal(await page.locator(".accts .acc button").count(), 0, "no account switch/remove action");
            daemonReady = false;
            await page.reload();
            await page.waitForFunction(() => document.querySelector("#reclaudeSwitch")?.getAttribute("aria-pressed") === "true");
            assert((await page.locator("#reclaudeReason").textContent()).includes("daemon"), "daemon failure must be explained");
            assert(await page.locator('#providers .provider[data-id="claude"] .key').evaluate((badge) => badge.classList.contains("none")), "unavailable source must not look ready");
            const overflows = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
            assert.equal(overflows, false, "source row must fit narrow windows");
            assert.deepEqual(errors, []);
          } finally {
            await context.close();
          }
        });
      }
    }
  });
}
