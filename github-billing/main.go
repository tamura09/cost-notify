package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	dateLayout                    = "2006-01-02"
	defaultGitHubAPIBaseURL       = "https://api.github.com"
	gitHubAPIVersion              = "2026-03-10"
	defaultExchangeRateAPIBaseURL = "https://api.frankfurter.dev"
	// GitHub bills in USD and the usage report carries no currency field.
	billingCurrency              = "USD"
	jpyCurrency                  = "JPY"
	discordContentLimit          = 2000
	discordEmbedTotalLimit       = 6000
	discordEmbedTitleLimit       = 256
	discordEmbedDescriptionLimit = 4096
	discordEmbedFieldNameLimit   = 256
	discordEmbedFieldValueLimit  = 1024
	discordEmbedFooterTextLimit  = 2048
	discordGitHubColor           = 0x24292f
	maxDiscordProductLines       = 10
	maxDiscordSKULines           = 15
	maxDiscordRepositoryLines    = 10
	truncationSuffix             = "..."
)

type parameterGetter interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type app struct {
	parameters parameterGetter
	httpClient *http.Client
	now        func() time.Time
}

// reportPeriod is always a UTC calendar month: GitHub's usage report is keyed
// by UTC day, and the year/month query selects those days. Shifting the period
// into Asia/Tokyo would label nine hours of the neighbouring month as this one.
type reportPeriod struct {
	Start time.Time
	End   time.Time
}

type usageItem struct {
	Date           string  `json:"date"`
	Product        string  `json:"product"`
	SKU            string  `json:"sku"`
	Quantity       float64 `json:"quantity"`
	UnitType       string  `json:"unitType"`
	PricePerUnit   float64 `json:"pricePerUnit"`
	GrossAmount    float64 `json:"grossAmount"`
	DiscountAmount float64 `json:"discountAmount"`
	NetAmount      float64 `json:"netAmount"`
	RepositoryName string  `json:"repositoryName"`
}

type usageReportResponse struct {
	UsageItems []usageItem `json:"usageItems"`
}

type amounts struct {
	Gross    float64
	Discount float64
	Net      float64
}

func (a *amounts) add(item usageItem) {
	a.Gross += item.GrossAmount
	a.Discount += item.DiscountAmount
	a.Net += item.NetAmount
}

type productTotal struct {
	Product string
	amounts
}

type skuTotal struct {
	Product  string
	SKU      string
	Quantity float64
	UnitType string
	amounts
}

type repositoryTotal struct {
	Repository string
	amounts
}

type monthlyReport struct {
	Period        reportPeriod
	Username      string
	Total         amounts
	Products      []productTotal
	SKUs          []skuTotal
	Repositories  []repositoryTotal
	JPYConversion *currencyConversion
	Warnings      []string
}

type currencyConversion struct {
	Base  string
	Quote string
	Rate  float64
	Date  string
}

type frankfurterRateResponse struct {
	Date  string  `json:"date"`
	Base  string  `json:"base"`
	Quote string  `json:"quote"`
	Rate  float64 `json:"rate"`
}

type discordWebhookPayload struct {
	Content         string                 `json:"content,omitempty"`
	Username        string                 `json:"username,omitempty"`
	Embeds          []discordEmbed         `json:"embeds,omitempty"`
	AllowedMentions discordAllowedMentions `json:"allowed_mentions"`
}

type discordAllowedMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Color       int                 `json:"color,omitempty"`
	Fields      []discordEmbedField `json:"fields,omitempty"`
	Footer      *discordEmbedFooter `json:"footer,omitempty"`
}

type discordEmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordEmbedFooter struct {
	Text string `json:"text"`
}

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		log.Fatalf("load AWS config: %v", err)
	}

	lambda.Start((&app{
		parameters: ssm.NewFromConfig(cfg),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		now: time.Now,
	}).handle)
}

