package main

// ExitProbeFailure is used when the startup compatibility probe fails (missing
// Python scripts, argparse mismatch, probe timeout). systemd units should set
// RestartPreventExitStatus=78 so the service stays down instead of restarting
// every RestartSec and spamming Discord. (EX_CONFIG from sysexits.h.)
const ExitProbeFailure = 78

// ExitSingletonLock is used when the daemon refuses to start because another
// live go-trader already holds the exclusive lock on the resolved state-DB
// path (#849). A duplicate against the same state.db/exchange account would
// double-trade and desync state, so the second process exits here instead.
//
// Both service files set RestartPreventExitStatus=79 alongside 78: under a
// normal `systemctl restart` the managed daemon is the lock holder, so it can
// only hit this code when an out-of-cgroup duplicate is alive — in that case
// systemd should stay down (one DM, no restart-loop) rather than respawn every
// RestartSec into the same refusal. The kernel auto-releases the flock when the
// holder dies, so this never permanently blocks a legitimate restart. 79 sits
// just past the sysexits.h range (64–78) to avoid colliding with those codes.
const ExitSingletonLock = 79
