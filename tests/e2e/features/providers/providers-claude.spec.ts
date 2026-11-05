import { expect, test } from "../../core/fixtures/base.fixture";


test("Claude exposes native Messages guidance without API key entry", async ({ page }) => {
  const provider = { name: "claude", keys: [], network_config: { max_retries: 0 }, concurrency_and_buffer_size: {}, provider_status: "active" };
  await page.route("**/api/providers", route => route.fulfill({ json: { providers: [provider], total: 1 } }));
  await page.route("**/api/providers/claude", route => route.fulfill({ json: provider }));
  await page.goto("/workspace/providers");
  const guidance = page.getByTestId("claude-provider-guidance");
  await expect(guidance).toBeVisible();
  await expect(guidance.getByText("Claude", { exact: true })).toBeVisible();
  await expect(guidance).toContainText("/v1/messages");
  await expect(guidance).toContainText("Each request is independent, with no saved conversation");
  await expect(guidance).not.toContainText("x-bifrost-claude-session-id");
  await expect(guidance).toContainText("caller-managed tools are not supported");
  await expect(page.getByRole("button", { name: "Add new key", exact: true })).toHaveCount(0);
  await expect(guidance.getByRole("button", { name: "Edit provider configuration" })).toBeVisible();
});
