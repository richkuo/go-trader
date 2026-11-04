package main

import "testing"

// Discord rejects an application command whose name is longer than 32
// characters, and one bad name fails the whole bulk overwrite. Keep every
// registered name inside the limit so one long name cannot unregister the
// entire command set.
func TestSlashCommandNamesFitDiscordLimit(t *testing.T) {
	seen := map[string]bool{}
	for _, cmd := range slashCommands() {
		if len(cmd.Name) > 32 {
			t.Errorf("slash command %q is %d characters, above the Discord limit of 32", cmd.Name, len(cmd.Name))
		}
		if seen[cmd.Name] {
			t.Errorf("duplicate slash command name %q", cmd.Name)
		}
		seen[cmd.Name] = true
	}
}
