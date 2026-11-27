---
paths:
  - "scheduler/discord*.go"
  - "scheduler/config*.go"
---

## Discord commands

- `discord_*commands.go`:new mutating cmd:`opsCommandNames`+`slashCommands()`+dispatch;sync only `go-trader-`,no bulk PUT;`slash_commands` DEFAULT-OFF;cmd calls ONLY daemon worker(no `mu`/retry).
