package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"trading-scheduler/internal/discordcmd"
	"trading-scheduler/internal/notifybroker"
)

type slashCommandAPI interface {
	ready() error
	identity(ctx context.Context) (string, error)
	list(ctx context.Context, appID, guildID string) ([]*discordgo.ApplicationCommand, error)
	create(ctx context.Context, appID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error)
	edit(ctx context.Context, appID, cmdID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error)
	remove(ctx context.Context, appID, guildID, cmdID string) error
}

var errSlashIdentityPending = errors.New("the Discord application identity is not available yet")

type slashCommandDeferred struct {
	reason string
}

func (e *slashCommandDeferred) Error() string {
	return e.reason
}

func (d *DiscordNotifier) slashCommandAPI() slashCommandAPI {
	if d == nil {
		return nil
	}
	if d.broker != nil {
		return &brokerSlashCommandAPI{c: d.broker.c}
	}
	if d.session == nil {
		return nil
	}
	return &directSlashCommandAPI{s: d.session, c: discordcmd.New(d.session.Client, d.session.Token, d.session.UserAgent)}
}

type directSlashCommandAPI struct {
	s *discordgo.Session
	c *discordcmd.Client
}

func (a *directSlashCommandAPI) ready() error {
	return nil
}

func (a *directSlashCommandAPI) identity(context.Context) (string, error) {
	st := a.s.State
	if st == nil {
		return "", errSlashIdentityPending
	}
	st.RLock()
	id := ""
	if st.User != nil {
		id = st.User.ID
	}
	st.RUnlock()
	if id == "" {
		return "", errSlashIdentityPending
	}
	return id, nil
}

func (a *directSlashCommandAPI) list(ctx context.Context, appID, guildID string) ([]*discordgo.ApplicationCommand, error) {
	return a.c.List(ctx, appID, guildID)
}

func (a *directSlashCommandAPI) create(ctx context.Context, appID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	return a.c.Create(ctx, appID, "", cmd)
}

func (a *directSlashCommandAPI) edit(ctx context.Context, appID, cmdID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	return a.c.Edit(ctx, appID, "", cmdID, cmd)
}

func (a *directSlashCommandAPI) remove(ctx context.Context, appID, guildID, cmdID string) error {
	return a.c.Delete(ctx, appID, guildID, cmdID)
}

type brokerSlashCommandAPI struct {
	c *notifybroker.Client
}

func (a *brokerSlashCommandAPI) ready() error {
	if !a.c.Connected() {
		return &slashCommandDeferred{reason: "the notification broker is not connected"}
	}
	if !a.c.Hello().Has(notifybroker.CapabilityCommandPolicy) {
		return &slashCommandDeferred{reason: "the notification broker runs an older build without the command policy; command maintenance waits until it is updated" + brokerUpgradeHint()}
	}
	return nil
}

func (a *brokerSlashCommandAPI) identity(ctx context.Context) (string, error) {
	if id := a.c.Hello().DiscordUserID; id != "" {
		return id, nil
	}
	hello, err := a.c.RefreshHello(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("%w: %v", errSlashIdentityPending, err)
	}
	if hello.DiscordUserID == "" {
		return "", errSlashIdentityPending
	}
	return hello.DiscordUserID, nil
}

func brokerTimeoutMS(ctx context.Context) int64 {
	dl, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	ms := time.Until(dl).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms
}

func (a *brokerSlashCommandAPI) call(ctx context.Context, op string, req any, out any) error {
	if err := a.ready(); err != nil {
		return err
	}
	err := a.c.CallCancellable(ctx, op, req, out)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var ce *notifybroker.CallError
	if errors.As(err, &ce) && ce.Detail != nil {
		return &discordcmd.Error{
			Op:          op,
			HTTPStatus:  ce.Detail.HTTPStatus,
			Code:        ce.Detail.Code,
			RateLimited: ce.Detail.RateLimited,
			RetryAfter:  time.Duration(ce.Detail.RetryAfterMS) * time.Millisecond,
			Global:      ce.Detail.Global,
			Message:     ce.Message,
		}
	}
	return err
}

func (a *brokerSlashCommandAPI) list(ctx context.Context, _ string, guildID string) ([]*discordgo.ApplicationCommand, error) {
	var out []*discordgo.ApplicationCommand
	err := a.call(ctx, notifybroker.OpDiscordCommandList, notifybroker.DiscordCommandListRequest{GuildID: guildID, TimeoutMS: brokerTimeoutMS(ctx)}, &out)
	return out, err
}

func (a *brokerSlashCommandAPI) create(ctx context.Context, _ string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	raw, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	var out discordgo.ApplicationCommand
	if err := a.call(ctx, notifybroker.OpDiscordCommandCreate, notifybroker.DiscordCommandCreateRequest{Command: raw, TimeoutMS: brokerTimeoutMS(ctx)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (a *brokerSlashCommandAPI) edit(ctx context.Context, _ string, cmdID string, cmd *discordgo.ApplicationCommand) (*discordgo.ApplicationCommand, error) {
	raw, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	var out discordgo.ApplicationCommand
	if err := a.call(ctx, notifybroker.OpDiscordCommandEdit, notifybroker.DiscordCommandEditRequest{ID: cmdID, Command: raw, TimeoutMS: brokerTimeoutMS(ctx)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (a *brokerSlashCommandAPI) remove(ctx context.Context, _ string, guildID, cmdID string) error {
	return a.call(ctx, notifybroker.OpDiscordCommandDelete, notifybroker.DiscordCommandDeleteRequest{GuildID: guildID, ID: cmdID, TimeoutMS: brokerTimeoutMS(ctx)}, nil)
}
