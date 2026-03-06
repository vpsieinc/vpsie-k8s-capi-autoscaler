package vpsie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"k8s.io/klog/v2"
)

const (
	defaultBaseURL = "https://api.vpsie.com"
	apiURLEnv      = "VPSIE_API_URL"
	userAgent      = "vpsie-cluster-scaler/0.1.0"
)

// Client implements PricingClient using the VPSie HTTP API.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
}

// NewClient creates a PricingClient for the given API key.
func NewClient(apiKey string) (PricingClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("apiKey is empty")
	}

	baseURL := defaultBaseURL
	if u := os.Getenv(apiURLEnv); u != "" {
		baseURL = u
		klog.V(2).Infof("using custom VPSie API URL: %s", baseURL)
	}

	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    baseURL,
		apiKey:     apiKey,
	}, nil
}

// FetchCategories calls GET /api/v2/plans/category to list plan categories.
func (c *Client) FetchCategories(ctx context.Context) ([]PlanCategory, error) {
	url := c.baseURL + "/api/v2/plans/category"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating categories request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching categories: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("categories API returned %d: %s", resp.StatusCode, string(body))
	}

	var result categoriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding categories response: %w", err)
	}

	klog.V(4).Infof("fetched %d plan categories", len(result.Data))
	return result.Data, nil
}

// FetchPlans calls POST /api/v2/resources to list VM plans for a given dc+os+category.
func (c *Client) FetchPlans(ctx context.Context, dcID, osID, planCatID string) ([]Plan, error) {
	url := c.baseURL + "/api/v2/resources"

	payload := map[string]string{
		"dcIdentifier":       dcID,
		"osIdentifier":       osID,
		"planCategoryNodeId": planCatID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling plans request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating plans request: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching plans: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("plans API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result plansResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding plans response: %w", err)
	}

	plans := make([]Plan, 0, len(result.Data))
	for _, p := range result.Data {
		plans = append(plans, Plan{
			Identifier:   p.Identifier,
			Nickname:     p.Nickname,
			CPU:          p.CPU,
			RAM:          p.RAM,
			SSD:          p.SSD,
			Traffic:      p.Traffic,
			PriceMonthly: p.Price,
			CategoryID:   planCatID,
		})
	}

	klog.V(4).Infof("fetched %d plans for dc=%s os=%s cat=%s", len(plans), dcID, osID, planCatID)
	return plans, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Vpsie-Auth", c.apiKey)
	req.Header.Set("User-Agent", userAgent)
}
