package main

const ExitProbeFailure = 78

const ExitSingletonLock = 79

// ExitStorageOwnership is a rejected storage layout: aliased files, an invalid
// identity map, or a book in the wrong file. systemd does not restart on it.
const ExitStorageOwnership = 80
