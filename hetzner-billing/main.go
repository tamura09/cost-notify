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
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	defaultHetznerAPIBaseURL      = "https://api.hetzner.cloud/v1"
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
	discordHetznerColor           = 0xd50c2d
	truncationSuffix              = "..."
)

type app struct {
	parameters *ssm.Client
	httpClient *http.Client
	now        func() time.Time
}

type projectToken struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type billingPeriod struct {
	Start        time.Time
	End          time.Time
	LocationName string
}

type projectReport struct {
	Name          string
	Currency      string
	Items         []lineItem
	JPYConversion *currencyConversion
	Warnings      []string
	Errors        []string
	Total         float64
}

type lineItem struct {
	Category string
	Name     string
	Location string
	Detail   string
	Cost     float64
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

type hcloudClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type pricingEnvelope struct {
	Pricing pricing `json:"pricing"`
}

type pricing struct {
	Currency          string        `json:"currency"`
	VATRate           string        `json:"vat_rate"`
	ServerTypes       []typedPrices `json:"server_types"`
	LoadBalancerTypes []typedPrices `json:"load_balancer_types"`
	FloatingIPs       []ipPrices    `json:"floating_ips"`
	PrimaryIPs        []ipPrices    `json:"primary_ips"`
	Image             unitPricing   `json:"image"`
	Volume            unitPricing   `json:"volume"`
	ServerBackup      struct {
		Percentage string `json:"percentage"`
	} `json:"server_backup"`
}

type typedPrices struct {
	Name   string          `json:"name"`
	Prices []locationPrice `json:"prices"`
}

type ipPrices struct {
	Type   string          `json:"type"`
	Prices []locationPrice `json:"prices"`
}

type locationPrice struct {
	Location     string  `json:"location"`
	PriceHourly  *amount `json:"price_hourly"`
	PriceMonthly *amount `json:"price_monthly"`
}

type amount struct {
	Net   string `json:"net"`
	Gross string `json:"gross"`
}

type unitPricing struct {
	PricePerGBMonth amount `json:"price_per_gb_month"`
}

type recurringPrice struct {
	Hourly     float64
	HasHourly  bool
	Monthly    float64
	HasMonthly bool
}

type priceCatalog struct {
	Currency              string
	ServerBackupPercent   float64
	ServerTypes           map[string][]locationPrice
	LoadBalancerTypes     map[string][]locationPrice
	FloatingIPs           map[string][]locationPrice
	PrimaryIPs            map[string][]locationPrice
	VolumePricePerGBMonth float64
	ImagePricePerGBMonth  float64
}

type namedRef struct {
	Name string `json:"name"`
}

type locationRef struct {
	Name string `json:"name"`
}

type serverResource struct {
	ID           int64       `json:"id"`
	Name         string      `json:"name"`
	Created      time.Time   `json:"created"`
	ServerType   namedRef    `json:"server_type"`
	Location     locationRef `json:"location"`
	BackupWindow *string     `json:"backup_window"`
}

type volumeResource struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Created  time.Time   `json:"created"`
	Size     float64     `json:"size"`
	Location locationRef `json:"location"`
}

type floatingIPResource struct {
	ID           int64       `json:"id"`
	Name         string      `json:"name"`
	IP           string      `json:"ip"`
	Created      time.Time   `json:"created"`
	Type         string      `json:"type"`
	HomeLocation locationRef `json:"home_location"`
}

type primaryIPResource struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	IP       string      `json:"ip"`
	Created  time.Time   `json:"created"`
	Type     string      `json:"type"`
	Location locationRef `json:"location"`
}

type loadBalancerResource struct {
	ID               int64       `json:"id"`
	Name             string      `json:"name"`
	Created          time.Time   `json:"created"`
	LoadBalancerType namedRef    `json:"load_balancer_type"`
	Location         locationRef `json:"location"`
}

type imageResource struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Created     time.Time `json:"created"`
	Type        string    `json:"type"`
	ImageSize   float64   `json:"image_size"`
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

