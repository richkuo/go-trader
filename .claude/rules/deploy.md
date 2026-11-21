---
paths:
  - "scripts/update.sh"
  - "scripts/update_*"
  - "scripts/install-service.sh"
  - "scripts/egress-install.sh"
  - "scripts/migrate-service-layout.py"
  - "scripts/shared-feed-convert.sh"
  - "scripts/test_migrate_service_layout_fixture.sh"
  - "scripts/test_update_helpers.sh"
  - "systemd/**"
---

## Deploy scripts

- `migrate-service-layout.py`/`shared-feed-convert.sh` Layout:enable-intent target authoritative,recovery NEVER restarts source from stale snapshot;feed:failed switch restores byte copy.
- Scripts:root git via `update_git`(py:exact-tree `-c`),never `*`/config;foreign-owned tree:confined units,Go only from `git archive`,probe as owner;give-back fd-pinned,no hardlinks;root `uv sync` copy-mode;uv/go via `update_resolve_tool`.
