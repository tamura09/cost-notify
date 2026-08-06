package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseProjectTokensSupportsWrapperAndMap(t *testing.T) {
	projects, err := parseProjectTokens(`{"projects":[{"name":"prod","token":"prod-token"}],"tokens":{"dev":"dev-token"}}`)
	if err != nil {
		t.Fatalf("parse wrapper secret: %v", err)
	}

	if len(projects) != 2 {
		t.Fatalf("project count = %d, want 2", len(projects))
	}
	if projects[0] != (projectToken{Name: "dev", Token: "dev-token"}) {
		t.Fatalf("projects[0] = %#v", projects[0])
	}
	if projects[1] != (projectToken{Name: "prod", Token: "prod-token"}) {
		t.Fatalf("projects[1] = %#v", projects[1])
	}
}

func TestParseProjectTokensSupportsSingleRawToken(t *testing.T) {
	projects, err := parseProjectTokens("raw-token")
	if err != nil {
		t.Fatalf("parse raw token: %v", err)
	}

	if len(projects) != 1 {
		t.Fatalf("project count = %d, want 1", len(projects))
	}
	if projects[0] != (projectToken{Name: "default", Token: "raw-token"}) {
		t.Fatalf("projects[0] = %#v", projects[0])
	}
}

func TestPreviousMonthUsesReportLocation(t *testing.T) {
	location := time.FixedZone("JST", 9*60*60)
	period := previousMonth(time.Date(2026, 6, 20, 12, 0, 0, 0, location), location)

	if got, want := period.Start.Format(time.RFC3339), "2026-05-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.Start = %s, want %s", got, want)
	}
	if got, want := period.End.Format(time.RFC3339), "2026-06-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.End = %s, want %s", got, want)
	}
}

func TestRecurringCostCapsHourlyAtMonthlyPrice(t *testing.T) {
	cost, ok := recurringCost(recurringPrice{Hourly: 1, HasHourly: true, Monthly: 10, HasMonthly: true}, 20, 720)
	if !ok {
		t.Fatal("recurringCost returned ok=false")
	}
	if cost != 10 {
		t.Fatalf("cost = %.2f, want 10.00", cost)
	}
}

func TestRecurringCostProratesMonthlyOnlyPrice(t *testing.T) {
	cost, ok := recurringCost(recurringPrice{Monthly: 30, HasMonthly: true}, 360, 720)
	if !ok {
		t.Fatal("recurringCost returned ok=false")
	}
	if cost != 15 {
		t.Fatalf("cost = %.2f, want 15.00", cost)
	}
}

func TestAddServerItemsUsesLocationField(t *testing.T) {
	var servers []serverResource
	if err := json.Unmarshal([]byte(`[{
		"id": 1,
		"name": "web",
		"created": "2026-05-01T00:00:00Z",
		"server_type": {"name": "cpx11"},
		"location": {"name": "ash"},
		"backup_window": null
	}]`), &servers); err != nil {
		t.Fatalf("decode servers: %v", err)
	}

	catalog := priceCatalog{
		Currency: "USD",
		ServerTypes: map[string][]locationPrice{
			"cpx11": {
				{Location: "fsn1", PriceHourly: &amount{Net: "0.0082"}, PriceMonthly: &amount{Net: "5.99"}},
				{Location: "ash", PriceHourly: &amount{Net: "0.0281"}, PriceMonthly: &amount{Net: "20.49"}},
			},
		},
	}
	report := projectReport{}
	addServerItems(&report, catalog, testPeriod(), servers)

	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", report.Warnings)
	}
	if len(report.Items) != 1 {
		t.Fatalf("item count = %d, want 1", len(report.Items))
	}
	if got, want := report.Items[0].Location, "ash"; got != want {
		t.Fatalf("item location = %q, want %q", got, want)
	}
	if got, want := report.Items[0].Cost, 20.49; got != want {
		t.Fatalf("cost = %.2f, want %.2f", got, want)
	}
}

func TestAddPrimaryIPItemsUsesLocationFieldAndSkipsIPv6(t *testing.T) {
	var primaryIPs []primaryIPResource
	if err := json.Unmarshal([]byte(`[
		{"id": 1, "name": "ip-v4", "ip": "203.0.113.1", "created": "2026-05-01T00:00:00Z", "type": "ipv4", "location": {"name": "hel1"}},
		{"id": 2, "name": "ip-v6", "ip": "2001:db8::/64", "created": "2026-05-01T00:00:00Z", "type": "ipv6", "location": {"name": "hel1"}}
	]`), &primaryIPs); err != nil {
		t.Fatalf("decode primary IPs: %v", err)
	}

	catalog := priceCatalog{
		Currency: "USD",
		PrimaryIPs: map[string][]locationPrice{
			"ipv4": {
				{Location: "fsn1", PriceHourly: &amount{Net: "0.001"}, PriceMonthly: &amount{Net: "0.60"}},
				{Location: "hel1", PriceHourly: &amount{Net: "0.001"}, PriceMonthly: &amount{Net: "0.60"}},
			},
		},
	}
	report := projectReport{}
	addPrimaryIPItems(&report, catalog, testPeriod(), primaryIPs)

	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", report.Warnings)
	}
	if len(report.Items) != 1 {
		t.Fatalf("item count = %d, want 1", len(report.Items))
	}
	if got, want := report.Items[0].Location, "hel1"; got != want {
		t.Fatalf("item location = %q, want %q", got, want)
	}
}