func (a *app) handle(ctx context.Context) error {
	tokensParameterName, err := requiredEnv("HETZNER_API_TOKENS_PARAMETER_NAME")
	if err != nil {
		return err
	}
	webhookParameterName, err := requiredEnv("DISCORD_WEBHOOK_PARAMETER_NAME")
	if err != nil {
		return err
	}

	tokensParameterValue, err := a.parameterString(ctx, tokensParameterName)
	if err != nil {
		return fmt.Errorf("read Hetzner API token parameter: %w", err)
	}
	projects, err := parseProjectTokens(tokensParameterValue)
	if err != nil {
		return fmt.Errorf("parse Hetzner API token parameter: %w", err)
	}

	webhookParameterValue, err := a.parameterString(ctx, webhookParameterName)
	if err != nil {
		return fmt.Errorf("read Discord webhook parameter: %w", err)
	}
	webhookURL, err := parseWebhookURL(webhookParameterValue)
	if err != nil {
		return fmt.Errorf("parse Discord webhook parameter: %w", err)
	}

	locationName := envOr("REPORT_TIMEZONE", "UTC")
	reportLocation, err := time.LoadLocation(locationName)
	if err != nil {
		return fmt.Errorf("load report timezone %q: %w", locationName, err)
	}
	period := previousMonth(a.now().In(reportLocation), reportLocation)

	baseURL := envOr("HETZNER_API_BASE_URL", defaultHetznerAPIBaseURL)
	reports := make([]projectReport, 0, len(projects))
	for _, project := range projects {
		report := a.collectProject(ctx, baseURL, project, period)
		reports = append(reports, report)
	}
	a.addJPYConversions(ctx, reports)

	payload := buildDiscordPayload(period, reports)
	if err := a.postDiscordWebhook(ctx, webhookURL, payload); err != nil {
		return fmt.Errorf("post Discord webhook: %w", err)
	}

	return nil
}

func (a *app) parameterString(ctx context.Context, parameterName string) (string, error) {
	withDecryption := true
	out, err := a.parameters.GetParameter(ctx, &ssm.GetParameterInput{Name: &parameterName, WithDecryption: &withDecryption})
	if err != nil {
		return "", err
	}
	if out.Parameter != nil && out.Parameter.Value != nil {
		return *out.Parameter.Value, nil
	}
	return "", errors.New("parameter has no value")
}

func (a *app) postDiscordWebhook(ctx context.Context, webhookURL string, payload discordWebhookPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Discord returned %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	return nil
}

func (a *app) collectProject(ctx context.Context, baseURL string, project projectToken, period billingPeriod) projectReport {
	report := projectReport{Name: project.Name}
	client := hcloudClient{
		baseURL:    baseURL,
		token:      project.Token,
		httpClient: a.httpClient,
	}

	var pricingResponse pricingEnvelope
	if err := client.getJSON(ctx, "pricing", nil, &pricingResponse); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("pricing: %v", err))
		return report
	}
	catalog, err := newPriceCatalog(pricingResponse.Pricing)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("pricing catalog: %v", err))
		return report
	}
	report.Currency = catalog.Currency

	servers, err := listAll[serverResource](ctx, client, "servers", "servers", nil)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("servers: %v", err))
	} else {
		addServerItems(&report, catalog, period, servers)
	}

	volumes, err := listAll[volumeResource](ctx, client, "volumes", "volumes", nil)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("volumes: %v", err))
	} else {
		addVolumeItems(&report, catalog, period, volumes)
	}

	floatingIPs, err := listAll[floatingIPResource](ctx, client, "floating_ips", "floating_ips", nil)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("floating_ips: %v", err))
	} else {
		addFloatingIPItems(&report, catalog, period, floatingIPs)
	}

	primaryIPs, err := listAll[primaryIPResource](ctx, client, "primary_ips", "primary_ips", nil)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("primary_ips: %v", err))
	} else {
		addPrimaryIPItems(&report, catalog, period, primaryIPs)
	}

	loadBalancers, err := listAll[loadBalancerResource](ctx, client, "load_balancers", "load_balancers", nil)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("load_balancers: %v", err))
	} else {
		addLoadBalancerItems(&report, catalog, period, loadBalancers)
	}

	imageQuery := url.Values{}
	imageQuery.Set("type", "snapshot")
	images, err := listAll[imageResource](ctx, client, "images", "images", imageQuery)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("images: %v", err))
	} else {
		addImageItems(&report, catalog, period, images)
	}

	for _, item := range report.Items {
		report.Total += item.Cost
	}

	return report
}

