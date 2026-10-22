package notifybroker

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	MaxFrameBytes = 16 << 20

	FrameRequest  = "request"
	FrameResponse = "response"
	FrameEvent    = "event"

	OpHello              = "hello"
	OpDiscordSend        = "discord.send"
	OpDiscordUserChannel = "discord.user_channel"
	OpDiscordRespond     = "discord.interaction_respond"
	OpDiscordFollowup    = "discord.followup"
	OpDiscordCommands    = "discord.register_commands"
	OpTelegramCall       = "telegram.call"
	OpGitHubIssue        = "github.create_issue"

	EventDiscordInteraction = "discord.interaction"
	EventDiscordMessage     = "discord.message"

	EnvGitHubRepos = "GO_TRADER_NOTIFY_GITHUB_REPOS"
)

type Frame struct {
	ID      uint64          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Op      string          `json:"op,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type HelloRequest struct {
	Events []string `json:"events,omitempty"`
}

type HelloResponse struct {
	DiscordUserID string `json:"discord_user_id,omitempty"`
	Discord       bool   `json:"discord"`
	Telegram      bool   `json:"telegram"`
	GitHub        bool   `json:"github"`
}

type DiscordSendRequest struct {
	ChannelID string `json:"channel_id"`
	Content   string `json:"content"`
}

type DiscordUserChannelRequest struct {
	UserID string `json:"user_id"`
}

type InteractionRef struct {
	ID    string `json:"id"`
	AppID string `json:"app_id"`
	Token string `json:"token"`
}

type DiscordRespondRequest struct {
	Interaction InteractionRef  `json:"interaction"`
	Response    json.RawMessage `json:"response"`
}

type FileAttachment struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	Data        []byte `json:"data"`
}

type DiscordFollowupRequest struct {
	Interaction InteractionRef   `json:"interaction"`
	Wait        bool             `json:"wait"`
	Content     string           `json:"content,omitempty"`
	Flags       int              `json:"flags,omitempty"`
	Files       []FileAttachment `json:"files,omitempty"`
}

type DiscordCommandsRequest struct {
	Commands json.RawMessage `json:"commands"`
}

type TelegramCallRequest struct {
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type GitHubIssueRequest struct {
	Repo   string   `json:"repo"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

type GitHubIssueResponse struct {
	HTMLURL string `json:"html_url"`
	Number  int    `json:"number"`
}

var (
	snowflakePattern   = regexp.MustCompile(`^[0-9]{1,25}$`)
	tokenPattern       = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,600}$`)
	repoPattern        = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,100}/[A-Za-z0-9_.\-]{1,100}$`)
	fileNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,100}$`)
	contentTypePattern = regexp.MustCompile(`^[a-z]+/[A-Za-z0-9.+\-]+(; ?charset=[A-Za-z0-9\-]+)?$`)
	telegramChatID     = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
)

func ValidSnowflake(s string) error {
	if !snowflakePattern.MatchString(s) {
		return errors.New("not a Discord identifier")
	}
	return nil
}

func (r InteractionRef) Validate() error {
	if err := ValidSnowflake(r.ID); err != nil {
		return fmt.Errorf("interaction id: %w", err)
	}
	if err := ValidSnowflake(r.AppID); err != nil {
		return fmt.Errorf("interaction app id: %w", err)
	}
	if !tokenPattern.MatchString(r.Token) {
		return errors.New("interaction token is malformed")
	}
	return nil
}

func ValidRepo(repo string) error {
	if !repoPattern.MatchString(repo) || strings.Contains(repo, "..") {
		return errors.New("repository must be owner/name")
	}
	return nil
}

func ValidFile(f FileAttachment) error {
	if !fileNamePattern.MatchString(f.Name) {
		return fmt.Errorf("attachment name %q is not allowed", f.Name)
	}
	if f.ContentType != "" && !contentTypePattern.MatchString(f.ContentType) {
		return fmt.Errorf("attachment content type %q is not allowed", f.ContentType)
	}
	if len(f.Data) > 8<<20 {
		return errors.New("attachment is larger than 8 MiB")
	}
	return nil
}

func ValidTelegramChat(id string) error {
	if !telegramChatID.MatchString(id) {
		return errors.New("not a Telegram chat id")
	}
	return nil
}

func readFrame(r *bufio.Reader) (*Frame, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxFrameBytes {
			return nil, fmt.Errorf("frame exceeds %d bytes", MaxFrameBytes)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) == 0 {
			return nil, io.EOF
		}
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(buf, &f); err != nil {
		return nil, fmt.Errorf("malformed frame: %v", err)
	}
	return &f, nil
}

func encodeFrame(f *Frame) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFrameBytes {
		return nil, fmt.Errorf("frame exceeds %d bytes", MaxFrameBytes)
	}
	return append(data, '\n'), nil
}
