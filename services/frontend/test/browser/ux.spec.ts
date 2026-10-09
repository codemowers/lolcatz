import { test, expect, type Page, type Route } from "@playwright/test";

const boards = [{ id: "b", name: "Random", icon: "🎲", blurb: "Anything goes", count: 0 }];
const post = { id: "example", board: "b", title: "Example post", filename: "example.jpg", author: "Alice", user_id: 1,
  uploaded_at: "2026-09-23T09:00:00Z", image_url: "/github.svg", annotations: [] };

async function mockApp(page: Page) {
  const state = { token: "first-token", adminStatus: 200, error: undefined as string | undefined };
  await page.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/auth/session") return route.fulfill({ json: {
      user: { name: "Alice", email: "alice@example.test" }, accessToken: state.token,
      error: state.error,
      oidcAttributes: { subject: "alice" }, expires: "2099-01-01T00:00:00Z",
    } });
    if (path === "/api/admin/me") return route.fulfill({ status: state.adminStatus, json: { role: "admin" } });
    if (path === "/api/browse/boards") return route.fulfill({ json: boards });
    if (path === "/api/browse/images/example") return route.fulfill({ json: { image: post, comments: [] } });
    if (path === "/api/upload/me") return route.fulfill({ json: { id: 1 } });
    return route.fulfill({ json: [] });
  });
  return state;
}

test("direct uploads send the signed storage headers before confirmation", async ({ page }) => {
  await mockApp(page);
  let uploaded = false;
  let confirmed = false;
  await page.route("**/api/upload/presign", route => route.fulfill({ json: {
    id: "new-image", put_url: "http://127.0.0.1:3101/storage-upload",
    put_headers: { "Content-Type": "image/png", "X-Amz-Meta-Lolcatz-Owner": "42" },
    board: "b", title: "Cat", filename: "cat.png",
  } }));
  await page.route("**/storage-upload", async route => {
    expect(route.request().method()).toBe("PUT");
    expect(route.request().headers()["x-amz-meta-lolcatz-owner"]).toBe("42");
    expect(route.request().headers()["content-type"]).toBe("image/png");
    uploaded = true;
    await route.fulfill({ status: 200 });
  });
  await page.route("**/api/upload/confirm", async route => {
    expect(uploaded).toBe(true);
    expect(route.request().postDataJSON()).toEqual({ id: "new-image", board: "b", title: "Cat", filename: "cat.png" });
    confirmed = true;
    await route.fulfill({ status: 201, json: { id: "new-image" } });
  });
  await page.goto("/b");
  await page.getByRole("button", { name: "Post something to /b/" }).click();
  await page.getByLabel("Images", { exact: true }).setInputFiles({ name: "cat.png", mimeType: "image/png", buffer: Buffer.from("image bytes") });
  await page.getByRole("button", { name: "Post", exact: true }).click();
  await expect(page.getByText("Batch finished")).toBeVisible();
  expect(confirmed).toBe(true);
  await expect(page.locator(".upload-item-done")).toHaveCount(1);
});

test("a transient refresh failure recovers without signing in again", async ({ page }) => {
  const state = await mockApp(page);
  state.token = "";
  state.error = "RefreshTokenRetry";
  await page.goto("/profile");
  await expect(page.locator(".auth-prompt")).toContainText("retry automatically");
  await expect(page.getByRole("button", { name: "Sign in again" })).toHaveCount(0);
  state.token = "renewed-token";
  state.error = undefined;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect(page.getByRole("tab", { name: "Account" })).toBeVisible();
});

test("board form survives token refresh and a failed access check", async ({ page }) => {
  const state = await mockApp(page);
  await page.goto("/profile/boards");
  await page.getByLabel("Slug").fill("cats");
  await page.getByLabel("Name", { exact: true }).fill("Cats");
  state.token = "renewed-token";
  let paused: Route | undefined;
  await page.route("**/api/admin/me", route => { paused = route; });
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await expect.poll(() => Boolean(paused)).toBe(true);
  await expect(page.getByRole("button", { name: "Add board" })).toBeDisabled();
  await expect(page.getByLabel("Name", { exact: true })).toHaveValue("Cats");
  await paused!.fulfill({ status: 503, body: "Unavailable" });
  await expect(page.locator("main").getByRole("alert")).toContainText("administration is unavailable");
  await expect(page.getByLabel("Slug")).toHaveValue("cats");
  await page.unroute("**/api/admin/me");
  await page.getByRole("button", { name: "Try again" }).click();
  await expect(page.getByRole("button", { name: "Add board" })).toBeEnabled();
  await expect(page.getByLabel("Name", { exact: true })).toHaveValue("Cats");
});

