package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const fxMacroDataDefaultBaseURL = "https://fxmacrodata.com/api/v1/"

// FXMacroDataClient adds macro, FX, COT, commodity, and session context to
// scheduler workflows without adding third-party dependencies.
type FXMacroDataClient struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

func NewFXMacroDataClient(apiKey string) *FXMacroDataClient {
	return &FXMacroDataClient{
		APIKey:  strings.TrimSpace(apiKey),
		BaseURL: fxMacroDataDefaultBaseURL,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func NewFXMacroDataClientFromEnv() *FXMacroDataClient {
	apiKey := os.Getenv("FXMACRODATA_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("FXMD_API_KEY")
	}
	return NewFXMacroDataClient(apiKey)
}

func (c *FXMacroDataClient) Request(
	ctx context.Context,
	path string,
	params url.Values,
	result any,
) error {
	requestURL, err := c.buildURL(path, params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("fxmacrodata http %d for %s", resp.StatusCode, path)
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

func (c *FXMacroDataClient) DataCatalogue(
	ctx context.Context,
	currency string,
) (map[string]any, error) {
	return c.getMap(ctx, "data_catalogue/"+normaliseFXMacroDataCurrency(currency), nil)
}

func (c *FXMacroDataClient) Announcements(
	ctx context.Context,
	currency string,
	indicator string,
	params url.Values,
) (map[string]any, error) {
	path := fmt.Sprintf(
		"announcements/%s/%s",
		normaliseFXMacroDataCurrency(currency),
		indicator,
	)
	return c.getMap(ctx, path, params)
}

func (c *FXMacroDataClient) Calendar(
	ctx context.Context,
	currency string,
	params url.Values,
) (map[string]any, error) {
	return c.getMap(ctx, "calendar/"+normaliseFXMacroDataCurrency(currency), params)
}

func (c *FXMacroDataClient) Forex(
	ctx context.Context,
	base string,
	quote string,
	params url.Values,
) (map[string]any, error) {
	path := fmt.Sprintf(
		"forex/%s/%s",
		normaliseFXMacroDataCurrency(base),
		normaliseFXMacroDataCurrency(quote),
	)
	return c.getMap(ctx, path, params)
}

func (c *FXMacroDataClient) COT(
	ctx context.Context,
	currency string,
	params url.Values,
) (map[string]any, error) {
	return c.getMap(ctx, "cot/"+normaliseFXMacroDataCurrency(currency), params)
}

func (c *FXMacroDataClient) Commodity(
	ctx context.Context,
	indicator string,
	params url.Values,
) (map[string]any, error) {
	return c.getMap(ctx, "commodities/"+indicator, params)
}

func (c *FXMacroDataClient) MarketSessions(
	ctx context.Context,
	params url.Values,
) (map[string]any, error) {
	return c.getMap(ctx, "market_sessions", params)
}

func (c *FXMacroDataClient) RiskSentiment(
	ctx context.Context,
	params url.Values,
) (map[string]any, error) {
	return c.getMap(ctx, "risk_sentiment", params)
}

func (c *FXMacroDataClient) getMap(
	ctx context.Context,
	path string,
	params url.Values,
) (map[string]any, error) {
	var result map[string]any
	err := c.Request(ctx, path, params, &result)
	return result, err
}

func (c *FXMacroDataClient) buildURL(path string, params url.Values) (string, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/") + "/"
	u, err := url.Parse(baseURL + strings.TrimLeft(path, "/"))
	if err != nil {
		return "", err
	}
	query := u.Query()
	for key, values := range params {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	if c.APIKey != "" {
		query.Set("api_key", c.APIKey)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func normaliseFXMacroDataCurrency(currency string) string {
	return strings.ToLower(strings.TrimSpace(currency))
}
