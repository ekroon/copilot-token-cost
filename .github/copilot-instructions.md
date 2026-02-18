# Copilot Instructions for copilot-token-cost

## Project Overview

This is the **Copilot CLI Token Usage & Cost Calculator** — a Go-based tool that parses Copilot CLI process logs to extract per-model token usage and calculates estimated API-equivalent costs.

## Repository Structure

- `go/main.go` — Go implementation
- `pricing.json` — Shared pricing data (historical, time-ranged)
- `.github/workflows/` — CI, release, and pricing update workflows

## Pricing Data Format (`pricing.json`)

The file uses a **time-ranged historical pricing** format:

```json
{
  "pricing_periods": [
    {
      "_label": "Human-readable description of what changed",
      "effective_from": "YYYY-MM-DD",
      "premium_request_cost": 0.04,
      "model_pricing": {
        "model-name": { "input": X, "output": X, "cache_read": X, "cache_write": X }
      },
      "premium_multiplier": {
        "model-name": N
      }
    }
  ]
}
```

### Key rules:
- `pricing_periods` is sorted **newest-first** (most recent period at index 0)
- Each period has an `effective_from` date — the first day this pricing applies
- Each period contains **FULL pricing for ALL models** known at that time (not just deltas)
- Token prices are **per 1M tokens in USD**
- `premium_request_cost` is the cost per premium request ($0.04)
- `premium_multiplier` is how many premium requests each user turn costs for that model

### When updating pricing:
1. **Add a new period** at the top of the array with today's date as `effective_from`
2. Copy the full model_pricing and premium_multiplier from the previous period
3. Apply the changes (new prices, new multipliers, new models)
4. Add a descriptive `_label` explaining what changed
5. **Never modify existing periods** — they represent historical truth
6. The Go implementation reads this same file

## Where to Find Pricing Data

### GitHub Copilot Premium Request Multipliers
- https://docs.github.com/en/copilot/reference/ai-models/supported-models
- Look for the "Premium requests" or "Multiplier" column in model tables

### API Token Pricing (per-model)
- **Anthropic (Claude):** https://www.anthropic.com/pricing
- **OpenAI (GPT):** https://openai.com/api/pricing/
- **Google (Gemini):** https://ai.google.dev/pricing

### What to Check
- New models added to Copilot
- Changes to premium request multipliers
- Changes to API token pricing (input, output, cache read, cache write)
- Changes to premium_request_cost (currently $0.04)

## Code Style
- Minimal comments — only where clarification is needed
- Keep Go output and behavior stable when making feature changes
