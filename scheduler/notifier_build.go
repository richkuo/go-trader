package main

import "fmt"

// buildNotifierFromConfig constructs a MultiNotifier from the Discord and Telegram
// sections of cfg. It prints the same connection messages as the daemon startup path
// so callers (main and CLI subcommands alike) see consistent output. The returned
// cleanup function must be deferred by the caller to close gateway connections.
func buildNotifierFromConfig(cfg *Config) (*MultiNotifier, func()) {
	var backends []notifierBackend
	var closers []func()

	if cfg.Discord.Enabled && cfg.Discord.Token != "" {
		discord, err := NewDiscordNotifier(cfg.Discord.Token, cfg.Discord.OwnerID)
		if err != nil {
			fmt.Printf("[WARN] Discord init failed: %v — continuing without Discord\n", err)
		} else {
			fmt.Printf("Discord gateway connected (%d channels", len(cfg.Discord.Channels))
			if cfg.Discord.OwnerID != "" {
				fmt.Printf(", DM owner enabled")
			}
			fmt.Println(")")
			backends = append(backends, notifierBackend{
				notifier:           discord,
				channels:           cfg.Discord.Channels,
				tradeAlertChannels: cfg.Discord.TradeAlertChannels,
				ownerID:            cfg.Discord.OwnerID,
				leaderboardChannel: cfg.Discord.LeaderboardChannel,
				dmChannels:         cfg.Discord.DMChannels,
			})
			closers = append(closers, discord.Close)
		}
	}

	if cfg.Telegram.Enabled && cfg.Telegram.BotToken != "" {
		tg, err := NewTelegramNotifier(cfg.Telegram.BotToken, cfg.Telegram.OwnerChatID)
		if err != nil {
			fmt.Printf("[WARN] Telegram init failed: %v — continuing without Telegram\n", err)
		} else {
			fmt.Printf("Telegram bot connected (%d channels", len(cfg.Telegram.Channels))
			if cfg.Telegram.OwnerChatID != "" {
				fmt.Printf(", DM owner enabled")
			}
			fmt.Println(")")
			backends = append(backends, notifierBackend{
				notifier:           tg,
				channels:           cfg.Telegram.Channels,
				tradeAlertChannels: cfg.Telegram.TradeAlertChannels,
				ownerID:            cfg.Telegram.OwnerChatID,
				dmChannels:         cfg.Telegram.DMChannels,
				plainText:          true,
			})
			closers = append(closers, tg.Close)
		}
	}

	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}
	return NewMultiNotifier(backends...), cleanup
}