func TestPriceForLocationDoesNotGuessBetweenDifferentPrices(t *testing.T) {
	prices := []locationPrice{
		{Location: "fsn1", PriceMonthly: &amount{Net: "5.99"}},
		{Location: "ash", PriceMonthly: &amount{Net: "20.49"}},
	}
	if _, ok := priceForLocation(prices, ""); ok {
		t.Fatal("priceForLocation returned a price for an unknown location")
	}

	uniform := []locationPrice{
		{Location: "fsn1", PriceMonthly: &amount{Net: "0.60"}},
		{Location: "ash", PriceMonthly: &amount{Net: "0.60"}},
	}
	price, ok := priceForLocation(uniform, "")
	if !ok {
		t.Fatal("priceForLocation returned ok=false for uniform prices")
	}
	if got, want := price.PriceMonthly.Net, "0.60"; got != want {
		t.Fatalf("price = %q, want %q", got, want)
	}
}

func testPeriod() billingPeriod {
	return billingPeriod{
		Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LocationName: "UTC",
	}
}

func TestBuildDiscordPayloadUsesEmbedsAndStaysWithinLimits(t *testing.T) {
	items := make([]lineItem, 0, 200)
	for index := 0; index < 200; index++ {
		items = append(items, lineItem{
			Category: "server",
			Name:     strings.Repeat("x", 20),
			Detail:   "cpx11, 720.0h",
			Cost:     float64(index),
		})
	}

	payload := buildDiscordPayload(
		billingPeriod{
			Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			LocationName: "UTC",
		},
		[]projectReport{{Name: "prod", Currency: "EUR", Items: items, JPYConversion: &currencyConversion{Base: "EUR", Quote: "JPY", Rate: 161.45, Date: "2026-06-22"}, Total: 19900}},
	)

	if charLen(payload.Content) > discordContentLimit {
		t.Fatalf("content length = %d, want <= %d", charLen(payload.Content), discordContentLimit)
	}
	if len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("allowed mentions parse = %#v, want empty", payload.AllowedMentions.Parse)
	}
	if len(payload.Embeds) != 1 {
		t.Fatalf("embed count = %d, want 1", len(payload.Embeds))
	}

	embed := payload.Embeds[0]
	if embed.Title != "Hetzner Cloud billing estimate" {
		t.Fatalf("embed title = %q", embed.Title)
	}
	if embed.Color != discordHetznerColor {
		t.Fatalf("embed color = %#x, want %#x", embed.Color, discordHetznerColor)
	}
	if embedCharCount(embed) > discordEmbedTotalLimit {
		t.Fatalf("embed length = %d, want <= %d", embedCharCount(embed), discordEmbedTotalLimit)
	}
	if len(embed.Fields) == 0 {
		t.Fatal("embed fields are empty")
	}
	for _, field := range embed.Fields {
		if charLen(field.Name) > discordEmbedFieldNameLimit {
			t.Fatalf("field name length = %d, want <= %d", charLen(field.Name), discordEmbedFieldNameLimit)
		}
		if charLen(field.Value) > discordEmbedFieldValueLimit {
			t.Fatalf("field value length = %d, want <= %d", charLen(field.Value), discordEmbedFieldValueLimit)
		}
	}
}

func TestBuildDiscordPayloadOmitsZeroCostRows(t *testing.T) {
	payload := buildDiscordPayload(
		billingPeriod{
			Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			LocationName: "UTC",
		},
		[]projectReport{{
			Name:     "prod",
			Currency: "EUR",
			Items: []lineItem{
				{Category: "server", Name: "free-resource", Detail: "free", Cost: 0},
				{Category: "server", Name: "paid-resource", Detail: "cpx11", Cost: 1},
			},
			Total: 1,
		}},
	)

	if len(payload.Embeds) != 1 {
		t.Fatalf("embed count = %d, want 1", len(payload.Embeds))
	}
	if !strings.Contains(payload.Embeds[0].Fields[0].Value, "paid-resource") {
		t.Fatalf("embed omits paid rows: %q", payload.Embeds[0].Fields[0].Value)
	}
	if strings.Contains(payload.Embeds[0].Fields[0].Value, "free-resource") {
		t.Fatalf("embed includes zero-cost rows: %q", payload.Embeds[0].Fields[0].Value)
	}
}

func TestExchangeRateFetchesFrankfurterRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v2/rate/EUR/JPY"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"date":"2026-06-22","base":"EUR","quote":"JPY","rate":161.45}`))
	}))
	defer server.Close()
	t.Setenv("EXCHANGE_RATE_API_BASE_URL", server.URL)

	conversion, err := (&app{httpClient: server.Client()}).exchangeRate(context.Background(), "EUR", "JPY")
	if err != nil {
		t.Fatalf("exchangeRate: %v", err)
	}
	if conversion == nil {
		t.Fatal("conversion is nil")
	}
	if got, want := conversion.Rate, 161.45; got != want {
		t.Fatalf("conversion.Rate = %.4f, want %.4f", got, want)
	}
	if got, want := conversion.Date, "2026-06-22"; got != want {
		t.Fatalf("conversion.Date = %q, want %q", got, want)
	}
}
