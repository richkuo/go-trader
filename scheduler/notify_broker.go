package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"trading-scheduler/internal/notifybroker"
)

const brokerCallTimeout = 30 * time.Second

type brokerDiscordTransport struct {
	c *notifybroker.Client

	mu            sync.Mutex
	onInteraction func(*discordgo.InteractionCreate)
	onMessage     func(*discordgo.MessageCreate)
}

func brokerContext(_ []discordgo.RequestOption) (context.Context, context.CancelFunc) {
	return context.WithTimeout(shutdownReadOnlyCtx, brokerCallTimeout)
}

func (b *brokerDiscordTransport) selfID() string {
	if id := b.c.Hello().DiscordUserID; id != "" {
		return id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hello, _ := b.c.RefreshHello(ctx)
	return hello.DiscordUserID
}

func (b *brokerDiscordTransport) close() {
	b.c.Close()
}

func (b *brokerDiscordTransport) setInteractionHandler(fn func(*discordgo.InteractionCreate)) {
	b.mu.Lock()
	b.onInteraction = fn
	b.mu.Unlock()
}

func (b *brokerDiscordTransport) setMessageHandler(fn func(*discordgo.MessageCreate)) {
	b.mu.Lock()
	b.onMessage = fn
	b.mu.Unlock()
}

func (b *brokerDiscordTransport) dispatch(kind string, raw json.RawMessage) {
	b.mu.Lock()
	onInteraction, onMessage := b.onInteraction, b.onMessage
	b.mu.Unlock()
	switch kind {
	case notifybroker.EventDiscordInteraction:
		if onInteraction == nil {
			return
		}
		var ic discordgo.InteractionCreate
		if err := json.Unmarshal(raw, &ic); err != nil || ic.Interaction == nil {
			return
		}
		onInteraction(&ic)
	case notifybroker.EventDiscordMessage:
		if onMessage == nil {
			return
		}
		var mc discordgo.MessageCreate
		if err := json.Unmarshal(raw, &mc); err != nil || mc.Message == nil {
			return
		}
		onMessage(&mc)
	}
}

func interactionRef(i *discordgo.Interaction) notifybroker.InteractionRef {
	if i == nil {
		return notifybroker.InteractionRef{}
	}
	return notifybroker.InteractionRef{ID: i.ID, AppID: i.AppID, Token: i.Token}
}

func (b *brokerDiscordTransport) ChannelMessageSend(channelID string, content string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	ctx, cancel := brokerContext(options)
	defer cancel()
	var msg discordgo.Message
	if err := b.c.Call(ctx, notifybroker.OpDiscordSend, notifybroker.DiscordSendRequest{ChannelID: channelID, Content: content}, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (b *brokerDiscordTransport) UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	ctx, cancel := brokerContext(options)
	defer cancel()
	var ch discordgo.Channel
	if err := b.c.Call(ctx, notifybroker.OpDiscordUserChannel, notifybroker.DiscordUserChannelRequest{UserID: recipientID}, &ch); err != nil {
		return nil, err
	}
	return &ch, nil
}

func (b *brokerDiscordTransport) InteractionRespond(interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error {
	ctx, cancel := brokerContext(options)
	defer cancel()
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return b.c.Call(ctx, notifybroker.OpDiscordRespond, notifybroker.DiscordRespondRequest{Interaction: interactionRef(interaction), Response: raw}, nil)
}

func (b *brokerDiscordTransport) FollowupMessageCreate(interaction *discordgo.Interaction, wait bool, data *discordgo.WebhookParams, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	ctx, cancel := brokerContext(options)
	defer cancel()
	req := notifybroker.DiscordFollowupRequest{Interaction: interactionRef(interaction), Wait: wait}
	if data != nil {
		req.Content = data.Content
		req.Flags = int(data.Flags)
		for _, f := range data.Files {
			if f == nil || f.Reader == nil {
				continue
			}
			body, err := io.ReadAll(io.LimitReader(f.Reader, 8<<20+1))
			if err != nil {
				return nil, fmt.Errorf("read attachment %s: %w", f.Name, err)
			}
			req.Files = append(req.Files, notifybroker.FileAttachment{Name: f.Name, ContentType: f.ContentType, Data: body})
		}
	}
	var msg discordgo.Message
	if err := b.c.Call(ctx, notifybroker.OpDiscordFollowup, req, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (b *brokerDiscordTransport) ApplicationCommandBulkOverwrite(appID string, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	if guildID != "" {
		return nil, fmt.Errorf("the notification broker registers global commands only")
	}
	ctx, cancel := brokerContext(options)
	defer cancel()
	raw, err := json.Marshal(commands)
	if err != nil {
		return nil, err
	}
	var out []*discordgo.ApplicationCommand
	if err := b.c.Call(ctx, notifybroker.OpDiscordCommands, notifybroker.DiscordCommandsRequest{Commands: raw}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type notifyBrokerSession struct {
	client  *notifybroker.Client
	discord *brokerDiscordTransport
}

func connectNotifyBroker() (*notifyBrokerSession, error) {
	processEgress.mu.Lock()
	a := processEgress.attestation
	processEgress.mu.Unlock()
	if a == nil || a.Notify == nil {
		return nil, fmt.Errorf("direct notifications need a notification broker attestation")
	}
	dt := &brokerDiscordTransport{}
	client := notifybroker.NewClient(a.Notify.Socket, a.Notify.BrokerUID,
		[]string{notifybroker.EventDiscordInteraction, notifybroker.EventDiscordMessage}, dt.dispatch)
	dt.c = client
	sess := &notifyBrokerSession{client: client, discord: dt}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := client.Start(ctx)
	cancel()
	deadline := time.Now().Add(brokerStartupWait)
	for err != nil && !client.Connected() && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if !client.Connected() {
		fmt.Printf("[WARN] notification broker not reachable yet (%v); notifications start when it answers\n", err)
	}
	return sess, nil
}

const brokerStartupWait = 30 * time.Second

func newBrokerDiscordNotifier(sess *notifyBrokerSession, ownerID string) *DiscordNotifier {
	d := &DiscordNotifier{transport: sess.discord, broker: sess.discord, ownerID: ownerID}
	sess.discord.setMessageHandler(d.handleDirectMessage)
	return d
}

func brokerCreateGitHubIssue(ctx context.Context, repo string, payload githubIssueRequest) (string, int, error) {
	sess := processNotifyBroker()
	if sess == nil {
		return "", 0, fmt.Errorf("the notification broker is not connected")
	}
	var out notifybroker.GitHubIssueResponse
	err := sess.client.Call(ctx, notifybroker.OpGitHubIssue, notifybroker.GitHubIssueRequest{Repo: repo, Title: payload.Title, Body: payload.Body, Labels: payload.Labels}, &out)
	return out.HTMLURL, out.Number, err
}

var (
	notifyBrokerMu      sync.Mutex
	notifyBrokerCurrent *notifyBrokerSession
)

func processNotifyBroker() *notifyBrokerSession {
	notifyBrokerMu.Lock()
	defer notifyBrokerMu.Unlock()
	return notifyBrokerCurrent
}

func setProcessNotifyBroker(s *notifyBrokerSession) {
	notifyBrokerMu.Lock()
	notifyBrokerCurrent = s
	notifyBrokerMu.Unlock()
}

func newBrokerTelegramNotifier(sess *notifyBrokerSession, ownerChatID string) (*TelegramNotifier, error) {
	t := &TelegramNotifier{ownerChatID: ownerChatID, broker: sess.client}
	if !sess.client.Connected() {
		return t, nil
	}
	resp, err := t.apiCall("getMe", nil)
	if err != nil {
		return nil, fmt.Errorf("telegram getMe through the notification broker failed: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("telegram getMe: %s", resp.Description)
	}
	return t, nil
}
