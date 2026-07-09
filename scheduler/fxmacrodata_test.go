package main

import (
	"net/url"
	"testing"
)

func TestFXMacroDataBuildURL(t *testing.T) {
	client := NewFXMacroDataClient("test-key")
	client.BaseURL = "https://example.com/api/v1/"

	params := url.Values{}
	params.Set("limit", "1")

	got, err := client.buildURL("forex/eur/usd", params)
	if err != nil {
		t.Fatal(err)
	}

	want := "https://example.com/api/v1/forex/eur/usd?api_key=test-key&limit=1"
	if got != want {
		t.Fatalf("buildURL() = %q, want %q", got, want)
	}
}

func TestNormaliseFXMacroDataCurrency(t *testing.T) {
	if got := normaliseFXMacroDataCurrency(" USD "); got != "usd" {
		t.Fatalf("normaliseFXMacroDataCurrency() = %q", got)
	}
}
