import { expect, test } from "@playwright/test";
import { buildSurfaceFrameDocument } from "../packages/views/plugins/surface-document";

const CONTENT_URL = "https://plugin-content.example.test/plugin-surfaces/opaque";
const BRIDGE_TOKEN = "one-use-proof";

test.describe("plugin surface frame document (real Chromium)", () => {
  test("relays only the valid guest port from the opaque hosted frame", async ({ page }) => {
    let contentRequests = 0;
    await page.route(`${CONTENT_URL}*`, async (route) => {
      contentRequests += 1;
      await route.fulfill({
        contentType: "text/html",
        headers: { "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'" },
        body: `<!doctype html><script>
          function connect(version, challenge, payload) {
            const channel = new MessageChannel();
            channel.port2.postMessage(payload);
            parent.postMessage({ type: "multica:plugin-bridge-connect", version, challenge }, "*", [channel.port1]);
          }
          let opaque = false;
          try { parent.document; } catch (error) { opaque = error.name === "SecurityError"; }
          connect(1, ${JSON.stringify(BRIDGE_TOKEN)}, { invalid: "version" });
          connect(2, "wrong-proof", { invalid: "challenge" });
          connect(2, ${JSON.stringify(BRIDGE_TOKEN)}, { opaque, origin: location.origin });
        </script>`,
      });
    });

    const wrapper = buildSurfaceFrameDocument({ url: CONTENT_URL, bridgeToken: BRIDGE_TOKEN });
    await page.setContent(`<script>
      window.bridgeMessages = [];
      addEventListener("message", event => {
        if (event.data?.type !== "multica:plugin-bridge-connect" || !event.ports[0]) return;
        window.bridgeMessages.push({ version: event.data.version, challenge: event.data.challenge });
        event.ports[0].onmessage = portEvent => window.bridgeMessages.push(portEvent.data);
        event.ports[0].start();
      });
    </script><iframe id="host" sandbox="allow-scripts allow-same-origin"></iframe>`);
    await page.locator("#host").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc as string;
    }, wrapper);

    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { bridgeMessages: unknown[] }).bridgeMessages,
    )).toEqual([
      { version: 2, challenge: BRIDGE_TOKEN },
      { opaque: true, origin: "https://plugin-content.example.test" },
    ]);
    expect(contentRequests).toBe(1);
  });

  test("forwards one terminal guest error and refuses a later connection", async ({ page }) => {
    await page.route(`${CONTENT_URL}*`, async (route) => {
      await route.fulfill({
        contentType: "text/html",
        headers: { "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'" },
        body: `<!doctype html><script>
          parent.postMessage({ type: "multica:plugin-surface-error" }, "*");
          parent.postMessage({ type: "multica:plugin-surface-error" }, "*");
          const channel = new MessageChannel();
          parent.postMessage({
            type: "multica:plugin-bridge-connect",
            version: 2,
            challenge: ${JSON.stringify(BRIDGE_TOKEN)}
          }, "*", [channel.port1]);
        </script>`,
      });
    });

    const wrapper = buildSurfaceFrameDocument({ url: CONTENT_URL, bridgeToken: BRIDGE_TOKEN });
    await page.setContent(`<script>
      window.surfaceState = { terminal: 0, connected: 0 };
      addEventListener("message", event => {
        if (event.data?.type === "multica:plugin-surface-error") window.surfaceState.terminal += 1;
        if (event.data?.type === "multica:plugin-bridge-connect") window.surfaceState.connected += 1;
      });
    </script><iframe id="host" sandbox="allow-scripts allow-same-origin"></iframe>`);
    await page.locator("#host").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc as string;
    }, wrapper);

    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { surfaceState: { terminal: number; connected: number } }).surfaceState,
    )).toEqual({ terminal: 1, connected: 0 });
  });
});