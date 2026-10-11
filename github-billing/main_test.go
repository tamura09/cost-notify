package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func TestPreviousMonthIsTheUTCMonthBeforeNow(t *testing.T) {
	// 09:00 JST on the 1st is still the 1st in UTC, so the report is September.
	period := previousMonth(time.Date(2026, 10, 1, 18, 0, 0, 0, time.FixedZone("JST", 9*60*60)))

	if got, want := period.Start.Format(time.RFC3339), "2026-09-01T00:00:00Z"; got != want {
		t.Fatalf("period.Start = %s, want %s", got, want)
	}
	if got, want := period.End.Format(time.RFC3339), "2026-10-01T00:00:00Z"; got != want {
		t.Fatalf("period.End = %s, want %s", got, want)
	}
}

func TestPreviousMonthHandlesYearBoundary(t *testing.T) {
	period := previousMonth(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC))

	if got, want := period.Start.Format(dateLayout), "2025-12-01"; got != want {
		t.Fatalf("period.Start = %s, want %s", got, want)
	}
	if got, want := period.displayRange(), "2025-12-01 to 2025-12-31"; got != want {
		t.Fatalf("displayRange = %s, want %s", got, want)
	}
}

func TestReportFromUsageItemsAggregatesDaysAndRepositories(t *testing.T) {
	items := []usageItem{
		{Date: "2026-09-01T00:00:00Z", Product: "actions", SKU: "actions_linux", Quantity: 100, UnitType: "minutes", GrossAmount: 0.6, DiscountAmount: 0.6, RepositoryName: "tamura09/cost-notify"},
		{Date: "2026-09-02T00:00:00Z", Product: "actions", SKU: "actions_linux", Quantity: 200, UnitType: "minutes", GrossAmount: 1.2, DiscountAmount: 1.2, RepositoryName: "tamura09/terraform"},
		{Date: "2026-09-02T00:00:00Z", Product: "actions", SKU: "actions_macos", Quantity: 10, UnitType: "minutes", GrossAmount: 0.8, DiscountAmount: 0.3, NetAmount: 0.5, RepositoryName: "tamura09/terraform"},
		{Date: "2026-09-01T00:00:00Z", Product: "copilot", SKU: "copilot_premium_request", Quantity: 50, UnitType: "requests", GrossAmount: 2, NetAmount: 2},
		// Free usage that GitHub never priced must not show up as a line.
		{Date: "2026-09-01T00:00:00Z", Product: "git_lfs", SKU: "git_lfs_storage", Quantity: 1, UnitType: "gigabyte-hours", RepositoryName: "tamura09/ha"},
	}

	report := reportFromUsageItems(previousMonth(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), "tamura09", items)

	if got, want := report.Total, (amounts{Gross: 4.6, Discount: 2.1, Net: 2.5}); !closeAmounts(got, want) {
		t.Fatalf("total = %+v, want %+v", got, want)
	}

	if len(report.Products) != 2 {
		t.Fatalf("products = %+v, want actions and copilot only", report.Products)
	}
	if report.Products[0].Product != "actions" || !closeAmounts(report.Products[0].amounts, amounts{Gross: 2.6, Discount: 2.1, Net: 0.5}) {
		t.Fatalf("products[0] = %+v", report.Products[0])
	}
	if report.Products[1].Product != "copilot" {
		t.Fatalf("products[1] = %+v", report.Products[1])
	}

	if len(report.SKUs) != 3 {
		t.Fatalf("skus = %+v, want 3", report.SKUs)
	}
	if got := report.SKUs[1]; got.SKU != "actions_linux" || got.Quantity != 300 || got.UnitType != "minutes" {
		t.Fatalf("skus[1] = %+v, want actions_linux with 300 minutes", got)
	}

	if len(report.Repositories) != 2 {
		t.Fatalf("repositories = %+v, want terraform and cost-notify only", report.Repositories)
	}
	if got := report.Repositories[0]; got.Repository != "tamura09/terraform" || !closeAmounts(got.amounts, amounts{Gross: 2.0, Discount: 1.5, Net: 0.5}) {
		t.Fatalf("repositories[0] = %+v", got)
	}
}

func TestBuildDiscordPayloadShowsNetWithGrossOnlyWhenDiscounted(t *testing.T) {
	report := monthlyReport{
		Period:   previousMonth(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)),
		Username: "tamura09",
		Total:    amounts{Gross: 3, Discount: 1, Net: 2},
		Products: []productTotal{
			{Product: "copilot", amounts: amounts{Gross: 2, Net: 2}},
			{Product: "actions", amounts: amounts{Gross: 1, Discount: 1}},
		},
		JPYConversion: &currencyConversion{Base: "USD", Quote: "JPY", Rate: 150, Date: "2026-09-30"},
	}

	payload := buildDiscordPayload(report)
	embed := payload.Embeds[0]

	if !strings.Contains(embed.Description, "Period: 2026-09-01 to 2026-09-30 (UTC)") {
		t.Fatalf("description = %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "Total: **USD 2.00 (JPY 300)**") {
		t.Fatalf("description = %q", embed.Description)
	}
	products := embed.Fields[0].Value
	if !strings.Contains(products, "- copilot: USD 2.00 (JPY 300)\n") {
		t.Fatalf("products = %q", products)
	}
	if !strings.Contains(products, "- actions: USD 0.00 (JPY 0) (gross USD 1.00)") {
		t.Fatalf("products = %q", products)
	}
	for _, field := range embed.Fields {
		if field.Name == "Repositories (gross)" {
			t.Fatalf("repositories field should be omitted without repository usage: %+v", field)
		}
	}
	if len(payload.AllowedMentions.Parse) != 0 {
		t.Fatalf("allowed mentions = %+v", payload.AllowedMentions)
	}
}

