import { test, expect } from "@playwright/test";
import { start, stop, baseUrl } from "./evcc";

const CONFIG = "battery-experimental.evcc.yaml";
const SQL = "battery-experimental.sql";

test.use({ baseURL: baseUrl() });
test.describe.configure({ mode: "parallel" });

test.beforeEach(async () => {
  await start(CONFIG, SQL);
});
test.afterEach(async () => {
  await stop();
});

test.describe("experimental battery page", async () => {
  test("status cards: combined aggregate plus one per battery", async ({ page }) => {
    await page.goto("/#/battery");
    await expect(page.getByTestId("battery-experimental")).toBeVisible();

    const cards = page.getByTestId("battery-status-card");
    await expect(cards).toHaveCount(3); // combined + battery1 + battery2

    // combined card first, showing the capacity-weighted site soc (76%@13.5 + 40%@7.5 -> 63%)
    await expect(cards.first()).toContainText("Combined");
    await expect(cards.first()).toContainText("63%");

    // per-battery: soc, charge/discharge state and energy of total
    const charging = cards.filter({ hasText: "76%" });
    await expect(charging).toContainText("charging"); // battery1 power -800 W
    await expect(charging).toContainText("13.5 kWh"); // of total capacity

    const discharging = cards.filter({ hasText: "40%" });
    await expect(discharging).toContainText("discharging"); // battery2 power 1200 W
  });

  test("history chart: unit toggle persists and window pages", async ({ page }) => {
    await page.goto("/#/battery");

    const energy = page.getByTestId("batteryUnit-energy");
    await expect(energy).toBeEnabled(); // both batteries have capacity
    await energy.click();
    await expect(energy).toHaveAttribute("aria-checked", "true");

    // unit choice persists across a reload
    await page.reload();
    await expect(page.getByTestId("batteryUnit-energy")).toHaveAttribute("aria-checked", "true");

    // paging: cannot go into the future at offset 0, prev enables next
    const prev = page.getByTestId("battery-chart-prev");
    const next = page.getByTestId("battery-chart-next");
    await expect(next).toBeDisabled();
    await expect(prev).toBeEnabled();
    await prev.click();
    await expect(next).toBeEnabled();
  });

  test("usage configuration reflects and updates the stored thresholds", async ({ page }) => {
    await page.goto("/#/battery");

    // section headings render
    await expect(page.getByText("Where does the surplus go first?")).toBeVisible();
    await expect(page.getByText("Home battery reserve")).toBeVisible();

    // the legacy buffer setting migrates to the shared reserve with solar support enabled
    const prioritySoc = page.getByTestId("battery-priority").getByRole("combobox");
    const reserveSoc = page.getByTestId("battery-reserve").getByRole("combobox").first();
    const solarSupport = page.getByLabel("Battery-supported solar charging");
    await expect(prioritySoc).toHaveValue("50");
    await expect(reserveSoc).toHaveValue("80");
    await expect(solarSupport).toHaveValue("true");

    // changing priority updates the picker value
    await Promise.all([
      page.waitForResponse("**/api/prioritysoc/30"),
      prioritySoc.selectOption("30"),
    ]);
    await expect(prioritySoc).toHaveValue("30");

    // values crossing the other threshold are not selectable (prioritySoc 30, bufferSoc 80)
    await expect(prioritySoc.getByRole("option", { name: "85%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );
    await expect(reserveSoc.getByRole("option", { name: "25%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );
    await expect(reserveSoc.getByRole("option", { name: "0%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );
    await expect(reserveSoc.getByRole("option", { name: "100%", exact: true })).toHaveAttribute(
      "disabled",
      ""
    );

    // equal values unlock higher priorities; selecting one raises the buffer instead
    await Promise.all([
      page.waitForResponse("**/api/prioritysoc/80"),
      prioritySoc.selectOption("80"),
    ]);
    await Promise.all([
      page.waitForResponse("**/api/bufferstartsoc/90"),
      page.getByLabel("Start automatically").selectOption("90"),
    ]);
    const [startResponse, reserveResponse] = await Promise.all([
      page.waitForResponse("**/api/bufferstartsoc/95"),
      page.waitForResponse("**/api/batteryreservesoc/95"),
      prioritySoc.selectOption("95"),
    ]);
    expect(startResponse.ok()).toBe(true);
    expect(reserveResponse.ok()).toBe(true);
    await expect(reserveSoc).toHaveValue("95");
    await expect(prioritySoc).toHaveValue("80");

    // disabling solar support keeps the shared reserve
    await Promise.all([
      page.waitForResponse("**/api/batterysolarsupport/false"),
      solarSupport.selectOption("false"),
    ]);
    await expect(reserveSoc).toHaveValue("95");

    // battery support policy is offered for the controllable battery
    const discharge = page.getByLabel("Home battery support during fast and planned charging");
    await expect(discharge).toHaveValue("allow");
    await Promise.all([
      page.waitForResponse("**/api/batterydischargemode/reserve"),
      discharge.selectOption("reserve"),
    ]);
    await expect(discharge).toHaveValue("reserve");
  });
});