func (application *app) handle(ctx context.Context) error {
	username, err := requiredEnv("GITHUB_USERNAME")
	if err != nil {
		return err
	}
	tokenParameterName, err := requiredEnv("GITHUB_TOKEN_PARAMETER_NAME")
	if err != nil {
		return err
	}
	webhookParameterName, err := requiredEnv("DISCORD_WEBHOOK_PARAMETER_NAME")
	if err != nil {
		return err
	}

	token, err := application.parameterString(ctx, tokenParameterName)
	if err != nil {
		return fmt.Errorf("read GitHub token parameter: %w", err)
	}

	period := previousMonth(application.now())
	items, err := application.usageItems(ctx, token, username, period)
	if err != nil {
		return err
	}
	report := reportFromUsageItems(period, username, items)
	if err := application.addJPYConversion(ctx, &report); err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("JPY conversion unavailable: %v", err))
	}

	webhookURL, err := application.parameterString(ctx, webhookParameterName)
	if err != nil {
		return fmt.Errorf("read Discord webhook parameter: %w", err)
	}

	return application.postDiscord(ctx, webhookURL, buildDiscordPayload(report))
}

// usageItems reads the GA usage report rather than the usage summary endpoint:
// the summary is a public preview "subject to change", and this runs unattended
// once a month, so a silent schema change would only show up as a wrong total.
// The report is per day and per repository; reportFromUsageItems sums it.
func (application *app) usageItems(ctx context.Context, token string, username string, period reportPeriod) ([]usageItem, error) {
	baseURL := strings.TrimRight(envOr("GITHUB_API_BASE_URL", defaultGitHubAPIBaseURL), "/")
	endpoint, err := url.Parse(baseURL + "/users/" + url.PathEscape(username) + "/settings/billing/usage")
	if err != nil {
		return nil, fmt.Errorf("parse GitHub usage URL: %w", err)
	}
	query := url.Values{}
	query.Set("year", strconv.Itoa(period.Start.Year()))
	query.Set("month", strconv.Itoa(int(period.Start.Month())))
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub usage request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", gitHubAPIVersion)
	request.Header.Set("User-Agent", "cost-notify-github-billing-lambda")

	response, err := application.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get GitHub billing usage: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		// GitHub answers 404, not 403, when the token lacks the permission, so
		// a missing "Plan" permission reads like a wrong username.
		return nil, fmt.Errorf("GitHub billing usage returned %s (a 404 also means the token lacks the Plan read permission): %s", response.Status, strings.TrimSpace(string(responseBody)))
	}

	var usage usageReportResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&usage); err != nil {
		return nil, fmt.Errorf("decode GitHub billing usage: %w", err)
	}
	return usage.UsageItems, nil
}

func reportFromUsageItems(period reportPeriod, username string, items []usageItem) monthlyReport {
	report := monthlyReport{
		Period:   period,
		Username: username,
	}
	productsByName := map[string]*productTotal{}
	skusByKey := map[string]*skuTotal{}
	repositoriesByName := map[string]*repositoryTotal{}

	for _, item := range items {
		product := nonEmpty(strings.TrimSpace(item.Product), "unknown")
		sku := nonEmpty(strings.TrimSpace(item.SKU), "unknown")
		report.Total.add(item)

		productLine, ok := productsByName[product]
		if !ok {
			productLine = &productTotal{Product: product}
			productsByName[product] = productLine
		}
		productLine.add(item)

		key := product + "\x00" + sku
		skuLine, ok := skusByKey[key]
		if !ok {
			skuLine = &skuTotal{Product: product, SKU: sku, UnitType: strings.TrimSpace(item.UnitType)}
			skusByKey[key] = skuLine
		}
		if unitType := strings.TrimSpace(item.UnitType); unitType != "" && skuLine.UnitType != unitType {
			if skuLine.UnitType == "" {
				skuLine.UnitType = unitType
			} else {
				skuLine.UnitType = "mixed"
			}
		}
		skuLine.Quantity += item.Quantity
		skuLine.add(item)

		// Usage without a repository (Copilot, for one) is already in the
		// product and SKU totals; a "no repository" row would only repeat it.
		if repository := strings.TrimSpace(item.RepositoryName); repository != "" {
			repositoryLine, ok := repositoriesByName[repository]
			if !ok {
				repositoryLine = &repositoryTotal{Repository: repository}
				repositoriesByName[repository] = repositoryLine
			}
			repositoryLine.add(item)
		}
	}

	// Lines with no gross amount are free usage that was never priced; they
	// would crowd out the lines that cost something.
	for _, line := range productsByName {
		if hasAmount(line.Gross) {
			report.Products = append(report.Products, *line)
		}
	}
	for _, line := range skusByKey {
		if hasAmount(line.Gross) {
			report.SKUs = append(report.SKUs, *line)
		}
	}
	for _, line := range repositoriesByName {
		if hasAmount(line.Gross) {
			report.Repositories = append(report.Repositories, *line)
		}
	}

	// Sorted by gross rather than net: the included quota discounts most usage
	// to zero, and gross is what shows where that quota went.
	sort.Slice(report.Products, func(left, right int) bool {
		return byGross(report.Products[left].amounts, report.Products[right].amounts, report.Products[left].Product, report.Products[right].Product)
	})
	sort.Slice(report.SKUs, func(left, right int) bool {
		return byGross(report.SKUs[left].amounts, report.SKUs[right].amounts, report.SKUs[left].Product+"/"+report.SKUs[left].SKU, report.SKUs[right].Product+"/"+report.SKUs[right].SKU)
	})
	sort.Slice(report.Repositories, func(left, right int) bool {
		return byGross(report.Repositories[left].amounts, report.Repositories[right].amounts, report.Repositories[left].Repository, report.Repositories[right].Repository)
	})

	return report
}

