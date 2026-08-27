package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const defaultReportRepo = "richkuo/go-trader"

var githubAPIBaseURL = "https://api.github.com"

type githubIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

func buildIssueRequest(title, body, label, reporter string) githubIssueRequest {
	footer := "Filed via `/go-trader-report-an-issue`"
	if r := strings.TrimSpace(reporter); r != "" {
		footer += " by " + r
	}
	footer += "."
	fullBody := strings.TrimRight(body, "\n") + "\n\n---\n" + footer
	req := githubIssueRequest{Title: strings.TrimSpace(title), Body: fullBody}
	if l := strings.TrimSpace(label); l != "" {
		req.Labels = []string{l}
	}
	return req
}

func createGitHubIssue(ctx context.Context, client *http.Client, repo, token string, payload githubIssueRequest) (htmlURL string, number int, err error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("marshal issue: %w", err)
	}
	url := fmt.Sprintf("%s/repos/%s/issues", strings.TrimRight(githubAPIBaseURL, "/"), repo)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "go-trader")

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("github request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return "", 0, fmt.Errorf("github returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", 0, fmt.Errorf("parse github response: %w", err)
	}
	return parsed.HTMLURL, parsed.Number, nil
}

func interactionUserLabel(i *discordgo.InteractionCreate) string {
	var u *discordgo.User
	if i.Member != nil && i.Member.User != nil {
		u = i.Member.User
	}
	if i.User != nil {
		u = i.User
	}
	if u == nil {
		return ""
	}
	if u.Username != "" {
		return fmt.Sprintf("%s (Discord ID %s)", u.Username, u.ID)
	}
	return "Discord ID " + u.ID
}

func (d *DiscordNotifier) handleReport(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	title := optionString(data.Options, "title", "")
	body := optionString(data.Options, "body", "")
	label := optionString(data.Options, "label", "")
	deferAck(s, i)

	followup := func(msg string) {
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: truncateForDiscord(msg),
		})
	}

	if title == "" || body == "" {
		followup("Both `title` and `body` are required.")
		return
	}

	var dcfg DiscordConfig
	if d.cfg != nil {
		dcfg = d.cfg.Discord
	}
	token := dcfg.reportToken()
	if token == "" {
		followup("Issue reporting is not configured: set a GitHub token via the GO_TRADER_GITHUB_TOKEN env var (or discord.report_github_token).")
		return
	}

	payload := buildIssueRequest(title, body, label, interactionUserLabel(i))
	ctx, cancel := context.WithTimeout(shutdownReadOnlyCtx, 20*time.Second)
	defer cancel()
	url, number, err := createGitHubIssue(ctx, http.DefaultClient, dcfg.reportRepo(), token, payload)
	if err != nil {
		followup(fmt.Sprintf("Failed to file issue: %v", err))
		return
	}
	followup(fmt.Sprintf("✅ Opened issue #%d: %s", number, url))
}
