package main

// ExitProbeFailure is used when the startup compatibility probe fails (missing
// Python scripts, argparse mismatch, probe timeout). systemd units should set
// RestartPreventExitStatus=78 so the service stays down instead of restarting
// every RestartSec and spamming Discord. (EX_CONFIG from sysexits.h.)
const ExitProbeFailure = 78
