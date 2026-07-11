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
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	costMetric                    = "UnblendedCost"
	usageMetric                   = "UsageQuantity"
	dateLayout                    = "2006-01-02"
	defaultReportTimezone         = "Asia/Tokyo"
	defaultCurrency               = "USD"
	defaultExchangeRateAPIBaseURL = "https://api.frankfurter.dev"
	jpyCurrency                   = "JPY"
	discordContentLimit           = 2000
	discordEmbedTotalLimit        = 6000
	discordEmbedFieldLimit        = 25
	discordEmbedTitleLimit        = 256
	discordEmbedDescriptionLimit  = 4096
	discordEmbedFieldNameLimit    = 256
	discordEmbedFieldValueLimit   = 1024
	discordEmbedFooterTextLimit   = 2048
	discordAWSColor               = 0xff9900
	maxDiscordServiceTotalLines   = 10
	maxDiscordUsageBreakdownLines = 15
	truncationSuffix              = "..."
)

type costUsageGetter interface {
	GetCostAndUsage(context.Context, *costexplorer.GetCostAndUsageInput, ...func(*costexplorer.Options)) (*costexplorer.GetCostAndUsageOutput, error)
}

type parameterGetter interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type app struct {
	costExplorer costUsageGetter
	parameters   parameterGetter
	httpClient   *http.Client
	now          func() time.Time
}

type reportPeriod struct {
	Start        time.Time
	End          time.Time
	LocationName string
}

type monthlyReport struct {
	Period        reportPeriod
	Currency      string
	TotalCost     float64
	JPYConversion *currencyConversion
	ServiceTotals []serviceTotal
	UsageLines    []usageLine
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

type serviceTotal struct {
	Service string
	Cost    float64
}

type usageLine struct {
	Service     string
	UsageType   string
	UsageAmount float64
	UsageUnit   string
	Cost        float64
	CostUnit    string
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
		costExplorer: costexplorer.NewFromConfig(cfg),
		parameters:   ssm.NewFromConfig(cfg),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
		},
		now: time.Now,
	}).handle)
}

func (application *app) handle(ctx context.Context) error {
	webhookParameterName, err := requiredEnv("DISCORD_WEBHOOK_PARAMETER_NAME")
	if err != nil {
		return err
	}

	reportLocation, err := reportTimezone()
	if err != nil {
		return err
	}

	period := previousMonth(application.now(), reportLocation)
	report, err := application.costReport(ctx, period)
	if err != nil {
		return err
	}
	if err := application.addJPYConversion(ctx, &report); err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("JPY conversion unavailable: %v", err))
	}

	webhookURL, err := application.parameterString(ctx, webhookParameterName)
	if err != nil {
		return fmt.Errorf("read Discord webhook parameter: %w", err)
	}

	return application.postDiscord(ctx, webhookURL, buildDiscordPayload(report))
}

func (application *app) costReport(ctx context.Context, period reportPeriod) (monthlyReport, error) {
	input := &costexplorer.GetCostAndUsageInput{
		TimePeriod: &cetypes.DateInterval{
			Start: aws.String(period.Start.Format(dateLayout)),
			End:   aws.String(period.End.Format(dateLayout)),
		},
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{costMetric, usageMetric},
		GroupBy: []cetypes.GroupDefinition{
			{
				Type: cetypes.GroupDefinitionTypeDimension,
				Key:  aws.String("SERVICE"),
			},
			{
				Type: cetypes.GroupDefinitionTypeDimension,
				Key:  aws.String("USAGE_TYPE"),
			},
		},
	}
	combinedOutput := &costexplorer.GetCostAndUsageOutput{}
	for {
		output, err := application.costExplorer.GetCostAndUsage(ctx, input)
		if err != nil {
			return monthlyReport{}, fmt.Errorf("get cost and usage: %w", err)
		}
		combinedOutput.ResultsByTime = append(combinedOutput.ResultsByTime, output.ResultsByTime...)

		nextPageToken := aws.ToString(output.NextPageToken)
		if nextPageToken == "" {
			break
		}
		input.NextPageToken = aws.String(nextPageToken)
	}

	report, err := reportFromCostExplorer(period, combinedOutput)
	if err != nil {
		return monthlyReport{}, err
	}
	return report, nil
}

func (application *app) addJPYConversion(ctx context.Context, report *monthlyReport) error {
	conversion, err := application.exchangeRate(ctx, report.Currency, jpyCurrency)
	if err != nil {
		return err
	}
	report.JPYConversion = conversion
	return nil
}