func (a *app) addJPYConversions(ctx context.Context, reports []projectReport) {
	conversionByCurrency := map[string]*currencyConversion{}
	errorByCurrency := map[string]error{}

	for index := range reports {
		currency := normalizeCurrencyCode(reports[index].Currency)
		if !looksLikeCurrencyCode(currency) || currency == jpyCurrency {
			continue
		}

		conversion, ok := conversionByCurrency[currency]
		if !ok {
			if cachedErr, hasError := errorByCurrency[currency]; hasError {
				reports[index].Warnings = append(reports[index].Warnings, fmt.Sprintf("JPY conversion unavailable: %v", cachedErr))
				continue
			}
			var err error
			conversion, err = a.exchangeRate(ctx, currency, jpyCurrency)
			if err != nil {
				errorByCurrency[currency] = err
				reports[index].Warnings = append(reports[index].Warnings, fmt.Sprintf("JPY conversion unavailable: %v", err))
				continue
			}
			conversionByCurrency[currency] = conversion
		}
		reports[index].JPYConversion = conversion
	}
}

func (a *app) exchangeRate(ctx context.Context, baseCurrency string, quoteCurrency string) (*currencyConversion, error) {
	baseCurrency = normalizeCurrencyCode(baseCurrency)
	quoteCurrency = normalizeCurrencyCode(quoteCurrency)
	if baseCurrency == "" || quoteCurrency == "" {
		return nil, errors.New("currency code is empty")
	}
	if baseCurrency == quoteCurrency {
		return nil, nil
	}
	if a.httpClient == nil {
		return nil, errors.New("HTTP client is not configured")
	}

	baseURL := strings.TrimRight(envOr("EXCHANGE_RATE_API_BASE_URL", defaultExchangeRateAPIBaseURL), "/")
	endpoint, err := url.Parse(baseURL + "/v2/rate/" + url.PathEscape(baseCurrency) + "/" + url.PathEscape(quoteCurrency))
	if err != nil {
		return nil, fmt.Errorf("parse exchange rate URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create exchange rate request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "aws-terraform-hetzner-billing-lambda")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get exchange rate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if readErr != nil {
			return nil, fmt.Errorf("exchange rate API returned %s and response body could not be read: %w", resp.Status, readErr)
		}
		return nil, fmt.Errorf("exchange rate API returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	var rateResponse frankfurterRateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rateResponse); err != nil {
		return nil, fmt.Errorf("decode exchange rate response: %w", err)
	}
	if rateResponse.Rate <= 0 {
		return nil, fmt.Errorf("exchange rate for %s/%s is %.4f", baseCurrency, quoteCurrency, rateResponse.Rate)
	}
	conversion := &currencyConversion{
		Base:  normalizeCurrencyCode(nonEmpty(rateResponse.Base, baseCurrency)),
		Quote: normalizeCurrencyCode(nonEmpty(rateResponse.Quote, quoteCurrency)),
		Rate:  rateResponse.Rate,
		Date:  strings.TrimSpace(rateResponse.Date),
	}
	return conversion, nil
}

func listAll[T any](ctx context.Context, client hcloudClient, apiPath, root string, query url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		pageQuery := cloneValues(query)
		pageQuery.Set("page", strconv.Itoa(page))
		pageQuery.Set("per_page", "50")

		var raw map[string]json.RawMessage
		if err := client.getJSON(ctx, apiPath, pageQuery, &raw); err != nil {
			return nil, err
		}

		var items []T
		if err := json.Unmarshal(raw[root], &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", root, err)
		}
		all = append(all, items...)

		var meta struct {
			Pagination struct {
				NextPage *int `json:"next_page"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(raw["meta"], &meta); err != nil {
			return nil, fmt.Errorf("decode pagination: %w", err)
		}
		if meta.Pagination.NextPage == nil {
			break
		}
	}

	return all, nil
}

func (c hcloudClient) getJSON(ctx context.Context, apiPath string, query url.Values, dest any) error {
	endpoint, err := url.Parse(strings.TrimRight(c.baseURL, "/") + "/" + strings.TrimLeft(apiPath, "/"))
	if err != nil {
		return err
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "aws-terraform-hetzner-billing-lambda")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s returned %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	return json.NewDecoder(resp.Body).Decode(dest)
}

func newPriceCatalog(source pricing) (priceCatalog, error) {
	volumePrice, err := source.Volume.PricePerGBMonth.value()
	if err != nil {
		return priceCatalog{}, fmt.Errorf("volume price: %w", err)
	}
	imagePrice, err := source.Image.PricePerGBMonth.value()
	if err != nil {
		return priceCatalog{}, fmt.Errorf("image price: %w", err)
	}
	backupPercent, err := parseOptionalFloat(source.ServerBackup.Percentage)
	if err != nil {
		return priceCatalog{}, fmt.Errorf("server backup percentage: %w", err)
	}

	catalog := priceCatalog{
		Currency:              source.Currency,
		ServerBackupPercent:   backupPercent,
		ServerTypes:           mapTypedPrices(source.ServerTypes),
		LoadBalancerTypes:     mapTypedPrices(source.LoadBalancerTypes),
		FloatingIPs:           mapIPPrices(source.FloatingIPs),
		PrimaryIPs:            mapIPPrices(source.PrimaryIPs),
		VolumePricePerGBMonth: volumePrice,
		ImagePricePerGBMonth:  imagePrice,
	}
	if catalog.Currency == "" {
		catalog.Currency = "UNKNOWN"
	}

	return catalog, nil
}

func addServerItems(report *projectReport, catalog priceCatalog, period billingPeriod, servers []serverResource) {
	for _, server := range servers {
		hours := activeHours(server.Created, period)
		if hours <= 0 {
			continue
		}
		location := server.Location.Name
		price, ok, err := catalog.recurring(catalog.ServerTypes, server.ServerType.Name, location)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("server %s: %v", resourceLabel(server.Name, server.ID), err))
			continue
		}
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("server %s: price not found for %s in %s", resourceLabel(server.Name, server.ID), server.ServerType.Name, location))
			continue
		}
		cost, ok := recurringCost(price, hours, periodHours(period))
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("server %s: usable price not found", resourceLabel(server.Name, server.ID)))
			continue
		}
		report.Items = append(report.Items, lineItem{
			Category: "server",
			Name:     resourceLabel(server.Name, server.ID),
			Location: location,
			Detail:   fmt.Sprintf("%s, %.1fh", server.ServerType.Name, hours),
			Cost:     cost,
		})

		if server.BackupWindow != nil && catalog.ServerBackupPercent > 0 {
			report.Items = append(report.Items, lineItem{
				Category: "backup",
				Name:     resourceLabel(server.Name, server.ID),
				Location: location,
				Detail:   fmt.Sprintf("%.2f%% of server cost", catalog.ServerBackupPercent),
				Cost:     cost * catalog.ServerBackupPercent / 100,
			})
		}
	}
}

func addVolumeItems(report *projectReport, catalog priceCatalog, period billingPeriod, volumes []volumeResource) {
	periodHours := periodHours(period)
	for _, volume := range volumes {
		hours := activeHours(volume.Created, period)
		if hours <= 0 {
			continue
		}
		cost := catalog.VolumePricePerGBMonth * volume.Size * hours / periodHours
		report.Items = append(report.Items, lineItem{
			Category: "volume",
			Name:     resourceLabel(volume.Name, volume.ID),
			Location: volume.Location.Name,
			Detail:   fmt.Sprintf("%.0f GB, %.1fh", volume.Size, hours),
			Cost:     cost,
		})
	}
}

func addFloatingIPItems(report *projectReport, catalog priceCatalog, period billingPeriod, floatingIPs []floatingIPResource) {
	for _, floatingIP := range floatingIPs {
		hours := activeHours(floatingIP.Created, period)
		if hours <= 0 {
			continue
		}
		location := floatingIP.HomeLocation.Name
		price, ok, err := catalog.recurring(catalog.FloatingIPs, floatingIP.Type, location)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("floating IP %s: %v", resourceLabel(ipName(floatingIP.Name, floatingIP.IP), floatingIP.ID), err))
			continue
		}
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("floating IP %s: price not found for %s in %s", resourceLabel(ipName(floatingIP.Name, floatingIP.IP), floatingIP.ID), floatingIP.Type, location))
			continue
		}
		cost, ok := recurringCost(price, hours, periodHours(period))
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("floating IP %s: usable price not found", resourceLabel(ipName(floatingIP.Name, floatingIP.IP), floatingIP.ID)))
			continue
		}
		report.Items = append(report.Items, lineItem{
			Category: "floating_ip",
			Name:     resourceLabel(ipName(floatingIP.Name, floatingIP.IP), floatingIP.ID),
			Location: location,
			Detail:   fmt.Sprintf("%s, %.1fh", floatingIP.Type, hours),
			Cost:     cost,
		})
	}
}

func addPrimaryIPItems(report *projectReport, catalog priceCatalog, period billingPeriod, primaryIPs []primaryIPResource) {
	for _, primaryIP := range primaryIPs {
		// Primary IPv6 addresses are free and carry no pricing entry.
		if strings.EqualFold(primaryIP.Type, "ipv6") {
			continue
		}
		hours := activeHours(primaryIP.Created, period)
		if hours <= 0 {
			continue
		}
		location := primaryIP.Location.Name
		price, ok, err := catalog.recurring(catalog.PrimaryIPs, primaryIP.Type, location)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("primary IP %s: %v", resourceLabel(ipName(primaryIP.Name, primaryIP.IP), primaryIP.ID), err))
			continue
		}
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("primary IP %s: price not found for %s in %s", resourceLabel(ipName(primaryIP.Name, primaryIP.IP), primaryIP.ID), primaryIP.Type, location))
			continue
		}
		cost, ok := recurringCost(price, hours, periodHours(period))
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("primary IP %s: usable price not found", resourceLabel(ipName(primaryIP.Name, primaryIP.IP), primaryIP.ID)))
			continue
		}
		report.Items = append(report.Items, lineItem{
			Category: "primary_ip",
			Name:     resourceLabel(ipName(primaryIP.Name, primaryIP.IP), primaryIP.ID),
			Location: location,
			Detail:   fmt.Sprintf("%s, %.1fh", primaryIP.Type, hours),
			Cost:     cost,
		})
	}
}

func addLoadBalancerItems(report *projectReport, catalog priceCatalog, period billingPeriod, loadBalancers []loadBalancerResource) {
	for _, loadBalancer := range loadBalancers {
		hours := activeHours(loadBalancer.Created, period)
		if hours <= 0 {
			continue
		}
		location := loadBalancer.Location.Name
		price, ok, err := catalog.recurring(catalog.LoadBalancerTypes, loadBalancer.LoadBalancerType.Name, location)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("load balancer %s: %v", resourceLabel(loadBalancer.Name, loadBalancer.ID), err))
			continue
		}
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("load balancer %s: price not found for %s in %s", resourceLabel(loadBalancer.Name, loadBalancer.ID), loadBalancer.LoadBalancerType.Name, location))
			continue
		}
		cost, ok := recurringCost(price, hours, periodHours(period))
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf("load balancer %s: usable price not found", resourceLabel(loadBalancer.Name, loadBalancer.ID)))
			continue
		}
		report.Items = append(report.Items, lineItem{
			Category: "load_balancer",
			Name:     resourceLabel(loadBalancer.Name, loadBalancer.ID),
			Location: location,
			Detail:   fmt.Sprintf("%s, %.1fh", loadBalancer.LoadBalancerType.Name, hours),
			Cost:     cost,
		})
	}
}

func addImageItems(report *projectReport, catalog priceCatalog, period billingPeriod, images []imageResource) {
	periodHours := periodHours(period)
	for _, image := range images {
		if image.Type != "snapshot" || image.ImageSize <= 0 {
			continue
		}
		hours := activeHours(image.Created, period)
		if hours <= 0 {
			continue
		}
		cost := catalog.ImagePricePerGBMonth * image.ImageSize * hours / periodHours
		report.Items = append(report.Items, lineItem{
			Category: "snapshot",
			Name:     resourceLabel(imageName(image.Name, image.Description), image.ID),
			Detail:   fmt.Sprintf("%.2f GB, %.1fh", image.ImageSize, hours),
			Cost:     cost,
		})
	}
}

func (c priceCatalog) recurring(source map[string][]locationPrice, name, location string) (recurringPrice, bool, error) {
	prices, ok := source[name]
	if !ok {
		return recurringPrice{}, false, nil
	}
	price, ok := priceForLocation(prices, location)
	if !ok {
		return recurringPrice{}, false, nil
	}
	return price.toRecurring()
}

func (p locationPrice) toRecurring() (recurringPrice, bool, error) {
	var result recurringPrice
	if p.PriceHourly != nil {
		value, err := p.PriceHourly.value()
		if err != nil {
			return recurringPrice{}, false, fmt.Errorf("hourly price: %w", err)
		}
		result.Hourly = value
		result.HasHourly = true
	}
	if p.PriceMonthly != nil {
		value, err := p.PriceMonthly.value()
		if err != nil {
			return recurringPrice{}, false, fmt.Errorf("monthly price: %w", err)
		}
		result.Monthly = value
		result.HasMonthly = true
	}
	return result, result.HasHourly || result.HasMonthly, nil
}

func recurringCost(price recurringPrice, hours, fullPeriodHours float64) (float64, bool) {
	if hours <= 0 || fullPeriodHours <= 0 {
		return 0, false
	}
	if price.HasHourly && price.HasMonthly {
		return math.Min(price.Hourly*hours, price.Monthly), true
	}
	if price.HasHourly {
		return price.Hourly * hours, true
	}
	if price.HasMonthly {
		return price.Monthly * hours / fullPeriodHours, true
	}
	return 0, false
}

func (a amount) value() (float64, error) {
	value := strings.TrimSpace(a.Gross)
	if value == "" {
		value = strings.TrimSpace(a.Net)
	}
	if value == "" {
		return 0, errors.New("missing net and gross values")
	}
	return strconv.ParseFloat(value, 64)
}

func mapTypedPrices(items []typedPrices) map[string][]locationPrice {
	result := make(map[string][]locationPrice, len(items))
	for _, item := range items {
		result[item.Name] = item.Prices
	}
	return result
}

func mapIPPrices(items []ipPrices) map[string][]locationPrice {
	result := make(map[string][]locationPrice, len(items))
	for _, item := range items {
		result[item.Type] = item.Prices
	}
	return result
}

func priceForLocation(prices []locationPrice, location string) (locationPrice, bool) {
	for _, price := range prices {
		if price.Location == location {
			return price, true
		}
	}
	// Without a location the first entry is only safe to use when every location
	// charges the same. Picking an arbitrary entry would silently report the
	// price of another location, which is how the removal of the `datacenter`
	// response field went unnoticed.
	if location == "" && len(prices) > 0 && sameEverywhere(prices) {
		return prices[0], true
	}
	return locationPrice{}, false
}

func sameEverywhere(prices []locationPrice) bool {
	for _, price := range prices[1:] {
		if !samePrice(price.PriceHourly, prices[0].PriceHourly) || !samePrice(price.PriceMonthly, prices[0].PriceMonthly) {
			return false
		}
	}
	return true
}

func samePrice(left, right *amount) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftValue, leftErr := left.value()
	rightValue, rightErr := right.value()
	if leftErr != nil || rightErr != nil {
		return false
	}
	return leftValue == rightValue
}

func previousMonth(now time.Time, location *time.Location) billingPeriod {
	currentMonthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	previousMonthStart := currentMonthStart.AddDate(0, -1, 0)
	return billingPeriod{
		Start:        previousMonthStart,
		End:          currentMonthStart,
		LocationName: location.String(),
	}
}

func activeHours(created time.Time, period billingPeriod) float64 {
	if created.IsZero() || created.Before(period.Start) {
		created = period.Start
	}
	if !created.Before(period.End) {
		return 0
	}
	return period.End.Sub(created).Hours()
}

func periodHours(period billingPeriod) float64 {
	return period.End.Sub(period.Start).Hours()
}

func parseProjectTokens(raw string) ([]projectToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("secret is empty")
	}

	var structured struct {
		Projects []projectToken    `json:"projects"`
		Tokens   map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &structured); err == nil && (len(structured.Projects) > 0 || len(structured.Tokens) > 0) {
		projects := append([]projectToken{}, structured.Projects...)
		for name, token := range structured.Tokens {
			projects = append(projects, projectToken{Name: name, Token: token})
		}
		return normalizeProjectTokens(projects)
	}

	var tokenMap map[string]string
	if err := json.Unmarshal([]byte(raw), &tokenMap); err == nil && len(tokenMap) > 0 {
		projects := make([]projectToken, 0, len(tokenMap))
		for name, token := range tokenMap {
			projects = append(projects, projectToken{Name: name, Token: token})
		}
		return normalizeProjectTokens(projects)
	}

	var tokenArray []projectToken
	if err := json.Unmarshal([]byte(raw), &tokenArray); err == nil && len(tokenArray) > 0 {
		return normalizeProjectTokens(tokenArray)
	}

	var tokenString string
	if err := json.Unmarshal([]byte(raw), &tokenString); err == nil && strings.TrimSpace(tokenString) != "" {
		return normalizeProjectTokens([]projectToken{{Name: "default", Token: tokenString}})
	}

	return normalizeProjectTokens([]projectToken{{Name: "default", Token: raw}})
}

func normalizeProjectTokens(projects []projectToken) ([]projectToken, error) {
	for i := range projects {
		projects[i].Name = strings.TrimSpace(projects[i].Name)
		projects[i].Token = strings.TrimSpace(projects[i].Token)
		if projects[i].Name == "" {
			projects[i].Name = fmt.Sprintf("project-%d", i+1)
		}
		if projects[i].Token == "" {
			return nil, fmt.Errorf("%s token is empty", projects[i].Name)
		}
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})
	return projects, nil
}

func parseWebhookURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("secret is empty")
	}

	var value string
	if err := json.Unmarshal([]byte(raw), &value); err == nil && strings.TrimSpace(value) != "" {
		return validateWebhookURL(strings.TrimSpace(value))
	}

	var object map[string]string
	if err := json.Unmarshal([]byte(raw), &object); err == nil {
		for _, key := range []string{"url", "webhook_url", "discord_webhook_url"} {
			if value := strings.TrimSpace(object[key]); value != "" {
				return validateWebhookURL(value)
			}
		}
	}

	return validateWebhookURL(raw)
}

func validateWebhookURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return "", errors.New("webhook URL must be an absolute https URL")
	}
	return raw, nil
}

func buildDiscordPayload(period billingPeriod, reports []projectReport) discordWebhookPayload {
	periodText := fmt.Sprintf("%s to %s (%s)", period.Start.Format("2006-01-02"), period.End.AddDate(0, 0, -1).Format("2006-01-02"), period.LocationName)
	description := fmt.Sprintf("Period: %s\nTotal: %s\nScope: active resources visible through Cloud API; deleted resources are not included.", periodText, formatTotals(reports))
	embed := discordEmbed{
		Title:       truncateDiscord("Hetzner Cloud billing estimate", discordEmbedTitleLimit),
		Description: truncateDiscord(description, discordEmbedDescriptionLimit),
		Color:       discordHetznerColor,
	}
	usedCharacters := embedCharCount(embed)
	omittedEntries := 0

	for _, report := range reports {
		if len(embed.Fields) >= discordEmbedFieldLimit {
			omittedEntries += reportEntryCount(report)
			continue
		}

		currency := displayCurrency(report.Currency)
		field := discordEmbedField{
			Name:  truncateDiscord(fmt.Sprintf("%s - %s", report.Name, formatMoneyWithJPY(report.Total, currency, report.JPYConversion)), discordEmbedFieldNameLimit),
			Value: "See CloudWatch Logs for details.",
		}
		value, omitted := buildProjectFieldValue(report, currency)
		field.Value = value
		omittedEntries += omitted

		fieldCharacters := charLen(field.Name) + charLen(field.Value)
		if usedCharacters+fieldCharacters > discordEmbedTotalLimit {
			omittedEntries += reportEntryCount(report) - omitted
			continue
		}
		embed.Fields = append(embed.Fields, field)
		usedCharacters += fieldCharacters
	}

	if omittedEntries > 0 {
		footerText := fmt.Sprintf("%d entries omitted. See CloudWatch Logs for full run context.", omittedEntries)
		remainingCharacters := discordEmbedTotalLimit - usedCharacters
		if remainingCharacters > 0 {
			footerLimit := discordEmbedFooterTextLimit
			if remainingCharacters < footerLimit {
				footerLimit = remainingCharacters
			}
			footerText = truncateDiscord(footerText, footerLimit)
			if footerText != "" {
				embed.Footer = &discordEmbedFooter{Text: footerText}
			}
		}
	}

	return discordWebhookPayload{
		Content:         truncateDiscord(fmt.Sprintf("Hetzner Cloud billing estimate: %s", formatTotals(reports)), discordContentLimit),
		Username:        "Hetzner Billing",
		Embeds:          []discordEmbed{embed},
		AllowedMentions: discordAllowedMentions{Parse: []string{}},
	}
}

func buildProjectFieldValue(report projectReport, currency string) (string, int) {
	var builder strings.Builder
	omittedEntries := 0

	for _, errMessage := range report.Errors {
		if !writeEmbedFieldLine(&builder, "Error: "+errMessage, discordEmbedFieldValueLimit) {
			omittedEntries++
		}
	}
	for _, warning := range report.Warnings {
		if !writeEmbedFieldLine(&builder, "Warning: "+warning, discordEmbedFieldValueLimit) {
			omittedEntries++
		}
	}

	items := billableLineItems(report.Items)
	sort.Slice(items, func(i, j int) bool {
		if items[i].Cost == items[j].Cost {
			return items[i].Name < items[j].Name
		}
		return items[i].Cost > items[j].Cost
	})
	if len(items) == 0 && len(report.Errors) == 0 {
		if !writeEmbedFieldLine(&builder, "No billable active resources found.", discordEmbedFieldValueLimit) {
			omittedEntries++
		}
	}
	for _, item := range items {
		line := fmt.Sprintf("%s %s: %.2f %s", item.Category, item.Name, item.Cost, currency)
		if item.Detail != "" {
			line += fmt.Sprintf(" (%s)", item.Detail)
		}
		if !writeEmbedFieldLine(&builder, line, discordEmbedFieldValueLimit) {
			omittedEntries++
		}
	}

	value := strings.TrimRight(builder.String(), "\n")
	if value == "" {
		value = "See CloudWatch Logs for details."
	}
	return value, omittedEntries
}

func billableLineItems(items []lineItem) []lineItem {
	billableItems := make([]lineItem, 0, len(items))
	for _, item := range items {
		if !hasBillableCost(item.Cost) {
			continue
		}
		billableItems = append(billableItems, item)
	}
	return billableItems
}

func hasBillableCost(cost float64) bool {
	return math.Abs(cost) > 0
}

func writeEmbedFieldLine(builder *strings.Builder, line string, limit int) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		line = "-"
	}
	line += "\n"
	remainingCharacters := limit - charLen(builder.String())
	if remainingCharacters <= 0 {
		return false
	}
	if charLen(line) <= remainingCharacters {
		builder.WriteString(line)
		return true
	}
	if builder.Len() == 0 {
		builder.WriteString(truncateDiscord(line, remainingCharacters))
		return true
	}
	return false
}

func reportEntryCount(report projectReport) int {
	count := len(report.Errors) + len(report.Warnings) + len(report.Items)
	if count == 0 {
		return 1
	}
	return count
}

func embedCharCount(embed discordEmbed) int {
	count := charLen(embed.Title) + charLen(embed.Description)
	for _, field := range embed.Fields {
		count += charLen(field.Name) + charLen(field.Value)
	}
	if embed.Footer != nil {
		count += charLen(embed.Footer.Text)
	}
	return count
}

func truncateDiscord(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return ""
	}
	if charLen(value) <= limit {
		return value
	}
	suffixLength := charLen(truncationSuffix)
	if limit <= suffixLength {
		return string([]rune(truncationSuffix)[:limit])
	}
	runes := []rune(value)
	return string(runes[:limit-suffixLength]) + truncationSuffix
}

func charLen(value string) int {
	return utf8.RuneCountInString(value)
}

func formatTotals(reports []projectReport) string {
	totals := map[string]float64{}
	conversions := map[string]*currencyConversion{}
	for _, report := range reports {
		currency := displayCurrency(report.Currency)
		totals[currency] += report.Total
		if report.JPYConversion != nil {
			conversions[currency] = report.JPYConversion
		}
	}

	currencies := make([]string, 0, len(totals))
	for currency := range totals {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)

	parts := make([]string, 0, len(currencies))
	for _, currency := range currencies {
		parts = append(parts, formatMoneyWithJPY(totals[currency], currency, conversions[currency]))
	}
	if len(parts) == 0 {
		return formatMoney(0, "UNKNOWN")
	}
	return strings.Join(parts, ", ")
}

func formatMoneyWithJPY(amount float64, currency string, conversion *currencyConversion) string {
	currency = displayCurrency(currency)
	formatted := formatMoney(amount, currency)
	if conversion == nil || conversion.Rate <= 0 || !strings.EqualFold(currency, conversion.Base) {
		return formatted
	}
	return fmt.Sprintf("%s (%s)", formatted, formatJPY(amount*conversion.Rate))
}

func formatMoney(amount float64, currency string) string {
	currency = displayCurrency(currency)
	return fmt.Sprintf("%.2f %s", amount, currency)
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

func displayCurrency(currency string) string {
	currency = strings.TrimSpace(currency)
	if currency == "" {
		return "UNKNOWN"
	}
	return currency
}

func nonEmpty(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func normalizeCurrencyCode(currency string) string {
	return strings.ToUpper(strings.TrimSpace(currency))
}

func looksLikeCurrencyCode(currency string) bool {
	currency = normalizeCurrencyCode(currency)
	if len(currency) != 3 {
		return false
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func parseOptionalFloat(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseFloat(raw, 64)
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func cloneValues(values url.Values) url.Values {
	clone := url.Values{}
	for key, value := range values {
		clone[key] = append([]string{}, value...)
	}
	return clone
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func resourceLabel(name string, id int64) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Sprintf("#%d", id)
	}
	return fmt.Sprintf("%s (#%d)", name, id)
}

func ipName(name, ip string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(ip)
}

func imageName(name, description string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	return strings.TrimSpace(description)
}
