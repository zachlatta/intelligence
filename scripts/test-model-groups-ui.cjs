// Invoked by TestModelGroupsBrowser with an isolated Postgres-backed HTTP fixture.
const { chromium } = require(process.env.MODEL_GROUP_PLAYWRIGHT);
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.MODEL_GROUP_CHROMIUM || undefined,
  });
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    const errors = [],
      failures = [];
    page.on("pageerror", (e) => errors.push(e.message));
    const output = path.resolve(
      __dirname,
      "../.factory-scratch/model-group-layout",
    );
    fs.mkdirSync(output, { recursive: true });
    for (const width of [390, 1440]) {
      await page.setViewportSize({ width, height: 1000 });
      for (const route of [
        "/usage",
        "/workflows/" + process.env.MODEL_GROUP_TEST_WORKFLOW,
        "/test/model-instructions",
        "/workflow-runs/" + process.env.MODEL_GROUP_TEST_RUN,
      ]) {
        await page.goto(process.env.MODEL_GROUP_TEST_URL + route);
        if (route === "/usage") {
          await page.locator("#model-groups [data-edit]").first().waitFor();
          const google = page
            .locator("#model-groups section.card")
            .filter({
              has: page.locator("code", {
                hasText: "{{google_frontier_model}}",
              }),
            });
          const suggested = await google
            .locator(".card-body > p")
            .first()
            .innerText();
          if (
            !suggested.includes("Antigravity plan") ||
            suggested.includes(" · default ·")
          )
            failures.push("suggested model shows raw login instead of label");
          const empty = page
            .locator("#model-groups section.card")
            .filter({
              has: page.locator("code", { hasText: "{{empty_model}}" }),
            });
          if (!(await empty.innerText()).includes("Empty group"))
            failures.push("empty group is not identified clearly");
          const group = page.locator("#model-groups section.card").filter({
            has: page.locator("code", { hasText: "{{frontier_model}}" }),
          });
          await group.locator("[data-edit]").click();
          const modal = page.locator("dialog[open]").last();
          if (await modal.locator("[data-error]").isVisible())
            failures.push("empty error banner visible in group editor");
          await page.screenshot({
            path: path.join(output, "group-editor-" + width + ".png"),
            fullPage: true,
          });
          const login = modal.locator('select[data-field="login"]').first();
          await login.selectOption("");
          await modal
            .getByRole("button", { name: "Save group", exact: true })
            .click();
          assert(
            (await login.evaluate((el) => el.validationMessage)).includes(
              "Choose a plan/login",
            ),
            "blank plan selection must show a clear validation message",
          );
          await modal
            .getByRole("button", { name: "Cancel", exact: true })
            .click();
        } else if (route.startsWith("/workflows/")) {
          await page
            .getByRole("button", { name: "Model groups", exact: true })
            .waitFor();
          await page.locator("#pipe .node[data-id=build]").click();
          await page
            .locator("[data-model-token-chips] [data-token]")
            .first()
            .waitFor();
          const card = page.locator("#pipe .node[data-id=build]");
          if (
            (await card
              .locator(".var", { hasText: "{{frontier_model}}" })
              .count()) !== 2
          )
            failures.push("model tokens are not highlighted");
          const prompt = page.locator("textarea[data-s=instructions]"),
            original = await prompt.inputValue();
          for (const [text, count] of [
            ["Run a worker without a group token.", 0],
            [original, 1],
          ]) {
            await prompt.fill(text);
            await page.locator("#save").click();
            await page.waitForFunction(() =>
              /^v\d+ · saved$/.test(
                document.querySelector("#saved").textContent,
              ),
            );
            await page
              .getByRole("button", { name: "Model groups", exact: true })
              .click();
            const manager = page.locator("dialog[open]").last();
            await manager.locator("[data-edit]").first().waitFor();
            await manager
              .locator("#model-groups > .card-body > section.card")
              .filter({
                has: page.locator("code", { hasText: "{{frontier_model}}" }),
              })
              .locator("[data-edit]")
              .click();
            const editor = page.locator("dialog[open]").last();
            if (
              !(await editor.innerText()).includes(`Used by ${count} workflows`)
            )
              failures.push(
                "stale used-by count after workflow save, expected " + count,
              );
            await editor
              .getByRole("button", { name: "Cancel", exact: true })
              .click();
            await manager
              .getByRole("button", { name: "Close", exact: true })
              .click();
          }
        } else if (route.startsWith("/workflow-runs/")) {
          const text = (await page.locator(".events").allTextContents()).join(
            "\n",
          );
          if (
            !text.includes(
              "Suggested GPT 6.1 Sol (High) on Test plan for {{frontier_model}}",
            ) ||
            text.includes("model_suggestion ") ||
            text.includes("Launch settings:") ||
            text.includes("Before another worker")
          )
            failures.push("run page shows raw model suggestion instructions");
        } else {
          const inline = await page.locator("#model-inline").innerText();
          const choice =
            "GPT 6.1 Sol (High) via codex/gpt-6.1-sol --thinking high on Test plan";
          assert.equal(
            inline,
            `Launch the builder with ${choice} and a second opinion with ${choice}.`,
          );
          const notes = await page.locator("#model-notes").innerText();
          assert.equal(
            notes.split("- frontier_model:").length - 1,
            1,
            "one note for repeated tokens",
          );
          for (const detail of [
            "More quota likely",
            "meter read",
            "Suggestion ",
            "--mode full-access",
          ])
            assert(notes.includes(detail), detail);
        }
        const name =
          (route === "/usage"
            ? "plans"
            : route.startsWith("/workflows/")
              ? "workflow"
              : route.startsWith("/workflow-runs/")
                ? "run"
                : "inline") +
          "-" +
          width;
        await page.screenshot({
          path: path.join(output, name + ".png"),
          fullPage: true,
        });
        const dimensions = await page.evaluate(() => ({
          viewport: innerWidth,
          document: document.documentElement.scrollWidth,
        }));
        if (dimensions.document > width)
          failures.push(
            `${name}: horizontal overflow ${JSON.stringify(dimensions)}`,
          );
        if (route === "/usage") {
          const controls = await page
            .locator("#model-groups [data-edit]")
            .evaluateAll((buttons) =>
              buttons.map((b) => {
                const r = b.getBoundingClientRect(),
                  card = b.closest(".card").getBoundingClientRect();
                return {
                  left: r.left,
                  right: r.right,
                  cardLeft: card.left,
                  cardRight: card.right,
                };
              }),
            );
          for (const r of controls)
            if (r.left < r.cardLeft || r.right > r.cardRight)
              failures.push(
                `${name}: clipped edit control ${JSON.stringify(r)}`,
              );
        }
        console.log(name + ": checked layout");
      }
    }
    assert.deepEqual(errors, [], "browser errors");
    assert.deepEqual(failures, [], "layout regressions");
  } finally {
    await browser.close();
  }
})().catch((e) => {
  console.error(e);
  process.exitCode = 1;
});