func (application *app) exchangeRate(ctx context.Context, baseCurrency string, quoteCurrency string) (*currencyConversion, error) {
	baseCurrency = strings.ToUpper(nonEmpty(baseCurrency, defaultCurrency))
	quoteCurrency = strings.ToUpper(strings.TrimSpace(quoteCurrency))
	if quoteCurrency == "" {
		return nil, errors.New("quote currency is empty")
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
	request.Header.Set("User-Agent", "aws-monthly-billing-lambda")

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
	conversion := &currencyConversion{
		Base:  strings.ToUpper(nonEmpty(rateResponse.Base, baseCurrency)),
		Quote: strings.ToUpper(nonEmpty(rateResponse.Quote, quoteCurrency)),
		Rate:  rateResponse.Rate,
		Date:  strings.TrimSpace(rateResponse.Date),
	}
	return conversion, nil
}

func reportFromCostExplorer(period reportPeriod, output *costexplorer.GetCostAndUsageOutput) (monthlyReport, error) {
	if output == nil {
		return monthlyReport{}, errors.New("empty Cost Explorer response")
	}

	report := monthlyReport{
		Period:   period,
		Currency: defaultCurrency,
	}
	usageLinesByKey := make(map[string]*usageLine)
	serviceTotalsByName := make(map[string]float64)

	for _, result := range output.ResultsByTime {
		for _, group := range result.Groups {
			if len(group.Keys) < 2 {
				report.Warnings = append(report.Warnings, "Cost Explorer returned a group without service and usage type.")
				continue
			}

			service := normalizedName(group.Keys[0], "Unknown service")
			usageType := normalizedName(group.Keys[1], "Unknown usage type")
			cost, costUnit, err := metricValue(group.Metrics, costMetric)
			if err != nil {
				return monthlyReport{}, fmt.Errorf("%s %s cost: %w", service, usageType, err)
			}
			usage, usageUnit, err := metricValue(group.Metrics, usageMetric)
			if err != nil {
				return monthlyReport{}, fmt.Errorf("%s %s usage: %w", service, usageType, err)
			}

			if costUnit != "" {
				report.Currency = costUnit
			}

			key := service + "\x00" + usageType
			line, ok := usageLinesByKey[key]
			if !ok {
				line = &usageLine{
					Service:   service,
					UsageType: usageType,
					UsageUnit: usageUnit,
					CostUnit:  costUnit,
				}
				usageLinesByKey[key] = line
			}

			if line.UsageUnit == "" {
				line.UsageUnit = usageUnit
			} else if usageUnit != "" && line.UsageUnit != usageUnit {
				line.UsageUnit = "mixed"
			}
			if line.CostUnit == "" {
				line.CostUnit = costUnit
			}

			line.Cost += cost
			line.UsageAmount += usage
			serviceTotalsByName[service] += cost
		}
	}

	for _, line := range usageLinesByKey {
		report.UsageLines = append(report.UsageLines, *line)
		report.TotalCost += line.Cost
	}

	if len(report.UsageLines) == 0 {
		totalCost, totalUnit, err := totalMetric(output.ResultsByTime, costMetric)
		if err != nil {
			return monthlyReport{}, err
		}
		report.TotalCost = totalCost
		if totalUnit != "" {
			report.Currency = totalUnit
		}
	}

	for service, cost := range serviceTotalsByName {
		report.ServiceTotals = append(report.ServiceTotals, serviceTotal{Service: service, Cost: cost})
	}
	sort.Slice(report.ServiceTotals, func(leftIndex, rightIndex int) bool {
		left := report.ServiceTotals[leftIndex]
		right := report.ServiceTotals[rightIndex]
		if left.Cost == right.Cost {
			return left.Service < right.Service
		}
		return left.Cost > right.Cost
	})
	sort.Slice(report.UsageLines, func(leftIndex, rightIndex int) bool {
		left := report.UsageLines[leftIndex]
		right := report.UsageLines[rightIndex]
		if left.Cost == right.Cost {
			if left.Service == right.Service {
				return left.UsageType < right.UsageType
			}
			return left.Service < right.Service
		}
		return left.Cost > right.Cost
	})

	return report, nil
}

func totalMetric(results []cetypes.ResultByTime, metricName string) (float64, string, error) {
	var total float64
	unit := ""
	for _, result := range results {
		amount, metricUnit, err := metricValue(result.Total, metricName)
		if err != nil {
			return 0, "", fmt.Errorf("total %s: %w", metricName, err)
		}
		total += amount
		if metricUnit != "" {
			unit = metricUnit
		}
	}
	return total, unit, nil
}

func metricValue(metrics map[string]cetypes.MetricValue, metricName string) (float64, string, error) {
	metric, ok := metrics[metricName]
	if !ok {
		return 0, "", nil
	}
	amountText := strings.TrimSpace(aws.ToString(metric.Amount))
	if amountText == "" {
		return 0, aws.ToString(metric.Unit), nil
	}
	amount, err := strconv.ParseFloat(amountText, 64)
	if err != nil {
		return 0, "", err
	}
	return amount, aws.ToString(metric.Unit), nil
}

func previousMonth(now time.Time, location *time.Location) reportPeriod {
	localNow := now.In(location)
	end := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
	start := end.AddDate(0, -1, 0)
	return reportPeriod{
		Start:        start,
		End:          end,
		LocationName: location.String(),
	}
}

func reportTimezone() (*time.Location, error) {
	locationName := strings.TrimSpace(os.Getenv("REPORT_TIMEZONE"))
	if locationName == "" {
		locationName = defaultReportTimezone
	}
	location, err := time.LoadLocation(locationName)
	if err != nil {
		return nil, fmt.Errorf("load report timezone %q: %w", locationName, err)
	}
	return location, nil
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
	content := fmt.Sprintf("AWS monthly billing report: %s", report.Period.displayRange())
	description := fmt.Sprintf(
		"Period: %s (%s)\nTotal: **%s**",
		report.Period.displayRange(),
		report.Period.LocationName,
		formatMoneyWithJPY(report.TotalCost, report.Currency, report.JPYConversion),
	)
	if report.JPYConversion != nil {
		description += fmt.Sprintf("\nJPY rate: 1 %s = %s %.4f (%s, Frankfurter)", report.JPYConversion.Base, report.JPYConversion.Quote, report.JPYConversion.Rate, nonEmpty(report.JPYConversion.Date, "latest"))
	}

	embed := discordEmbed{
		Title:       truncateString("AWS monthly resource usage and cost", discordEmbedTitleLimit),
		Description: truncateString(description, discordEmbedDescriptionLimit),
		Color:       discordAWSColor,
		Fields:      buildDiscordFields(report),
		Footer: &discordEmbedFooter{
			Text: truncateString("Cost Explorer grouped by SERVICE and USAGE_TYPE. End date is exclusive.", discordEmbedFooterTextLimit),
		},
	}

	for embedCharCount(embed) > discordEmbedTotalLimit && len(embed.Fields) > 0 {
		embed.Fields = embed.Fields[:len(embed.Fields)-1]
	}

	return discordWebhookPayload{
		Content:  truncateString(content, discordContentLimit),
		Embeds:   []discordEmbed{embed},
		AllowedMentions: discordAllowedMentions{
			Parse: []string{},
		},
	}
}

func buildDiscordFields(report monthlyReport) []discordEmbedField {
	fields := make([]discordEmbedField, 0, 3)
	serviceLines := make([]string, 0, minInt(len(report.ServiceTotals), maxDiscordServiceTotalLines))
	for index, service := range report.ServiceTotals {
		if index >= maxDiscordServiceTotalLines {
			break
		}
		serviceLines = append(serviceLines, fmt.Sprintf("- %s: %s", service.Service, formatMoneyWithJPY(service.Cost, report.Currency, report.JPYConversion)))
	}
	fields = append(fields, discordEmbedField{
		Name:   truncateString("Service totals", discordEmbedFieldNameLimit),
		Value:  truncateString(linesWithOverflow(serviceLines, len(report.ServiceTotals)), discordEmbedFieldValueLimit),
		Inline: false,
	})

	usageLines := make([]string, 0, minInt(len(report.UsageLines), maxDiscordUsageBreakdownLines))
	for index, line := range report.UsageLines {
		if index >= maxDiscordUsageBreakdownLines {
			break
		}
		usageLines = append(usageLines, fmt.Sprintf(
			"- %s / %s: %s, %s",
			line.Service,
			line.UsageType,
			formatMoneyWithJPY(line.Cost, nonEmpty(line.CostUnit, report.Currency), report.JPYConversion),
			formatUsage(line.UsageAmount, line.UsageUnit),
		))
	}
	fields = append(fields, discordEmbedField{
		Name:   truncateString("Top usage lines", discordEmbedFieldNameLimit),
		Value:  truncateString(linesWithOverflow(usageLines, len(report.UsageLines)), discordEmbedFieldValueLimit),
		Inline: false,
	})

	if len(report.Warnings) > 0 && len(fields) < discordEmbedFieldLimit {
		fields = append(fields, discordEmbedField{
			Name:   truncateString("Warnings", discordEmbedFieldNameLimit),
			Value:  truncateString(linesWithOverflow(report.Warnings, len(report.Warnings)), discordEmbedFieldValueLimit),
			Inline: false,
		})
	}

	return fields
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

	_, _ = io.Copy(io.Discard, response.Body)
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

func normalizedName(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func formatMoney(amount float64, unit string) string {
	unit = nonEmpty(unit, defaultCurrency)
	if math.Abs(amount) > 0 && math.Abs(amount) < 0.01 {
		return fmt.Sprintf("%s %.4f", unit, amount)
	}
	return fmt.Sprintf("%s %.2f", unit, amount)
}

func formatMoneyWithJPY(amount float64, unit string, conversion *currencyConversion) string {
	unit = nonEmpty(unit, defaultCurrency)
	formatted := formatMoney(amount, unit)
	if conversion == nil || conversion.Rate <= 0 || !strings.EqualFold(unit, conversion.Base) {
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
	if unit == "" || unit == "N/A" {
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
	maxRunes := limit - charLen(truncationSuffix)
	runes := []rune(value)
	return string(runes[:maxRunes]) + truncationSuffix
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

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
