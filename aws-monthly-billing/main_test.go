package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

func TestPreviousMonthUsesReportLocation(t *testing.T) {
	location := time.FixedZone("JST", 9*60*60)
	period := previousMonth(time.Date(2026, 6, 21, 15, 30, 0, 0, location), location)

	if got, want := period.Start.Format(time.RFC3339), "2026-05-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.Start = %s, want %s", got, want)
	}
	if got, want := period.End.Format(time.RFC3339), "2026-06-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.End = %s, want %s", got, want)
	}
}

func TestPreviousMonthHandlesYearBoundary(t *testing.T) {
	location := time.FixedZone("JST", 9*60*60)
	period := previousMonth(time.Date(2026, 1, 3, 8, 0, 0, 0, location), location)

	if got, want := period.Start.Format(time.RFC3339), "2025-12-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.Start = %s, want %s", got, want)
	}
	if got, want := period.End.Format(time.RFC3339), "2026-01-01T00:00:00+09:00"; got != want {
		t.Fatalf("period.End = %s, want %s", got, want)
	}
}

func TestReportFromCostExplorerAggregatesDailyGroups(t *testing.T) {
	period := reportPeriod{
		Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LocationName: "UTC",
	}
	output := &costexplorer.GetCostAndUsageOutput{
		ResultsByTime: []cetypes.ResultByTime{
			{
				Groups: []cetypes.Group{
					costGroup("Amazon Elastic Compute Cloud - Compute", "BoxUsage:t4g.nano", "1.25", "12", "Hrs"),
					costGroup("Amazon Simple Storage Service", "TimedStorage-ByteHrs", "0.75", "250", "GB-Mo"),
				},
			},
			{
				Groups: []cetypes.Group{
					costGroup("Amazon Elastic Compute Cloud - Compute", "BoxUsage:t4g.nano", "2.50", "24", "Hrs"),
				},
			},
		},
	}

	report, err := reportFromCostExplorer(period, output)
	if err != nil {
		t.Fatalf("reportFromCostExplorer: %v", err)
	}

	if got, want := report.TotalCost, 4.50; got != want {
		t.Fatalf("report.TotalCost = %.2f, want %.2f", got, want)
	}
	if got, want := len(report.UsageLines), 2; got != want {
		t.Fatalf("usage line count = %d, want %d", got, want)
	}
	if got, want := report.UsageLines[0].Service, "Amazon Elastic Compute Cloud - Compute"; got != want {
		t.Fatalf("top service = %q, want %q", got, want)
	}
	if got, want := report.UsageLines[0].UsageAmount, 36.0; got != want {
		t.Fatalf("top usage amount = %.2f, want %.2f", got, want)
	}
	if got, want := len(report.ServiceTotals), 2; got != want {
		t.Fatalf("service total count = %d, want %d", got, want)
	}
	if got, want := report.ServiceTotals[0].Cost, 3.75; got != want {
		t.Fatalf("top service total = %.2f, want %.2f", got, want)
	}
}

func TestCostReportFollowsNextPageToken(t *testing.T) {
	period := reportPeriod{
		Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LocationName: "UTC",
	}
	costExplorer := &fakeCostExplorer{outputs: []*costexplorer.GetCostAndUsageOutput{
		{
			NextPageToken: aws.String("page-2"),
			ResultsByTime: []cetypes.ResultByTime{
				{Groups: []cetypes.Group{costGroup("Service A", "Usage A", "1.00", "1", "Hrs")}},
			},
		},
		{
			ResultsByTime: []cetypes.ResultByTime{
				{Groups: []cetypes.Group{costGroup("Service B", "Usage B", "2.00", "2", "Hrs")}},
			},
		},
	}}

	report, err := (&app{costExplorer: costExplorer}).costReport(context.Background(), period)
	if err != nil {
		t.Fatalf("costReport: %v", err)
	}

	if got, want := len(costExplorer.inputs), 2; got != want {
		t.Fatalf("GetCostAndUsage calls = %d, want %d", got, want)
	}
	if got, want := aws.ToString(costExplorer.inputs[1].NextPageToken), "page-2"; got != want {
		t.Fatalf("second NextPageToken = %q, want %q", got, want)
	}
	if got, want := report.TotalCost, 3.00; got != want {
		t.Fatalf("report.TotalCost = %.2f, want %.2f", got, want)
	}
}

func TestBuildDiscordPayloadUsesEmbedsAndStaysWithinLimits(t *testing.T) {
	usageLines := make([]usageLine, 0, 200)
	serviceTotals := make([]serviceTotal, 0, 200)
	for index := range 200 {
		serviceName := "Service " + strings.Repeat("x", 30)
		usageLines = append(usageLines, usageLine{
			Service:     serviceName,
			UsageType:   "UsageType-" + strings.Repeat("y", 30),
			UsageAmount: float64(index) + 0.5,
			UsageUnit:   "Hrs",
			Cost:        float64(index),
			CostUnit:    "USD",
		})
		serviceTotals = append(serviceTotals, serviceTotal{Service: serviceName, Cost: float64(index)})
	}

	payload := buildDiscordPayload(monthlyReport{
		Period: reportPeriod{
			Start:        time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			End:          time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			LocationName: "UTC",
		},
		Currency:      "USD",
		TotalCost:     19900,
		JPYConversion: &currencyConversion{Base: "USD", Quote: "JPY", Rate: 161.45, Date: "2026-06-22"},
		ServiceTotals: serviceTotals,
		UsageLines:    usageLines,
	})

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
	if embed.Title != "AWS monthly resource usage and cost" {
		t.Fatalf("embed title = %q", embed.Title)
	}
	if embed.Color != discordAWSColor {
		t.Fatalf("embed color = %#x, want %#x", embed.Color, discordAWSColor)
	}
	if !strings.Contains(embed.Description, "Total: **USD 19900.00 (JPY 3,212,855)**") {
		t.Fatalf("embed description does not include JPY total: %q", embed.Description)
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

func TestExchangeRateFetchesFrankfurterRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v2/rate/USD/JPY"; got != want {
			t.Fatalf("request path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"date":"2026-06-22","base":"USD","quote":"JPY","rate":161.45}`))
	}))
	defer server.Close()
	t.Setenv("EXCHANGE_RATE_API_BASE_URL", server.URL)

	conversion, err := (&app{httpClient: server.Client()}).exchangeRate(context.Background(), "USD", "JPY")
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

func costGroup(service string, usageType string, cost string, usage string, usageUnit string) cetypes.Group {
	return cetypes.Group{
		Keys: []string{service, usageType},
		Metrics: map[string]cetypes.MetricValue{
			costMetric: {
				Amount: aws.String(cost),
				Unit:   aws.String("USD"),
			},
			usageMetric: {
				Amount: aws.String(usage),
				Unit:   aws.String(usageUnit),
			},
		},
	}
}

type fakeCostExplorer struct {
	inputs  []*costexplorer.GetCostAndUsageInput
	outputs []*costexplorer.GetCostAndUsageOutput
}

func (fake *fakeCostExplorer) GetCostAndUsage(_ context.Context, input *costexplorer.GetCostAndUsageInput, _ ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error) {
	requestCopy := *input
	fake.inputs = append(fake.inputs, &requestCopy)
	if len(fake.inputs) > len(fake.outputs) {
		return &costexplorer.GetCostAndUsageOutput{}, nil
	}
	return fake.outputs[len(fake.inputs)-1], nil
}
