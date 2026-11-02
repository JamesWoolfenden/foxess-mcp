---
name: solar-savings
description: Estimates what a home solar and battery system saves on an Octopus Flux tariff by combining the octopus MCP (grid import, export, rates, costs) with the foxess MCP (solar generation, battery, household load). Gives a savings range, the export-versus-avoided-import split, payback on a given outlay, a year-on-year comparison, and an optional read-only review of the inverter's battery schedule.
---

# solar-savings

The grid meters only see what crosses the grid boundary, so on their own they can't say what solar saves. The inverter sees the rest. This skill joins the two: **octopus** gives what you paid and earned, **foxess** gives what you generated and used.

Everything here is read-only. Never change inverter settings, schedules, or tariffs, and never print an API key.

---

# Arguments

Ask for anything missing, but default sensibly first.

- **period**: default the last 12 complete months. Octopus data lags 1-2 days, so end at the last full month, not today.
- **outlay**: total cost of the system in pounds. Needed only for payback. Ask once; don't guess.
- **compare**: optionally a second period (usually the same months a year earlier) for a year-on-year view.

---

# Steps

## 1. Check both servers are connected

Call `mcp__octopus__get_account` and `mcp__foxess__get_plants`. If a `foxess` tool is missing, the server was added after the session started: tell the user to restart Claude Code (`claude --continue`), then stop. Don't try to work around it.

## 2. What was paid and earned (Octopus)

`get_costs` with `from`/`to` for the period, meter `all`. It prices every half-hour at the rate in force and returns import energy, standing charge, export earnings and a net figure, all inc VAT.

Check `readings_found` against `readings_expected` for each side. **If either is below about 95%, the Octopus cost is understated.** For those periods, price the FoxESS kWh at Octopus's average pence per kWh for the same period and label the result an estimate, not billed. Older periods are often incomplete.

## 3. What was generated and used (FoxESS)

`get_energy_report` with `group_by: month` for the same period. Fields: `pv_kwh`, `export_kwh`, `grid_import_kwh`, `battery_charge_kwh`, `battery_discharge_kwh`, `load_kwh`, `self_sufficiency_pct`.

**Cross-check against Octopus.** FoxESS `export_kwh` and `grid_import_kwh` should match Octopus's kWh to within a few percent. If they don't, stop and say so before going further, because something is wrong with one source.

## 4. Pick the load figure carefully

Two figures for household load disagree by about 7%:
- The API's `load_kwh` is the lower one.
- The FoxESS dashboard Reports page shows a higher figure (grid import plus energy from solar and battery). The API cannot read it, so use the energy balance instead, which lands close to it: `import + pv + discharge - export - charge`.

Run the estimate with **both** and report the range. Don't pick one silently.

## 5. The no-solar baseline and the saving

- **Actual net bill** = import (energy + standing charge) minus export earnings. This is what Octopus's `net_cost_pounds` already is.
- **No-solar bill** = load × blended import price + the same standing charge.
- **Blended price**: don't assume one number. Take the average pence per kWh for each Flux band from the `get_costs` bands (off-peak, day, peak), and weight them by an assumed load profile. A sensible default is 8% off-peak, 75% day, 17% peak. Show **low, central and high** by moving the blended rate about 2p either side.
- **Saving** = no-solar bill minus actual net bill.

**The saving already includes the export earnings.** Never add them on top. Report the split instead: export earnings (exact, priced half-hour by half-hour) plus avoided imports (the remainder, which rests on the price assumption).

## 6. Payback

Outlay divided by the saving, as a range. Say it's a measured year, not a forecast, and name what moves it: tariff changes, export rate changes, panel and battery degradation, and the price assumption in step 5. Offer a year-on-year comparison if only one period was run.

## 7. Year-on-year (if comparing)

Show a two-column table: solar, export, import, load, self-sufficiency, net bill, estimated saving. Look for a change in behaviour (for example export and import both rising with self-sufficiency falling) rather than just reporting totals. An odd month should be checked with the user before it is called an error: a heat wave once looked like a data glitch and wasn't.

## 8. Optional: review the battery schedule (read-only)

Only if asked, or if step 7 shows a behaviour change.

1. Get today's Flux rates with `get_rates` for import and export.
2. Open the FoxESS Mode Scheduler in the user's browser (claude-in-chrome). **Never type a password.** If a login page appears, ask the user to log in and wait. Scroll the Work Mode drawer to read it. **Never click Edit Schedule or change anything.**
3. Rank what a stored kWh is worth, using the live rates: covering the house at peak, exporting at peak, covering the house in the day, exporting at the day rate (worst), against the cost of charging at the off-peak rate plus roughly 10% loss.
4. Check the schedule against it. The usual finding: forced charge to 100% every night all year leaves no room for solar in summer, so surplus solar exports at the day rate. A lower charge cut-off from about April is the fix, but size the gain from the battery's real daily throughput, not an assumption. It is usually small.
5. Recommend testing a change for one month and comparing against the same month last year with these tools, rather than guessing.

---

# Output

Lead with the answer: the saving range and the payback, in a short table. Then the split between export earnings and avoided imports. Then caveats. Label every number as **measured** (from a meter or report) or **estimated** (from an assumption), and say which Octopus periods were incomplete.

---

# Known gotchas

- `get_costs` and `get_consumption` need both `from` and `to`. Dates are UK local calendar days, inclusive.
- The FoxESS `get_realtime` tool returns no data on some inverters even when online. That is a known limit, so say so once and don't loop on it.
- `pv_self_consumed_kwh` is solar minus export, treating all export as solar. It can understate self-consumption if the battery exports energy it bought from the grid.
- On Flux export, the **day rate applies from 19:00 to 02:00 as well** as in the daytime. Only 16:00-19:00 pays the high rate.
- A gas meter that isn't reporting gives `no_data`, not zero. Leave gas out of a solar analysis unless asked.
- Serials and account numbers are masked by the tools. Keep them masked in anything you write.

The two servers are [octo-mcp](https://github.com/JamesWoolfenden/octo-mcp) and [foxess-mcp](https://github.com/JamesWoolfenden/foxess-mcp).