test("admin denial and expired login have explicit states", async ({ page }) => {
  const state = await mockApp(page);
  state.adminStatus = 403;
  await page.goto("/profile/boards");
  await expect(page.getByText("You do not have access to manage boards.")).toBeVisible();
  await expect(page.getByLabel("Slug")).toHaveCount(0);
  state.adminStatus = 401;
  await page.reload();
  await expect(page.getByText("Your session has expired.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign in", exact: true })).toBeVisible();
});

test("admin controls wait for the availability probe", async ({ page }) => {
  await mockApp(page);
  let pending: Route | undefined;
  await page.route("**/api/admin/me", route => { pending = route; });
  await page.goto("/profile");
  const control = page.locator("main .admin-action");
  await expect(control.getByRole("button", { name: "Manage boards" })).toBeDisabled();
  await control.focus();
  await expect(control.getByRole("tooltip")).toHaveText("Checking board administration availability…");
  await expect.poll(() => Boolean(pending)).toBe(true);
  await pending!.fulfill({ json: { role: "admin" } });
  await expect(page.locator("main").getByRole("link", { name: "Manage boards" })).toBeVisible();
});

for (const failure of [404, 503, "network"] as const) {
  test(`admin controls are disabled when administration is unavailable (${failure})`, async ({ page }) => {
    const state = await mockApp(page);
    await page.route("**/api/admin/me", route => failure === "network"
      ? route.abort("failed") : route.fulfill({ status: failure, body: "Unavailable" }));
    await page.goto("/profile");
    await expect(page.getByRole("button", { name: "Manage boards" })).toHaveCount(2);
    const control = page.locator("main .admin-action");
    await expect(control.getByRole("button")).toBeDisabled();
    await expect(page.getByRole("link", { name: "Manage boards" })).toHaveCount(0);
    await control.hover();
    await expect(control.getByRole("tooltip")).toHaveText("Board administration is currently unavailable.");
    await control.focus();
    await expect(control.getByRole("tooltip")).toBeVisible();
    state.adminStatus = 200;
    await page.unroute("**/api/admin/me");
    await page.getByRole("button", { name: "Try again" }).click();
    await expect(page.getByRole("link", { name: "Manage boards" })).toHaveCount(2);
  });
}

test("admin controls are hidden without permission", async ({ page }) => {
  const state = await mockApp(page);
  state.adminStatus = 403;
  await page.goto("/profile");
  await expect(page.getByText("You do not have access to manage boards.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Manage boards" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "Manage boards" })).toHaveCount(0);
});

