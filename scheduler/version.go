package main

// Version is set at build time via -ldflags "-X main.Version=vX.Y.Z".
// Falls back to "dev" for local builds. scripts/update.sh re-stamps this on
// every rebuild via `git describe --tags --always --dirty=-mod`.
var Version = "dev"
