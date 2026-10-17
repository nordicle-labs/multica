import { test, expect } from "@playwright/test";
import { loginAsDefault, waitForPageText } from "./helpers";

async function enableComposioMcpApps(page: import("@playwright/test").Page) {
  await page.route("**/api/config", async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as {
      feature_flags?: Record<string, boolean>;
      [key: string]: unknown;
    };
    await route.fulfill({
      response,
      json: {
        ...body,
        feature_flags: {
          ...body.feature_flags,
          composio_mcp_apps: true,
        },
      },
    });
  });
}

test.describe("Settings", () => {
  test("updating workspace name reflects in sidebar immediately", async ({
    page,
  }) => {
    const workspaceSlug = await loginAsDefault(page);

    // Read the current workspace name from the sidebar
    const sidebarName = page.getByRole("button", { name: /E2E Workspace/ }).first();
    const originalName = (await sidebarName.innerText()).split("\n").pop()?.trim() ?? "E2E Workspace";

    await page.goto(`/${workspaceSlug}/settings?tab=workspace`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, "General");

    const nameInput = page.getByRole("textbox", { name: "Name", exact: true });
    const saveName = async (name: string) => {
      if ((await nameInput.inputValue()) === name) return;
      const saved = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return (
          response.request().method() === "PATCH" &&
          /\/api\/workspaces\/[^/]+$/.test(url.pathname) &&
          response.ok()
        );
      });
      await nameInput.fill(name);
      await nameInput.blur();
      await saved;
      await expect(page.getByText("Saved", { exact: true })).toBeVisible();
    };

    const newName = "Renamed WS " + Date.now();
    try {
      await saveName(newName);
      await expect(
        page.getByRole("button", { name: new RegExp(newName) }).first(),
      ).toBeVisible();
    } finally {
      // Restore the shared workspace even when an assertion above fails.
      await saveName(originalName.trim());
      await expect(
        page.getByRole("button", { name: new RegExp(originalName) }).first(),
      ).toBeVisible();
    }
  });

  // Composio connect flow, fully mocked at the network boundary so it runs
  // without a configured COMPOSIO_API_KEY or a live Composio project. The
  // backend redirect is simulated by pointing the init endpoint's redirect_url
  // straight back at the settings page with ?connected=<slug> — exercising the
  // frontend's callback toast + connections refresh (MUL-3718) end to end.
  test("connecting a Composio toolkit shows a toast and refreshes the list", async ({
    page,
  }) => {
    await enableComposioMcpApps(page);
    const workspaceSlug = await loginAsDefault(page);
    const settingsUrl = `/${workspaceSlug}/settings?tab=integrations&integration=composio`;

    // Stateful: connections is empty until the (mocked) connect flow lands.
    let connected = false;

    await page.route("**/api/integrations/composio/toolkits", (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { slug: "notion", name: "Notion", connectable: true },
        ]),
      }),
    );

    await page.route("**/api/integrations/composio/connections", (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(
          connected
            ? [
                {
                  id: "conn-notion-1",
                  toolkit_slug: "notion",
                  status: "active",
                  connected_at: new Date().toISOString(),
                  last_used_at: null,
                },
              ]
            : [],
        ),
      });
    });

    await page.route("**/api/integrations/composio/connect/init", (route) => {
      // Composio would 302 through its hosted consent and back to our callback,
      // which emits CallbackRedirect's slug-less shape:
      // `/settings?tab=integrations&connected=<slug>`. The web proxy's
      // legacy-route redirect then prepends the last workspace slug, landing on
      // the real settings route. Mock that exact backend shape (NOT the final
      // slugged URL) so the test exercises the same redirect path real users hit.
      connected = true;
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          redirect_url: `/settings?tab=integrations&connected=notion`,
        }),
      });
    });

    await page.goto(settingsUrl, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, "Connected apps");

    // Notion starts disconnected → click Connect.
    await page.getByRole("button", { name: /^Connect$/ }).first().click();

    // Success toast from the simulated callback redirect.
    await expect(
      page.locator("[data-sonner-toast]").getByText("Connected", { exact: true }),
    ).toBeVisible({ timeout: 10000 });

    // List refreshed without a manual reload: the Notion card now offers
    // Disconnect, and the one-shot ?connected param has been stripped.
    await expect(
      page.getByRole("button", { name: /Disconnect/ }).first(),
    ).toBeVisible({ timeout: 10000 });
    await expect(page).not.toHaveURL(/connected=notion/);
  });
});
