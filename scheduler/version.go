package main

// Version is set at build time via -ldflags "-X main.Version=vX.Y.Z".
// Falls back to "dev" for local builds.
var Version = "dev"
