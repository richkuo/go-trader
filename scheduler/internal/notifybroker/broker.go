package notifybroker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"trading-scheduler/internal/egresspolicy"
)

const (
	telegramAPIBase = "https://api.telegram.org/bot"
	githubAPIBase   = "https://api.github.com"
	defaultRepo     = "richkuo/go-trader"
	requestTimeout  = 30 * time.Second
)

type Broker struct {
	WorkerUID    int
	SocketPath   string
	Dialer       *egresspolicy.Dialer
	Destinations map[string]bool
	TLSConfig    *tls.Config

	discordToken  string
	telegramToken string
	githubToken   string
	githubRepos   map[string]bool

	session *discordgo.Session
	client  *http.Client

	mu   sync.Mutex
	subs map[*brokerConn][]string
}

type brokerConn struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *brokerConn) send(f *Frame) error {
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(requestTimeout))
	_, err = c.conn.Write(data)
	return err
}

func NewBrokerFromEnv(workerUID int, socketPath string, dialer *egresspolicy.Dialer, destinations []string) *Broker {
	b := &Broker{
		WorkerUID:     workerUID,
		SocketPath:    socketPath,
		Dialer:        dialer,
		Destinations:  map[string]bool{},
		discordToken:  strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")),
		telegramToken: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		githubToken:   strings.TrimSpace(os.Getenv("GO_TRADER_GITHUB_TOKEN")),
		githubRepos:   map[string]bool{},
		subs:          map[*brokerConn][]string{},
	}
	if b.githubToken == "" {
		b.githubToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	for _, d := range destinations {
		b.Destinations[d] = true
	}
	repos := strings.TrimSpace(os.Getenv(EnvGitHubRepos))
	if repos == "" {
		repos = defaultRepo
	}
	for _, r := range strings.Split(repos, ",") {
		if r = strings.TrimSpace(r); ValidRepo(r) == nil {
			b.githubRepos[r] = true
		}
	}
	return b
}

func (b *Broker) transport() *http.Transport {
	t := b.Dialer.Transport()
	if b.TLSConfig != nil {
		t.TLSClientConfig = b.TLSConfig.Clone()
	}
	return t
}

func (b *Broker) redact(s string) string {
	for _, secret := range []string{b.discordToken, b.telegramToken, b.githubToken} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

func (b *Broker) Run(ctx context.Context) error {
	b.client = &http.Client{Transport: b.transport(), Timeout: requestTimeout}
	if b.discordToken != "" && b.Destinations["discord"] {
		go b.keepDiscordOpen(ctx)
	}
	if err := os.Remove(b.SocketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", b.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(b.SocketPath, 0o666); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("notification broker serving %s (discord=%t telegram=%t github=%t)", b.SocketPath, b.discordToken != "" && b.Destinations["discord"], b.telegramEnabled(), b.githubEnabled())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go b.serveConn(ctx, conn)
	}
}

func (b *Broker) telegramEnabled() bool {
	return b.telegramToken != "" && b.Destinations["telegram"]
}

func (b *Broker) githubEnabled() bool {
	return b.githubToken != "" && b.Destinations["github"]
}

func (b *Broker) discordSession() *discordgo.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

func (b *Broker) keepDiscordOpen(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		err := b.openDiscord()
		if err == nil {
			<-ctx.Done()
			if s := b.discordSession(); s != nil {
				s.Close()
			}
			return
		}
		log.Printf("notification broker: discord gateway open failed; retrying in %s: %s", backoff, b.redact(err.Error()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 2*time.Minute {
			backoff *= 2
		}
	}
}

func (b *Broker) openDiscord() error {
	s, err := discordgo.New("Bot " + b.discordToken)
	if err != nil {
		return err
	}
	s.Client = &http.Client{Transport: b.transport(), Timeout: 20 * time.Second}
	s.Dialer = b.Dialer.WebsocketDialer(45 * time.Second)
	if b.TLSConfig != nil {
		s.Dialer.TLSClientConfig = b.TLSConfig.Clone()
	}
	s.Identify.Intents = discordgo.IntentsDirectMessages
	s.AddHandler(func(_ *discordgo.Session, e *discordgo.Event) {
		switch e.Type {
		case "INTERACTION_CREATE":
			b.publish(EventDiscordInteraction, e.RawData)
		case "MESSAGE_CREATE":
			b.publish(EventDiscordMessage, e.RawData)
		}
	})
	if err := s.Open(); err != nil {
		return err
	}
	b.mu.Lock()
	b.session = s
	b.mu.Unlock()
	return nil
}

func (b *Broker) publish(kind string, raw json.RawMessage) {
	b.mu.Lock()
	var targets []*brokerConn
	for c, kinds := range b.subs {
		for _, k := range kinds {
			if k == kind {
				targets = append(targets, c)
				break
			}
		}
	}
	b.mu.Unlock()
	for _, c := range targets {
		_ = c.send(&Frame{Type: FrameEvent, Op: kind, Payload: append(json.RawMessage(nil), raw...)})
	}
}

func (b *Broker) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	cred, err := egresspolicy.PeerCredentials(conn)
	if err != nil || cred.UID != b.WorkerUID {
		log.Printf("notification broker refused a connection (uid %d, err %v)", cred.UID, err)
		return
	}
	bc := &brokerConn{conn: conn}
	defer func() {
		b.mu.Lock()
		delete(b.subs, bc)
		b.mu.Unlock()
	}()
	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		f, err := readFrame(r)
		if err != nil {
			return
		}
		if f.Type != FrameRequest || f.ID == 0 {
			return
		}
		go func(f *Frame) {
			rctx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			payload, err := b.handle(rctx, bc, f)
			resp := &Frame{ID: f.ID, Type: FrameResponse, Op: f.Op, Payload: payload}
			if err != nil {
				resp.Error = b.redact(err.Error())
				resp.Payload = nil
			}
			_ = bc.send(resp)
		}(f)
	}
}

func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (b *Broker) handle(ctx context.Context, bc *brokerConn, f *Frame) (json.RawMessage, error) {
	switch f.Op {
	case OpHello:
		var req HelloRequest
		if len(f.Payload) > 0 {
			if err := decodeStrict(f.Payload, &req); err != nil {
				return nil, err
			}
		}
		for _, k := range req.Events {
			if k != EventDiscordInteraction && k != EventDiscordMessage {
				return nil, fmt.Errorf("unknown event kind %q", k)
			}
		}
		if len(req.Events) > 0 {
			b.mu.Lock()
			b.subs[bc] = append([]string(nil), req.Events...)
			b.mu.Unlock()
		}
		resp := HelloResponse{Discord: b.discordToken != "" && b.Destinations["discord"], Telegram: b.telegramEnabled(), GitHub: b.githubEnabled()}
		if s := b.discordSession(); s != nil && s.State != nil && s.State.User != nil {
			resp.DiscordUserID = s.State.User.ID
		}
		return json.Marshal(resp)
	case OpDiscordSend, OpDiscordUserChannel, OpDiscordRespond, OpDiscordFollowup, OpDiscordCommands:
		s := b.discordSession()
		if s == nil {
			return nil, errors.New("discord is not connected in the notification broker")
		}
		return b.handleDiscord(ctx, s, f)
	case OpTelegramCall:
		if !b.telegramEnabled() {
			return nil, errors.New("telegram is not configured in the notification broker")
		}
		return b.handleTelegram(ctx, f.Payload)
	case OpGitHubIssue:
		if !b.githubEnabled() {
			return nil, errors.New("issue reporting is not configured in the notification broker")
		}
		return b.handleGitHub(ctx, f.Payload)
	default:
		return nil, fmt.Errorf("unknown operation %q", f.Op)
	}
}

func (b *Broker) handleDiscord(ctx context.Context, session *discordgo.Session, f *Frame) (json.RawMessage, error) {
	opt := discordgo.WithContext(ctx)
	switch f.Op {
	case OpDiscordSend:
		var req DiscordSendRequest
		if err := decodeStrict(f.Payload, &req); err != nil {
			return nil, err
		}
		if err := ValidSnowflake(req.ChannelID); err != nil {
			return nil, fmt.Errorf("channel id: %w", err)
		}
		if len(req.Content) > 2000 {
			return nil, errors.New("message is longer than 2000 characters")
		}
		msg, err := session.ChannelMessageSend(req.ChannelID, req.Content, opt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(msg)
	case OpDiscordUserChannel:
		var req DiscordUserChannelRequest
		if err := decodeStrict(f.Payload, &req); err != nil {
			return nil, err
		}
		if err := ValidSnowflake(req.UserID); err != nil {
			return nil, fmt.Errorf("user id: %w", err)
		}
		ch, err := session.UserChannelCreate(req.UserID, opt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(ch)
	case OpDiscordRespond:
		var req DiscordRespondRequest
		if err := decodeStrict(f.Payload, &req); err != nil {
			return nil, err
		}
		if err := req.Interaction.Validate(); err != nil {
			return nil, err
		}
		var resp discordgo.InteractionResponse
		if err := json.Unmarshal(req.Response, &resp); err != nil {
			return nil, fmt.Errorf("interaction response: %v", err)
		}
		in := &discordgo.Interaction{ID: req.Interaction.ID, AppID: req.Interaction.AppID, Token: req.Interaction.Token}
		if err := session.InteractionRespond(in, &resp, opt); err != nil {
			return nil, err
		}
		return json.RawMessage(`{}`), nil
	case OpDiscordFollowup:
		var req DiscordFollowupRequest
		if err := decodeStrict(f.Payload, &req); err != nil {
			return nil, err
		}
		if err := req.Interaction.Validate(); err != nil {
			return nil, err
		}
		if len(req.Content) > 2000 {
			return nil, errors.New("message is longer than 2000 characters")
		}
		params := &discordgo.WebhookParams{Content: req.Content, Flags: discordgo.MessageFlags(req.Flags)}
		for _, file := range req.Files {
			if err := ValidFile(file); err != nil {
				return nil, err
			}
			params.Files = append(params.Files, &discordgo.File{Name: file.Name, ContentType: file.ContentType, Reader: bytes.NewReader(file.Data)})
		}
		in := &discordgo.Interaction{ID: req.Interaction.ID, AppID: req.Interaction.AppID, Token: req.Interaction.Token}
		msg, err := session.FollowupMessageCreate(in, req.Wait, params, opt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(msg)
	case OpDiscordCommands:
		var req DiscordCommandsRequest
		if err := decodeStrict(f.Payload, &req); err != nil {
			return nil, err
		}
		var cmds []*discordgo.ApplicationCommand
		if err := json.Unmarshal(req.Commands, &cmds); err != nil {
			return nil, fmt.Errorf("commands: %v", err)
		}
		if session.State == nil || session.State.User == nil {
			return nil, errors.New("discord gateway not ready (no application identity)")
		}
		out, err := session.ApplicationCommandBulkOverwrite(session.State.User.ID, "", cmds, opt)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}
	return nil, fmt.Errorf("unknown operation %q", f.Op)
}

var telegramMethods = map[string]map[string]bool{
	"getMe":       {},
	"sendMessage": {"chat_id": true, "text": true},
	"getUpdates":  {"timeout": true, "offset": true},
}

func (b *Broker) handleTelegram(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req TelegramCallRequest
	if err := decodeStrict(raw, &req); err != nil {
		return nil, err
	}
	allowed, ok := telegramMethods[req.Method]
	if !ok {
		return nil, fmt.Errorf("telegram method %q is not allowed", req.Method)
	}
	fields := map[string]json.RawMessage{}
	if len(req.Payload) > 0 && string(req.Payload) != "null" {
		if err := json.Unmarshal(req.Payload, &fields); err != nil {
			return nil, fmt.Errorf("telegram payload: %v", err)
		}
	}
	for k := range fields {
		if !allowed[k] {
			return nil, fmt.Errorf("telegram field %q is not allowed for %s", k, req.Method)
		}
	}
	switch req.Method {
	case "sendMessage":
		var chat, text string
		if json.Unmarshal(fields["chat_id"], &chat) != nil || ValidTelegramChat(chat) != nil {
			return nil, errors.New("telegram chat_id is not a chat id")
		}
		if json.Unmarshal(fields["text"], &text) != nil || len(text) > 4096 {
			return nil, errors.New("telegram text is missing or longer than 4096 bytes")
		}
	case "getUpdates":
		var timeout int
		if raw, ok := fields["timeout"]; ok && (json.Unmarshal(raw, &timeout) != nil || timeout < 0 || timeout > 25) {
			return nil, errors.New("telegram timeout must be 0-25 seconds")
		}
		var offset int64
		if raw, ok := fields["offset"]; ok && json.Unmarshal(raw, &offset) != nil {
			return nil, errors.New("telegram offset must be an integer")
		}
	}
	var body io.Reader
	if len(fields) > 0 {
		data, _ := json.Marshal(fields)
		body = bytes.NewReader(data)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, telegramAPIBase+b.telegramToken+"/"+req.Method, body)
	if err != nil {
		return nil, errors.New("telegram request could not be built")
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: %s", req.Method, b.redact(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("telegram %s returned HTTP %d without JSON", req.Method, resp.StatusCode)
	}
	return data, nil
}

func (b *Broker) handleGitHub(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req GitHubIssueRequest
	if err := decodeStrict(raw, &req); err != nil {
		return nil, err
	}
	if err := ValidRepo(req.Repo); err != nil {
		return nil, err
	}
	if !b.githubRepos[req.Repo] {
		return nil, fmt.Errorf("repository %s is not in %s", req.Repo, EnvGitHubRepos)
	}
	if strings.TrimSpace(req.Title) == "" || len(req.Title) > 256 || len(req.Body) > 65536 || len(req.Labels) > 10 {
		return nil, errors.New("issue title, body or labels are out of bounds")
	}
	payload, _ := json.Marshal(map[string]any{"title": req.Title, "body": req.Body, "labels": req.Labels})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAPIBase+"/repos/"+req.Repo+"/issues", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+b.githubToken)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "go-trader")
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("github request: %s", b.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github returned %d: %s", resp.StatusCode, b.redact(strings.TrimSpace(string(body))))
	}
	var out GitHubIssueResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}
	return json.Marshal(out)
}
