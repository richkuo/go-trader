package main

import "fmt"

func buildNotifierFromConfig(cfg *Config) (*MultiNotifier, func()) {
	if processEgress.directNotifications() {
		return buildBrokerNotifierFromConfig(cfg)
	}
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
				partitionChannels:  partitionChannelValues(cfg.Discord.Channels, cfg.Strategies),
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
				partitionChannels:  partitionChannelValues(cfg.Telegram.Channels, cfg.Strategies),
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

func buildBrokerNotifierFromConfig(cfg *Config) (*MultiNotifier, func()) {
	var backends []notifierBackend
	var closers []func()
	if !cfg.Discord.Enabled && !cfg.Telegram.Enabled {
		return NewMultiNotifier(), func() {}
	}
	sess := processNotifyBroker()
	if sess == nil {
		var err error
		sess, err = connectNotifyBroker()
		if err != nil {
			fmt.Printf("[WARN] notification broker unavailable: %v — continuing without notifications\n", err)
			return NewMultiNotifier(), func() {}
		}
		setProcessNotifyBroker(sess)
		closers = append(closers, func() {
			setProcessNotifyBroker(nil)
			sess.client.Close()
		})
	}
	hello := sess.client.Hello()
	if !sess.client.Connected() {
		hello.Discord, hello.Telegram = true, true
	}
	if cfg.Discord.Enabled {
		if !hello.Discord {
			fmt.Println("[WARN] Discord is enabled but the notification broker has no Discord token — continuing without Discord")
		} else {
			discord := newBrokerDiscordNotifier(sess, cfg.Discord.OwnerID)
			fmt.Printf("Discord connected through the notification broker (%d channels)\n", len(cfg.Discord.Channels))
			backends = append(backends, notifierBackend{
				notifier:           discord,
				channels:           cfg.Discord.Channels,
				tradeAlertChannels: cfg.Discord.TradeAlertChannels,
				ownerID:            cfg.Discord.OwnerID,
				leaderboardChannel: cfg.Discord.LeaderboardChannel,
				dmChannels:         cfg.Discord.DMChannels,
				partitionChannels:  partitionChannelValues(cfg.Discord.Channels, cfg.Strategies),
			})
		}
	}
	if cfg.Telegram.Enabled {
		if !hello.Telegram {
			fmt.Println("[WARN] Telegram is enabled but the notification broker has no Telegram token — continuing without Telegram")
		} else if tg, err := newBrokerTelegramNotifier(sess, cfg.Telegram.OwnerChatID); err != nil {
			fmt.Printf("[WARN] Telegram init failed: %v — continuing without Telegram\n", err)
		} else {
			fmt.Printf("Telegram connected through the notification broker (%d channels)\n", len(cfg.Telegram.Channels))
			backends = append(backends, notifierBackend{
				notifier:           tg,
				channels:           cfg.Telegram.Channels,
				tradeAlertChannels: cfg.Telegram.TradeAlertChannels,
				ownerID:            cfg.Telegram.OwnerChatID,
				dmChannels:         cfg.Telegram.DMChannels,
				plainText:          true,
				partitionChannels:  partitionChannelValues(cfg.Telegram.Channels, cfg.Strategies),
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
