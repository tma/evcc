import { test, expect } from "@playwright/test";
import { start, stop, restart, baseUrl } from "./evcc";

test.use({ baseURL: baseUrl() });
test.describe.configure({ mode: "parallel" });

test.beforeEach(async () => {
  await start("battery-settings.evcc.yaml");
});
test.afterEach(async () => {
  await stop();
});

test.describe("battery settings", async () => {
  test("battery view", async ({ page }) => {
    await page.goto("/#/battery");

    await expect(page.getByRole("heading", { name: "Home Battery" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Grid charging" })).toBeVisible();
    await expect(page.getByTestId("header")).toContainText("Home Battery");
    await expect(page.locator("body")).toContainText("Battery level: 50%");
    await expect(page.locator("body")).toContainText("10.0 kWh of 20.0 kWh");
  });

  test("battery usage", async ({ page }) => {
    await page.goto("/#/battery");

    await page.locator("#batterySettingsPriority").selectOption({ label: "50%" });
    await expect(page.locator("label[for=batterySettingsPriorityMiddle] span")).toHaveText("50%");
    await expect(page.locator("label[for=batterySettingsPriorityBottom] span")).toHaveText("50%");
    const reserve = page.getByTestId("battery-reserve").getByRole("combobox").first();
    const solarSupport = page.getByLabel("Battery-supported solar charging");
    await expect(reserve).toHaveValue("100");
    await expect(solarSupport).toHaveValue("false");
    await expect(page.getByText("Start automatically")).toBeHidden();

    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/80"),
      reserve.selectOption("80"),
    ]);
    await expect(solarSupport).toHaveValue("false");
    await expect(page.getByText("Start automatically")).toBeHidden();

    await Promise.all([
      page.waitForResponse("**/api/batterysolarsupport/true"),
      solarSupport.selectOption("true"),
    ]);
    await expect(page.getByText("Start automatically")).toBeVisible();

    await Promise.all([
      page.waitForResponse("**/api/bufferstartsoc/90"),
      page.getByLabel("Start automatically").selectOption("90"),
    ]);
    await expect(reserve.getByRole("option", { name: "0%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );
    await expect(reserve.getByRole("option", { name: "100%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );

    const priority = page.locator("#batterySettingsPriority");
    await Promise.all([page.waitForResponse("**/api/prioritysoc/80"), priority.selectOption("80")]);
    const [startResponse, reserveResponse] = await Promise.all([
      page.waitForResponse("**/api/bufferstartsoc/95"),
      page.waitForResponse("**/api/batteryreservesoc/95"),
      priority.selectOption("95"),
    ]);
    expect(startResponse.ok()).toBe(true);
    expect(reserveResponse.ok()).toBe(true);
    await expect(reserve).toHaveValue("95");

    await Promise.all([
      page.waitForResponse("**/api/batterysolarsupport/false"),
      solarSupport.selectOption("false"),
    ]);
    await expect(page.getByText("Start automatically")).toBeHidden();
    await expect(reserve).toHaveValue("95");
    const state = await page.request.get("/api/state");
    expect((await state.json()).bufferSoc).toBe(0);
  });

  test("battery support mode persists", async ({ page }) => {
    await page.goto("/#/battery");

    const reserve = page.getByTestId("battery-reserve").getByRole("combobox").first();
    const mode = page.getByLabel("Home battery support during fast and planned charging");
    await expect(mode).toHaveValue("allow");
    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/80"),
      reserve.selectOption("80"),
    ]);
    await Promise.all([
      page.waitForResponse("**/api/batterydischargemode/reserve"),
      mode.selectOption("reserve"),
    ]);
    await expect(mode).toHaveValue("reserve");

    await restart("battery-settings.evcc.yaml");
    await page.goto("/#/battery");
    await expect(
      page.getByLabel("Home battery support during fast and planned charging")
    ).toHaveValue("reserve");
  });

  test("reserve mode needs a usable reserve", async ({ page }) => {
    await page.goto("/#/battery");

    const reserve = page.getByTestId("battery-reserve").getByRole("combobox").first();
    const mode = page.getByLabel("Home battery support during fast and planned charging");
    const reserveOption = mode.getByRole("option", { name: "down to the reserve" });
    const note = page.getByText("Reserve mode needs a reserve between 0% and 100%");

    await expect(reserve).toHaveValue("100");
    await expect(mode).toHaveValue("allow");
    await expect(reserveOption).toHaveAttribute("disabled", "");
    await expect(
      mode.getByRole("option", { name: "without an additional limit" })
    ).not.toHaveAttribute("disabled");
    await expect(mode.getByRole("option", { name: "not at all" })).not.toHaveAttribute("disabled");
    await expect(note).toBeHidden();

    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/80"),
      reserve.selectOption("80"),
    ]);
    await expect(reserveOption).not.toHaveAttribute("disabled");

    await Promise.all([
      page.waitForResponse("**/api/batterydischargemode/reserve"),
      mode.selectOption("reserve"),
    ]);
    await expect(mode).toHaveValue("reserve");
    await expect(note).toBeHidden();

    const modePosts: string[] = [];
    page.on("request", (req) => {
      if (req.url().includes("/api/batterydischargemode/")) {
        modePosts.push(req.url());
      }
    });

    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/100"),
      reserve.selectOption("100"),
    ]);
    await expect(mode).toHaveValue("reserve");
    await expect(reserveOption).toHaveAttribute("disabled", "");
    await expect(note).toBeVisible();
    expect(modePosts).toEqual([]);

    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/0"),
      reserve.selectOption("0"),
    ]);
    await expect(mode).toHaveValue("reserve");
    await expect(reserveOption).toHaveAttribute("disabled", "");
    await expect(note).toBeVisible();
    expect(modePosts).toEqual([]);

    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/50"),
      reserve.selectOption("50"),
    ]);
    await expect(mode).toHaveValue("reserve");
    await expect(reserveOption).not.toHaveAttribute("disabled");
    await expect(note).toBeHidden();
    expect(modePosts).toEqual([]);
  });

  test("legacy battery support settings migrate", async ({ page }) => {
    await stop();
    await start("battery-settings.evcc.yaml", "battery-discharge-legacy.sql");
    await page.goto("/#/battery");

    const mode = page.getByLabel("Home battery support during fast and planned charging");
    await expect(mode).toHaveValue("prevent");
  });

  test("legacy buffer changes override split settings", async ({ page }) => {
    await stop();
    await start("battery-settings.evcc.yaml", "battery-buffer-downgrade.sql");
    await page.goto("/#/battery");

    await expect(page.getByTestId("battery-reserve").getByRole("combobox").first()).toHaveValue(
      "80"
    );
    await expect(page.getByLabel("Battery-supported solar charging")).toHaveValue("false");
  });

  test("invalid solar settings do not stop restore", async ({ page }) => {
    await stop();
    await start("battery-settings.evcc.yaml", "battery-reserve-invalid.sql");
    await page.goto("/#/battery");

    await expect(page.getByLabel("Battery-supported solar charging")).toHaveValue("false");
    await expect(
      page.getByLabel("Home battery support during fast and planned charging")
    ).toHaveValue("prevent");
  });

  test("grid charging", async ({ page }) => {
    await page.goto("/#/battery");

    await page.getByLabel("Enable limit").check();
    await page.getByLabel("Price limit").selectOption({ label: "≤ 50.0 ct/kWh" });
    await expect(page.getByTestId("active-hours")).toHaveText(["Active time", "96 hr"].join(""));
    await expect(page.locator("body")).toContainText("5.0 ct – 50.0 ct");

    await page.getByRole("link", { name: "Charge" }).click();
    await page.getByTestId("energyflow").click();
    await page.getByRole("button", { name: "Grid charging: active (≤ 50.0 ct)" }).click();
    await expect(page).toHaveURL(/#\/battery/);

    await page.getByLabel("Price limit").selectOption({ label: "≤ -10.0 ct/kWh" });
    await expect(page.getByTestId("active-hours")).toHaveText("Active time");

    await page.getByRole("link", { name: "Charge" }).click();
    await expect(
      page.getByRole("button", { name: "Grid charging: when ≤ -10.0 ct" })
    ).toBeVisible();
  });

  test("hold mode display", async ({ page }) => {
    await page.goto("/");
    await page.getByTestId("energyflow").click();

    const discharge = page.getByTestId("energyflow-entry-batterydischarge");
    const charge = page.getByTestId("energyflow-entry-batterycharge");

    await expect(discharge).toContainText("Battery discharging");
    await expect(charge).toContainText("Battery charging");

    // use battery support down to the shared reserve while solar support stays disabled
    await page.goto("/#/battery");
    const reserve = page.getByTestId("battery-reserve").getByRole("combobox").first();
    await expect(page.getByLabel("Battery-supported solar charging")).toHaveValue("false");
    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/40"),
      reserve.selectOption("40"),
    ]);
    await Promise.all([
      page.waitForResponse("**/api/batterydischargemode/reserve"),
      page
        .getByLabel("Home battery support during fast and planned charging")
        .selectOption("reserve"),
    ]);
    await page.getByRole("link", { name: "Charge" }).click();

    await page.getByTestId("energyflow").click();
    await expect(discharge).toContainText("Battery discharging");

    // reaching the buffer latches discharge control
    await page.goto("/#/battery");
    await Promise.all([
      page.waitForResponse("**/api/batteryreservesoc/60"),
      reserve.selectOption("60"),
    ]);
    await page.getByRole("link", { name: "Charge" }).click();
    await page.getByTestId("energyflow").click();
    await expect(discharge).toContainText("Battery (discharge locked)");
    await expect(charge).toContainText("Battery charging");

    // unrestricted support releases the battery again
    await page.goto("/#/battery");
    await Promise.all([
      page.waitForResponse("**/api/batterydischargemode/allow"),
      page
        .getByLabel("Home battery support during fast and planned charging")
        .selectOption("allow"),
    ]);
    await page.getByRole("link", { name: "Charge" }).click();
    await page.getByTestId("energyflow").click();
    await expect(discharge).toContainText("Battery discharging");
  });
});
