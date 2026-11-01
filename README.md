# foxess-mcp

A small, read-only [MCP](https://modelcontextprotocol.io) server (Go, stdio) for [FoxESS Cloud](https://www.foxesscloud.com). It lets Claude answer questions about your solar and battery system: how much you generated, exported, imported and used.

It is the other half of [octo-mcp](https://github.com/JamesWoolfenden/octo-mcp). Your grid meters only see what crosses the grid boundary. They can't see solar you used directly or via the battery, so on their own they can't tell you what solar saves. The inverter can. With both servers connected, Claude can combine the two.

## Tools

| Tool | What it does |
|---|---|
| `get_plants` | Inverters on the account: status, model, solar and battery present, battery count, inverter rating. Serials are masked and the station name is left out (it is often an address). |
| `get_energy_report` | Solar generation, grid import and export, battery charge and discharge and household load in kWh, per day or month, with totals. |
| `get_realtime` | Live solar, load, grid, battery power and state of charge. |

Everything is read-only. The client has an allowlist of query endpoints and refuses to call anything else, so it cannot change inverter settings or schedules.

## Setup

### 1. Get an API key

In FoxESS Cloud open your profile, then **API Management**, and generate a key. The API allows 1,440 calls per inverter per day and one query per second per endpoint. The server paces itself and caches reports, so normal use stays well inside that.

### 2. Build

```sh
go build -o foxess-mcp.exe .
```

### 3. Configure

Config comes from environment variables only:

| Variable | Required | Meaning |
|---|---|---|
| `FOXESS_API_KEY` | yes | Your API key. Never logged or echoed. |
| `FOXESS_DEVICE_SN` | no | Pin one inverter if the account has several. |
| `FOXESS_BASE_URL` | no | Override the API host (default `https://www.foxesscloud.com`). |

#### Claude Code

```sh
claude mcp add foxess --env FOXESS_API_KEY=... -- /path/to/foxess-mcp
```

#### Claude Desktop

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "foxess": {
      "command": "C:\\path\\to\\foxess-mcp.exe",
      "env": {
        "FOXESS_API_KEY": "..."
      }
    }
  }
}
```

## Example prompts

- "How much solar did we generate each month over the last year?"
- "What share of our household use was covered by solar and the battery in October?"
- "What is the battery at right now, and are we importing or exporting?"
- With octo-mcp too: "Estimate what solar and the battery saved us over the last year."

## Behaviour worth knowing

- **Missing is not zero.** A variable FoxESS doesn't return is `null`. A period where everything is zero is reported as `no_data` with an explanation (the inverter may have been offline), not as zero use.
- **Partial periods.** A range that includes today or the current month has status `partial`.
- **Derived fields.** `pv_self_consumed_kwh` is solar minus export, treating all export as solar, and is left out if that would be negative. `self_sufficiency_pct` is the share of load not bought from the grid.
- **Dates.** Days and months are calendar periods in UK local time. `to` is inclusive and capped at today. Month grouping returns whole calendar months.
- **Caching.** Finished days and months are cached for a day, the current one for five minutes, to protect the daily call allowance.

## Development

```sh
go test ./...
```

Tests never touch the network. The API is faked with an in-process `httptest` server that checks the token and signature on every request, and all identifiers in the fixtures are fake.

The request signature is `md5(path + "\r\n" + token + "\r\n" + timestamp)`, where `\r\n` is the four literal characters backslash, r, backslash, n, as in FoxESS's own sample code (real CR LF bytes are rejected). If you see `illegal signature` errors, that is the first place to look.

### Keeping secrets out of the repo

Your API key must never be committed. After cloning, install the [pre-commit](https://pre-commit.com) hooks, which run gitleaks, a private-key check, `go vet` and `go test` before every commit:

```sh
pre-commit install
```

The same gitleaks scan runs in CI on every push and pull request (`.github/workflows/gitleaks.yml`). `.env` files, keys and local MCP config are in `.gitignore`.
