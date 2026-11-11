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
      ]) {
        await page.goto(process.env.MODEL_GROUP_TEST_URL + route);
        if (route === "/usage")
          await page.locator("#model-groups [data-edit]").first().waitFor();
        else if (route.startsWith("/workflows/")) {
          await page
            .getByRole("button", { name: "Model groups", exact: true })
            .waitFor();
          await page.locator("#pipe .node[data-id=build]").click();
          await page
            .locator("[data-model-token-chips] [data-token]")
            .first()
            .waitFor();
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