test("rejected replies preserve the draft and block duplicate submissions", async ({ page }) => {
  await mockApp(page);
  let attempts = 0;
  let pending: Route | undefined;
  await page.route("**/api/comments/**", route => { attempts++; pending = route; });
  await page.goto("/thread/example");
  await page.getByRole("textbox", { name: "Reply" }).fill("Keep this draft");
  await page.getByRole("button", { name: "Post reply" }).click();
  await expect.poll(() => attempts).toBe(1);
  await expect(page.getByRole("button", { name: "Posting…" })).toBeDisabled();
  await page.locator("form.composer-body").evaluate(form => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  await pending!.fulfill({ status: 500, body: "Unavailable" });
  await expect(page.locator("main").getByRole("alert")).toContainText("unavailable");
  await expect(page.getByRole("textbox", { name: "Reply" })).toHaveValue("Keep this draft");
  expect(attempts).toBe(1);
  await page.route("**/api/comments/**", route => route.fulfill({ status: 201, json: { id: 1 } }));
  await page.getByRole("button", { name: "Post reply" }).click();
  await expect(page.getByRole("textbox", { name: "Reply" })).toHaveValue("");
});

test("a stale search cannot replace newer results", async ({ page }) => {
  await mockApp(page);
  let oldSearch: Route | undefined;
  await page.route("**/api/search/search?**", route => {
    const q = new URL(route.request().url()).searchParams.get("q");
    if (q === "old") { oldSearch = route; return; }
    return route.fulfill({ json: [{ ...post, id: "new", title: "New result" }] });
  });
  await page.goto("/search?q=old");
  await expect.poll(() => Boolean(oldSearch)).toBe(true);
  await page.getByRole("textbox", { name: "Search query" }).fill("new");
  await page.getByRole("button", { name: "Go", exact: true }).click();
  await expect(page.locator(".card-heading")).toHaveText("New result");
  await oldSearch!.fulfill({ json: [{ ...post, title: "Old result" }] });
  await expect(page.locator(".card-heading")).toHaveText("New result");
});

for (const [path, endpoint] of [["/", "/api/browse/boards"], ["/b", "/api/browse/boards/b"],
  ["/search?q=cats", "/api/search/search?q=cats"], ["/thread/missing", "/api/browse/images/missing"]]) {
  test(`${path} shows a recoverable load failure`, async ({ page }) => {
    await mockApp(page);
    await page.route(`**${endpoint}`, route => route.fulfill({ status: 503, body: "Unavailable" }));
    await page.goto(path);
    await expect(page.locator("main").getByRole("alert")).toContainText("unavailable");
    await expect(page.getByRole("button", { name: "Try again" })).toBeVisible();
    await expect(page.getByText("Nothing found.", { exact: true })).toHaveCount(0);
  });
}

test("profile sections are bookmarkable and management is visible on mobile", async ({ page }) => {
  await mockApp(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/profile?section=settings");
  await expect(page.getByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
  await page.screenshot({ path: test.info().outputPath("settings-light.png"), fullPage: true, animations: "disabled" });
  await page.getByRole("button", { name: "Dark", exact: false }).click();
  await page.screenshot({ path: test.info().outputPath("settings-dark.png"), fullPage: true, animations: "disabled" });
  await page.getByRole("tab", { name: "Uploads" }).click();
  await expect(page).toHaveURL(/section=uploads/);
  await page.reload();
  await expect(page.getByRole("tab", { name: "Uploads" })).toHaveAttribute("aria-selected", "true");
  await page.locator("header").getByRole("link", { name: "Manage boards" }).click();
  await expect(page.getByLabel("Slug")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test("cards show reply counts and link to the replies", async ({ page }) => {
  await mockApp(page);
  await page.route("**/api/browse/boards/b", route => route.fulfill({ json: [
    { ...post, comment_count: 2 }, { ...post, id: "empty", comment_count: 0 },
  ] }));
  await page.goto("/b");
  const replies = page.getByRole("link", { name: "2 replies", exact: true });
  await expect(replies).toHaveText("2");
  await expect(page.getByRole("link", { name: "0 replies", exact: true })).toHaveText("0");
  await replies.click();
  await expect(page).toHaveURL(/\/thread\/example#replies$/);
  await expect(page.locator("#replies")).toBeInViewport();
});

test("board wall scrolls through 427 uploads and retries a failed page", async ({ page }) => {
  test.setTimeout(90_000);
  await mockApp(page);
  const posts = Array.from({ length: 427 }, (_, index) => ({
    ...post, id: `wall-${427 - index}`, title: `Upload ${427 - index}`,
    uploaded_at: `2026-09-26T12:00:${String(59 - Math.floor(index / 50)).padStart(2, "0")}.123456Z`,
  }));
  const cursors: string[] = [];
  let fail = true;
  await page.route("**/api/browse/boards/b*", route => {
    const query = new URL(route.request().url()).searchParams;
    const cursor = query.get("before_id");
    cursors.push(cursor || "first");
    const index = cursor ? posts.findIndex(post => post.id === cursor) + 1 : 0;
    if (cursor) expect(query.get("before")).toBe(posts[index - 1].uploaded_at);
    if (index === 25 && fail) {
      fail = false;
      return route.fulfill({ status: 503, body: "Unavailable" });
    }
    return route.fulfill({ json: posts.slice(index, index + 25) });
  });
  await page.goto("/b");
  await expect(page.locator(".feed .card")).toHaveCount(25);
  await page.locator(".wall-status").scrollIntoViewIfNeeded();
  await expect(page.locator("main").getByRole("alert")).toContainText("unavailable");
  await expect(page.locator(".feed .card")).toHaveCount(25);
  await page.getByRole("button", { name: "Try again" }).click();
  await expect(page.locator(".feed .card")).toHaveCount(50);
  for (let count = 75; count < 450; count += 25) {
    await page.locator(".wall-status").scrollIntoViewIfNeeded();
    await expect(page.locator(".feed .card")).toHaveCount(count);
  }
  await page.locator(".wall-status").scrollIntoViewIfNeeded();
  await expect(page.locator(".feed .card")).toHaveCount(427);
  await expect(page.getByRole("status")).toHaveText("All 427 posts loaded.");
  expect(await page.locator(".card-heading").allTextContents()).toEqual(posts.map(post => post.title));
  // Development StrictMode may abort and repeat the initial request.
  const continuations = cursors.filter(cursor => cursor !== "first");
  expect(continuations).toHaveLength(18); // 17 continuations and one retry.
  expect(continuations[0]).toBe(continuations[1]);
});

test("changing boards discards an in-flight wall page", async ({ page }) => {
  await mockApp(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route("**/api/browse/boards", route => route.fulfill({ json: [
    ...boards, { id: "cats", name: "Cats", icon: "🐱", count: 1 },
  ] }));
  let pending: Route | undefined;
  await page.route("**/api/browse/boards/b*", route => {
    if (new URL(route.request().url()).search) { pending = route; return; }
    return route.fulfill({ json: Array.from({ length: 25 }, (_, index) => ({ ...post, id: `b-${index}` })) });
  });
  await page.route("**/api/browse/boards/cats", route => route.fulfill({ json: [{ ...post, id: "cat", board: "cats", title: "Cat board" }] }));
  await page.goto("/b");
  await expect(page.locator(".feed .card")).toHaveCount(25);
  await page.locator(".wall-status").scrollIntoViewIfNeeded();
  await expect.poll(() => Boolean(pending)).toBe(true);
  await page.locator('header a[href="/cats"]').click();
  await expect(page.locator(".card-heading")).toHaveText(["Cat board"]);
  await pending!.fulfill({ json: [{ ...post, id: "stale", title: "Old board response" }] });
  await expect(page.locator(".card-heading")).toHaveText(["Cat board"]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.screenshot({ path: test.info().outputPath("board-wall-mobile.png"), animations: "disabled" });
});


test("cards use thumbnails, threads use uncropped previews and offer original downloads", async ({ page }) => {
  await mockApp(page);
  await page.route("**/api/browse/boards/b", route => route.fulfill({ json: [post] }));
  await page.route("**/api/thumbnails/**", route => route.fulfill({
    contentType: "image/svg+xml",
    body: '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"><rect width="256" height="256" fill="orange"/></svg>',
  }));
  await page.goto("/b");
  const card = page.locator(".card-media img");
  await expect(card).toHaveAttribute("src", "/api/thumbnails/v1/example?size=256");
  await expect(card).toHaveAttribute("srcset", /size=512 2x/);
  await page.locator(".card-title").click();
  const preview = page.locator(".thread-media img");
  await expect(preview).toHaveAttribute("src", "/api/thumbnails/v1/example?size=1024");
  const download = page.getByRole("link", { name: "Download original" });
  await expect(download).toHaveAttribute("href", "/api/browse/media/example?download=1");
  await expect(download).toHaveAttribute("download", "example.jpg");
  await page.route("**/api/thumbnails/**", route => route.fulfill({ status: 503 }));
  await page.reload();
  await expect(page.locator(".thread-media img")).toHaveAttribute("src", "/github.svg");
});
