---
paths:
  - "scripts/merge-paper-instance.sh"
  - "scripts/paper_alias.py"
  - "scripts/test_merge_paper_instance.sh"
  - "scripts/test_paper_alias.py"
  - "scripts/check-live-paper-config-drift.sh"
  - "scheduler/inspect*.go"
  - "scheduler/storage_inspect*.go"
  - "scheduler/merge_paper_instance_test.go"
  - "scheduler/config.go"
  - "scheduler/notifier.go"
  - "scheduler/discord.go"
---

## Merge paper instance

- `merge-paper-instance.sh`:binaries on config COPIES;`inspect`/`storage-inspect`=`LoadConfigReadOnly`;both locks preflight>apply except `--diff`(config-only);`--align-to-live` writes only `.aligned`;compose lists ALL root-key conflicts,refuses;alias always via `paper_alias.py`;`--once`/`--probe-only` never proof;zero paper override inherits.
- `channels`/`trade_alert_channels` `-paper` keys skip if merged bare key routes there(=`resolveChannel`);`dm_channels` `-paper`/`-paper:<id>` NEVER skip;proof refuses paper DM route send path disagrees with