func byGross(left amounts, right amounts, leftName string, rightName string) bool {
	if left.Gross == right.Gross {
		return leftName < rightName
	}
	return left.Gross > right.Gross
}

func hasAmount(amount float64) bool {
	return math.Abs(amount) >= 0.000001
}

func (application *app) addJPYConversion(ctx context.Context, report *monthlyReport) error {
	conversion, err := application.exchangeRate(ctx, billingCurrency, jpyCurrency)
	if err != nil {
		return err
	}
	report.JPYConversion = conversion
	return nil
}

func (application *app) exchangeRate(ctx context.Context, baseCurrency string, quoteCurrency string) (*currencyConversion, error) {
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	quoteCurrency = strings.ToUpper(strings.TrimSpace(quoteCurrency))
	if baseCurrency == "" || quoteCurrency == "" {
		return nil, errors.New("currency code is empty")
	}
	if baseCurrency == quoteCurrency {
		return nil, nil
	}
	if application.httpClient == nil {
		return nil, errors.New("HTTP client is not configured")
	}

	baseURL := strings.TrimRight(envOr("EXCHANGE_RATE_API_BASE_URL", defaultExchangeRateAPIBaseURL), "/")
	endpoint, err := url.Parse(baseURL + "/v2/rate/" + url.PathEscape(baseCurrency) + "/" + url.PathEscape(quoteCurrency))
	if err != nil {
		return nil, fmt.Errorf("parse exchange rate URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create exchange rate request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "cost-notify-github-billing-lambda")

	response, err := application.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get exchange rate: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return nil, fmt.Errorf("exchange rate API returned %s and response body could not be read: %w", response.Status, readErr)
		}
		return nil, fmt.Errorf("exchange rate API returned %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}

	var rateResponse frankfurterRateResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&rateResponse); err != nil {
		return nil, fmt.Errorf("decode exchange rate response: %w", err)
	}
	if rateResponse.Rate <= 0 {
		return nil, fmt.Errorf("exchange rate for %s/%s is %.4f", baseCurrency, quoteCurrency, rateResponse.Rate)
	}
	return &currencyConversion{
		Base:  strings.ToUpper(nonEmpty(rateResponse.Base, baseCurrency)),
		Quote: strings.ToUpper(nonEmpty(rateResponse.Quote, quoteCurrency)),
		Rate:  rateResponse.Rate,
		Date:  strings.TrimSpace(rateResponse.Date),
	}, nil
}

// previousMonth reports the UTC month before now. The schedule fires on the
// 1st, after the month has closed, so the report holds settled usage.
func previousMonth(now time.Time) reportPeriod {
	utcNow := now.UTC()
	end := time.Date(utcNow.Year(), utcNow.Month(), 1, 0, 0, 0, 0, time.UTC)
	return reportPeriod{
		Start: end.AddDate(0, -1, 0),
		End:   end,
	}
}

func (application *app) parameterString(ctx context.Context, name string) (string, error) {
	output, err := application.parameters.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	if output.Parameter == nil {
		return "", fmt.Errorf("parameter %q has no value", name)
	}
	value := strings.TrimSpace(aws.ToString(output.Parameter.Value))
	if value == "" {
		return "", fmt.Errorf("parameter %q is empty", name)
	}
	return value, nil
}

func buildDiscordPayload(report monthlyReport) discordWebhookPayload {
	content := fmt.Sprintf("GitHub monthly billing report: %s", report.Period.displayRange())
	description := fmt.Sprintf(
		"Account: %s\nPeriod: %s (UTC)\nTotal: **%s**\nGross %s, discount %s",
		report.Username,
		report.Period.displayRange(),
		formatMoneyWithJPY(report.Total.Net, report.JPYConversion),
		formatMoney(report.Total.Gross),
		formatMoney(report.Total.Discount),
	)
	if report.JPYConversion != nil {
		description += fmt.Sprintf("\nJPY rate: 1 %s = %s %.4f (%s, Frankfurter)", report.JPYConversion.Base, report.JPYConversion.Quote, report.JPYConversion.Rate, nonEmpty(report.JPYConversion.Date, "latest"))
	}

	embed := discordEmbed{
		Title:       truncateString("GitHub monthly usage and billing", discordEmbedTitleLimit),
		Description: truncateString(description, discordEmbedDescriptionLimit),
		Color:       discordGitHubColor,
		Fields:      buildDiscordFields(report),
		Footer: &discordEmbedFooter{
			Text: truncateString("Metered usage from the GitHub billing usage report, sorted by gross amount. Net is after the included quota.", discordEmbedFooterTextLimit),
		},
	}

	for embedCharCount(embed) > discordEmbedTotalLimit && len(embed.Fields) > 0 {
		embed.Fields = embed.Fields[:len(embed.Fields)-1]
	}

	return discordWebhookPayload{
		Content:  truncateString(content, discordContentLimit),
		Username: "GitHub Billing",
		Embeds:   []discordEmbed{embed},
		AllowedMentions: discordAllowedMentions{
			Parse: []string{},
		},
	}
}

func buildDiscordFields(report monthlyReport) []discordEmbedField {
	fields := make([]discordEmbedField, 0, 4)

	productLines := make([]string, 0, min(len(report.Products), maxDiscordProductLines))
	for _, line := range report.Products[:min(len(report.Products), maxDiscordProductLines)] {
		productLines = append(productLines, fmt.Sprintf("- %s: %s", line.Product, formatAmounts(line.amounts, report.JPYConversion)))
	}
	fields = append(fields, discordEmbedField{
		Name:  "Products",
		Value: truncateString(linesWithOverflow(productLines, len(report.Products)), discordEmbedFieldValueLimit),
	})

	skuLines := make([]string, 0, min(len(report.SKUs), maxDiscordSKULines))
	for _, line := range report.SKUs[:min(len(report.SKUs), maxDiscordSKULines)] {
		skuLines = append(skuLines, fmt.Sprintf("- %s / %s: %s, %s", line.Product, line.SKU, formatAmounts(line.amounts, report.JPYConversion), formatUsage(line.Quantity, line.UnitType)))
	}
	fields = append(fields, discordEmbedField{
		Name:  "SKUs",
		Value: truncateString(linesWithOverflow(skuLines, len(report.SKUs)), discordEmbedFieldValueLimit),
	})

	if len(report.Repositories) > 0 {
		repositoryLines := make([]string, 0, min(len(report.Repositories), maxDiscordRepositoryLines))
		for _, line := range report.Repositories[:min(len(report.Repositories), maxDiscordRepositoryLines)] {
			repositoryLines = append(repositoryLines, fmt.Sprintf("- %s: %s", line.Repository, formatAmounts(line.amounts, report.JPYConversion)))
		}
		fields = append(fields, discordEmbedField{
			Name:  "Repositories",
			Value: truncateString(linesWithOverflow(repositoryLines, len(report.Repositories)), discordEmbedFieldValueLimit),
		})
	}

	if len(report.Warnings) > 0 {
		fields = append(fields, discordEmbedField{
			Name:  "Warnings",
			Value: truncateString(linesWithOverflow(report.Warnings, len(report.Warnings)), discordEmbedFieldValueLimit),
		})
	}

	return fields
}

// formatAmounts leads with net, which is what is actually charged, and adds
// gross only when the included quota took something off.
func formatAmounts(line amounts, conversion *currencyConversion) string {
	formatted := formatMoneyWithJPY(line.Net, conversion)
	if hasAmount(line.Discount) {
		formatted += fmt.Sprintf(" (gross %s)", formatMoney(line.Gross))
	}
	return formatted
}

func linesWithOverflow(lines []string, originalCount int) string {
	if len(lines) == 0 {
		return "None"
	}
	value := strings.Join(lines, "\n")
	if originalCount > len(lines) {
		value += fmt.Sprintf("\n- +%d more", originalCount-len(lines))
	}
	return value
}

func (period reportPeriod) displayRange() string {
	endInclusive := period.End.AddDate(0, 0, -1)
	return fmt.Sprintf("%s to %s", period.Start.Format(dateLayout), endInclusive.Format(dateLayout))
}

func (application *app) postDiscord(ctx context.Context, webhookURL string, payload discordWebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal Discord payload: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Discord request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := application.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("post Discord webhook: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("Discord webhook returned %s and response body could not be read: %w", response.Status, readErr)
		}
		return fmt.Errorf("Discord webhook returned %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}

	return nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func envOr(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func formatMoney(amount float64) string {
	if math.Abs(amount) > 0 && math.Abs(amount) < 0.01 {
		return fmt.Sprintf("%s %.4f", billingCurrency, amount)
	}
	return fmt.Sprintf("%s %.2f", billingCurrency, amount)
}

func formatMoneyWithJPY(amount float64, conversion *currencyConversion) string {
	formatted := formatMoney(amount)
	if conversion == nil || conversion.Rate <= 0 || !strings.EqualFold(billingCurrency, conversion.Base) {
		return formatted
	}
	return fmt.Sprintf("%s (%s)", formatted, formatJPY(amount*conversion.Rate))
}

func formatJPY(amount float64) string {
	return fmt.Sprintf("%s %s", jpyCurrency, formatIntegerWithCommas(int64(math.Round(amount))))
}

func formatIntegerWithCommas(amount int64) string {
	if amount == 0 {
		return "0"
	}
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	digits := strconv.FormatInt(amount, 10)
	firstGroupLength := len(digits) % 3
	if firstGroupLength == 0 {
		firstGroupLength = 3
	}
	var builder strings.Builder
	builder.WriteString(sign)
	builder.WriteString(digits[:firstGroupLength])
	for index := firstGroupLength; index < len(digits); index += 3 {
		builder.WriteString(",")
		builder.WriteString(digits[index : index+3])
	}
	return builder.String()
}

func formatUsage(amount float64, unit string) string {
	formattedAmount := formatNumber(amount)
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return formattedAmount
	}
	return formattedAmount + " " + unit
}

func formatNumber(amount float64) string {
	absoluteAmount := math.Abs(amount)
	format := "%.4f"
	if absoluteAmount >= 100 {
		format = "%.0f"
	} else if absoluteAmount >= 1 {
		format = "%.2f"
	}
	formatted := strings.TrimRight(strings.TrimRight(fmt.Sprintf(format, amount), "0"), ".")
	if formatted == "" || formatted == "-0" {
		return "0"
	}
	return formatted
}

func nonEmpty(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func truncateString(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if charLen(value) <= limit {
		return value
	}
	if limit <= charLen(truncationSuffix) {
		return string([]rune(value)[:limit])
	}
	runes := []rune(value)
	return string(runes[:limit-charLen(truncationSuffix)]) + truncationSuffix
}

func charLen(value string) int {
	return utf8.RuneCountInString(value)
}

func embedCharCount(embed discordEmbed) int {
	total := charLen(embed.Title) + charLen(embed.Description)
	if embed.Footer != nil {
		total += charLen(embed.Footer.Text)
	}
	for _, field := range embed.Fields {
		total += charLen(field.Name) + charLen(field.Value)
	}
	return total
}
