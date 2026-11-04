package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	quoteOwnerSetupRole     = "quote_owner"
	quoteOwnerSetupMaxBytes = 1 << 20
	quoteOwnerSetupAction   = "add-quote-owner"
	quoteSetupFetchTimeout  = 20 * time.Second
)

var quoteSetupAttachmentHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

type quoteOwnerSetup struct {
	Normalized []byte
	Digest     string
	Entry      json.RawMessage
	Strategy   StrategyConfig
}

func jsonDuplicateKeyError(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return fmt.Errorf("%s: object key is not a string", path)
				}
				if seen[key] {
					return fmt.Errorf("duplicate key %q at %s", key, path)
				}
				seen[key] = true
				if err := walk(path + "." + key); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		case '[':
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		}
		return nil
	}
	if err := walk("$"); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

func parseQuoteOwnerSetup(data []byte) (*quoteOwnerSetup, error) {
	if len(data) == 0 {
		return nil, errors.New("the quote_owner setup fragment is empty")
	}
	if len(data) > quoteOwnerSetupMaxBytes {
		return nil, fmt.Errorf("the quote_owner setup fragment is %d bytes; the ceiling is %d", len(data), quoteOwnerSetupMaxBytes)
	}
	if err := jsonDuplicateKeyError(data); err != nil {
		return nil, fmt.Errorf("the quote_owner setup fragment is not strict JSON: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("the quote_owner setup fragment must be a JSON object: %w", err)
	}
	for k := range top {
		if k != "role" && k != "strategies" {
			return nil, fmt.Errorf("the quote_owner setup fragment holds unknown key %q; it takes only \"role\" and \"strategies\" and never changes root settings", k)
		}
	}
	var role string
	if err := json.Unmarshal(top["role"], &role); err != nil || role != quoteOwnerSetupRole {
		return nil, fmt.Errorf("the setup fragment role must be %q, got %s", quoteOwnerSetupRole, strings.TrimSpace(string(top["role"])))
	}
	var list []json.RawMessage
	if err := json.Unmarshal(top["strategies"], &list); err != nil || len(list) != 1 {
		return nil, errors.New("the quote_owner setup fragment needs \"strategies\" with exactly one quote owner")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, list[0]); err != nil {
		return nil, err
	}
	entry := json.RawMessage(compact.Bytes())
	envelope, err := json.Marshal(map[string]interface{}{"strategies": []json.RawMessage{entry}})
	if err != nil {
		return nil, err
	}
	var errs []string
	errs = append(errs, validateStrategyJSONKeys(envelope)...)
	errs = append(errs, validateQuoteRawFields(envelope)...)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(entry, &raw); err != nil {
		return nil, fmt.Errorf("the quote owner must be a JSON object: %w", err)
	}
	if _, ok := raw["quote"]; !ok {
		errs = append(errs, "the quote owner needs a complete quote block")
	}
	var sc StrategyConfig
	if err := json.Unmarshal(entry, &sc); err != nil {
		errs = append(errs, fmt.Sprintf("the quote owner does not decode: %v", err))
	}
	if strings.TrimSpace(sc.ID) == "" {
		errs = append(errs, "the quote owner needs an id")
	}
	if rawEdgeGateMode(sc.Args).Kind != edgeGateModePaper {
		errs = append(errs, "the quote owner needs exactly one explicit paper mode in args (\"--mode=paper\")")
	}
	if open := effectiveOpenStrategy(sc); open != inventoryMarketMakingStrategyName {
		errs = append(errs, fmt.Sprintf("the quote owner's open strategy must be %q, got %q", inventoryMarketMakingStrategyName, open))
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the quote_owner setup fragment is refused:\n  %s", strings.Join(errs, "\n  "))
	}
	normalized, err := json.Marshal(struct {
		Role       string            `json:"role"`
		Strategies []json.RawMessage `json:"strategies"`
	}{quoteOwnerSetupRole, []json.RawMessage{entry}})
	if err != nil {
		return nil, err
	}
	return &quoteOwnerSetup{Normalized: normalized, Digest: quoteConfigDigest(normalized), Entry: entry, Strategy: sc}, nil
}

func (q *quoteOwnerSetup) description() string {
	h, _ := engineHoldFor(q.Strategy)
	return fmt.Sprintf("add-quote-owner: append held quote owner %s (paper, coin %s) from the fragment with sha256 %s. Full entry: %s. It is admitted as an explicit-paper research quote owner and stays held (%s) — no evaluation, order, fill or protection work runs for it. No existing entry or root setting changes.",
		q.Strategy.ID, quoteCoinOf(q.Strategy), q.Digest, string(q.Entry), h.message())
}

func addQuoteOwnerToRoot(root map[string]json.RawMessage, setup *quoteOwnerSetup) error {
	if setup == nil {
		return errors.New("no confirmed quote_owner setup")
	}
	list, err := configStrategies(root)
	if err != nil {
		return err
	}
	for _, raw := range list {
		if strategyRawID(raw) == setup.Strategy.ID {
			return fmt.Errorf("strategy %q already exists; the quote_owner setup never changes an existing entry", setup.Strategy.ID)
		}
	}
	list = append(list, append(json.RawMessage(nil), setup.Entry...))
	if err := setConfigStrategies(root, list); err != nil {
		return err
	}
	data, err := json.Marshal(root)
	if err != nil {
		return err
	}
	cfg, err := loadConfigDataOpts("", data, true, true, configLoadOptions{skipRegimeSideArtifact: true})
	if err != nil {
		return fmt.Errorf("the config with the new quote owner does not validate; nothing changed: %w", err)
	}
	if cfg.quoteLoad != nil {
		return errors.New("the quote_owner setup must not create a quote admission")
	}
	for _, sc := range cfg.Strategies {
		if sc.ID != setup.Strategy.ID {
			continue
		}
		if _, held := engineHoldFor(sc); !held {
			return fmt.Errorf("strategy %q would not be engine-held; nothing changed", sc.ID)
		}
		return nil
	}
	return fmt.Errorf("strategy %q is missing from the validated candidate", setup.Strategy.ID)
}

func validateQuoteSetupAttachmentURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("attachment URL is unreadable: %w", err)
	}
	if u.Scheme != "https" || !quoteSetupAttachmentHosts[strings.ToLower(u.Hostname())] || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("attachment URL host %q is not a Discord attachment CDN host; arbitrary remote config URLs are never fetched", u.Host)
	}
	return u, nil
}

func fetchQuoteSetupAttachment(client *http.Client, rawURL string, declaredSize int) ([]byte, error) {
	if declaredSize <= 0 || declaredSize > quoteOwnerSetupMaxBytes {
		return nil, fmt.Errorf("attachment size %d is outside (0, %d] bytes", declaredSize, quoteOwnerSetupMaxBytes)
	}
	u, err := validateQuoteSetupAttachmentURL(rawURL)
	if err != nil {
		return nil, err
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("attachment redirects are refused")
	}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("attachment download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment download returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, quoteOwnerSetupMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("attachment read failed: %w", err)
	}
	if len(body) > quoteOwnerSetupMaxBytes {
		return nil, fmt.Errorf("attachment exceeds %d bytes", quoteOwnerSetupMaxBytes)
	}
	if len(body) != declaredSize {
		return nil, fmt.Errorf("attachment is %d bytes but Discord declared %d", len(body), declaredSize)
	}
	return body, nil
}
