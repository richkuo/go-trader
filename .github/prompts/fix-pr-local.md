Repository-specific additions for go-trader, appended to the shared fix instructions above.

The no-execution ban covers backtests as well: do NOT run backtests, optimizers, tuning runs, or parity harnesses. CI runs the Go build, go test, and pytest on the pull request separately.

Run gofmt on every Go file you edit.

Add or update the tests a change needs, as code, without running them.
