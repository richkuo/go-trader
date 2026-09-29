Repository-specific additions for go-trader, appended to the shared review format above. Where a line here names something the shared text states generally, this line adds detail and never relaxes the shared rule.

The no-execution ban covers backtests as well: do NOT run backtests, optimizers, tuning runs, or parity harnesses in this workflow. CI runs the Go build, go test, and pytest on the pull request separately.

The safety carve-out applies to this repository's auto-protective mechanisms by name: the portfolio kill switch, the per-strategy circuit breaker, stop-loss arming and trailing-stop replacement, liquidation clamping, shared-wallet and cash reconciliation, and position, fill, or realized-PnL accounting. A finding that touches any of them is always surfaced, at any confidence and any magnitude.

Apply the test-scope rule and the test budget in CLAUDE.md to every new test. A new test whose only assertion is log or DM wording, a constant, or a plain-struct round-trip is Needs Fixing, unless the wording drives an operator decision, which the CLAUDE.md test-scope rule permits. Violations that touch money, data integrity, security, or an auto-protective mechanism remain subject to the shared safety carve-out. All other violations belong under ### Recommended Optional.
