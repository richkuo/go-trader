---
paths:
  - "scheduler/robinhood*.go"
  - "platforms/robinhood/**"
  - "shared_scripts/*robinhood*"
  - "shared_scripts/testdata/robinhood_offline/**"
  - "scheduler/agent_info.go"
  - "scripts/*robinhood*"
  - "scripts/update_helpers.sh"
  - "scripts/migrate-service-layout.py"
  - "scripts/test_migrate_service_layout_fixture.sh"
  - "systemd/**"
---

## Robinhood

- RH session:owner-only dir beside config(`GO_TRADER_ROBINHOOD_SESSION_DIR`),never `$HOME`;scheduler login never answers device verify(`robinhood-<key>.verification` latch)