func TestHandleReportsThePreviousMonth(t *testing.T) {
	var gotUsageQuery, gotAuthorization, gotAPIVersion string
	var posted discordWebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/users/tamura09/settings/billing/usage":
			gotUsageQuery = request.URL.RawQuery
			gotAuthorization = request.Header.Get("Authorization")
			gotAPIVersion = request.Header.Get("X-GitHub-Api-Version")
			writer.Write([]byte(`{"usageItems":[{"date":"2026-09-03T00:00:00Z","product":"actions","sku":"actions_linux","quantity":42,"unitType":"minutes","pricePerUnit":0.006,"grossAmount":0.252,"discountAmount":0.252,"netAmount":0,"repositoryName":"tamura09/cost-notify"}]}`))
		case request.URL.Path == "/v2/rate/USD/JPY":
			writer.Write([]byte(`{"date":"2026-09-30","base":"USD","quote":"JPY","rate":150}`))
		case request.URL.Path == "/webhook":
			body, _ := io.ReadAll(request.Body)
			if err := json.Unmarshal(body, &posted); err != nil {
				t.Errorf("decode webhook body: %v", err)
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", request.URL)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("GITHUB_USERNAME", "tamura09")
	t.Setenv("GITHUB_TOKEN_PARAMETER_NAME", "/billing/github-billing/token")
	t.Setenv("DISCORD_WEBHOOK_PARAMETER_NAME", "/billing/github-billing/discord-webhook-url")
	t.Setenv("GITHUB_API_BASE_URL", server.URL)
	t.Setenv("EXCHANGE_RATE_API_BASE_URL", server.URL)

	application := &app{
		parameters: fakeParameters{
			"/billing/github-billing/token":               " github_pat_test \n",
			"/billing/github-billing/discord-webhook-url": server.URL + "/webhook",
		},
		httpClient: server.Client(),
		now:        func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) },
	}

	if err := application.handle(context.Background()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if gotUsageQuery != "month=9&year=2026" {
		t.Fatalf("usage query = %q", gotUsageQuery)
	}
	if gotAuthorization != "Bearer github_pat_test" {
		t.Fatalf("authorization = %q", gotAuthorization)
	}
	if gotAPIVersion != gitHubAPIVersion {
		t.Fatalf("API version = %q", gotAPIVersion)
	}
	if len(posted.Embeds) != 1 {
		t.Fatalf("posted = %+v", posted)
	}
	if !strings.Contains(posted.Embeds[0].Fields[0].Value, "- actions: USD 0.00 (JPY 0) (gross USD 0.25)") {
		t.Fatalf("products = %q", posted.Embeds[0].Fields[0].Value)
	}
	if got := posted.Embeds[0].Fields[2]; got.Name != "Repositories (gross)" || got.Value != "- tamura09/cost-notify: gross USD 0.25 (JPY 38)" {
		t.Fatalf("repositories = %+v", got)
	}
}

func TestHandleFailsWhenGitHubRejectsTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/webhook" {
			t.Error("webhook must not be called when usage cannot be read")
		}
		writer.WriteHeader(http.StatusNotFound)
		writer.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	t.Setenv("GITHUB_USERNAME", "tamura09")
	t.Setenv("GITHUB_TOKEN_PARAMETER_NAME", "token")
	t.Setenv("DISCORD_WEBHOOK_PARAMETER_NAME", "webhook")
	t.Setenv("GITHUB_API_BASE_URL", server.URL)

	application := &app{
		parameters: fakeParameters{"token": "github_pat_test", "webhook": server.URL + "/webhook"},
		httpClient: server.Client(),
		now:        func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) },
	}

	err := application.handle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Plan read permission") {
		t.Fatalf("err = %v, want a hint about the Plan permission", err)
	}
}

type fakeParameters map[string]string

func (parameters fakeParameters) GetParameter(_ context.Context, input *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	value, ok := parameters[aws.ToString(input.Name)]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(value)}}, nil
}

func closeAmounts(left amounts, right amounts) bool {
	return !hasAmount(left.Gross-right.Gross) && !hasAmount(left.Discount-right.Discount) && !hasAmount(left.Net-right.Net)
}
