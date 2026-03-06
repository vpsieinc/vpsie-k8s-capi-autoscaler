package pricing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

// Cache maintains an in-memory cache of VPSie plan categories and plans.
// It periodically refreshes data from the VPSie API.
type Cache struct {
	client          vpsie.PricingClient
	dcID            string
	osID            string
	refreshInterval time.Duration

	mu              sync.RWMutex
	categories      []vpsie.PlanCategory
	plans           []vpsie.Plan      // all plans across categories
	categoryNameMap map[string]string // name (lower) → identifier
	lastRefresh     time.Time
}

// NewCache creates a new pricing cache.
func NewCache(client vpsie.PricingClient, dcID, osID string, refreshInterval time.Duration) *Cache {
	if refreshInterval <= 0 {
		refreshInterval = 5 * time.Minute
	}
	return &Cache{
		client:          client,
		dcID:            dcID,
		osID:            osID,
		refreshInterval: refreshInterval,
		categoryNameMap: make(map[string]string),
	}
}

// EnsureFresh refreshes the cache if data is stale.
func (c *Cache) EnsureFresh(ctx context.Context) error {
	c.mu.RLock()
	fresh := time.Since(c.lastRefresh) < c.refreshInterval
	c.mu.RUnlock()

	if fresh {
		return nil
	}
	return c.Refresh(ctx)
}

// Refresh fetches categories and plans from the API.
func (c *Cache) Refresh(ctx context.Context) error {
	start := time.Now()

	cats, err := c.client.FetchCategories(ctx)
	if err != nil {
		return fmt.Errorf("fetching categories: %w", err)
	}

	var allPlans []vpsie.Plan
	nameMap := make(map[string]string, len(cats))

	for _, cat := range cats {
		nameMap[strings.ToLower(cat.Name)] = cat.Identifier

		plans, err := c.client.FetchPlans(ctx, c.dcID, c.osID, cat.Identifier)
		if err != nil {
			klog.V(2).Infof("failed to fetch plans for category %s (%s): %v", cat.Name, cat.Identifier, err)
			continue
		}
		// Enrich plans with category name
		for i := range plans {
			plans[i].CategoryName = cat.Name
		}
		allPlans = append(allPlans, plans...)
	}

	c.mu.Lock()
	c.categories = cats
	c.plans = allPlans
	c.categoryNameMap = nameMap
	c.lastRefresh = time.Now()
	c.mu.Unlock()

	klog.V(2).Infof("pricing cache refreshed: %d categories, %d plans in %v", len(cats), len(allPlans), time.Since(start))
	return nil
}

// Categories returns cached category names.
func (c *Cache) Categories() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.categories))
	for _, cat := range c.categories {
		names = append(names, cat.Name)
	}
	return names
}

// ResolveCategoryID returns the identifier for a category name (case-insensitive).
func (c *Cache) ResolveCategoryID(name string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	id, ok := c.categoryNameMap[strings.ToLower(name)]
	return id, ok
}

// Plans returns all cached plans, optionally filtered by category names.
// If allowedCategories is empty, all plans are returned.
func (c *Cache) Plans(allowedCategories []string) []vpsie.Plan {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(allowedCategories) == 0 {
		result := make([]vpsie.Plan, len(c.plans))
		copy(result, c.plans)
		return result
	}

	allowed := make(map[string]bool, len(allowedCategories))
	for _, name := range allowedCategories {
		if id, ok := c.categoryNameMap[strings.ToLower(name)]; ok {
			allowed[id] = true
		}
	}

	var result []vpsie.Plan
	for _, p := range c.plans {
		if allowed[p.CategoryID] {
			result = append(result, p)
		}
	}
	return result
}

// FindPlanByID finds a plan by its identifier.
func (c *Cache) FindPlanByID(identifier string) (vpsie.Plan, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, p := range c.plans {
		if p.Identifier == identifier {
			return p, true
		}
	}
	return vpsie.Plan{}, false
}

// LastRefreshTime returns when the cache was last refreshed.
func (c *Cache) LastRefreshTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefresh
}
