package pricing

import (
	"context"
	"testing"
	"time"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

type fakePricingClient struct {
	categories []vpsie.PlanCategory
	plans      map[string][]vpsie.Plan // keyed by categoryID
	fetchErr   error
}

func (f *fakePricingClient) FetchCategories(_ context.Context) ([]vpsie.PlanCategory, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.categories, nil
}

func (f *fakePricingClient) FetchPlans(_ context.Context, _, _ string, catID string) ([]vpsie.Plan, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.plans[catID], nil
}

func newFakePricingClient() *fakePricingClient {
	return &fakePricingClient{
		categories: []vpsie.PlanCategory{
			{Identifier: "cat-shared", Name: "Shared CPU"},
			{Identifier: "cat-compute", Name: "Compute Optimized"},
		},
		plans: map[string][]vpsie.Plan{
			"cat-shared": {
				{Identifier: "plan-1", Nickname: "s-2vcpu-4gb", CPU: 2, RAM: 4096, SSD: 80, PriceMonthly: 24.0, CategoryID: "cat-shared"},
				{Identifier: "plan-2", Nickname: "s-4vcpu-8gb", CPU: 4, RAM: 8192, SSD: 160, PriceMonthly: 48.0, CategoryID: "cat-shared"},
			},
			"cat-compute": {
				{Identifier: "plan-3", Nickname: "c-2vcpu-4gb", CPU: 2, RAM: 4096, SSD: 80, PriceMonthly: 32.0, CategoryID: "cat-compute"},
			},
		},
	}
}

func TestCache_Refresh(t *testing.T) {
	fpc := newFakePricingClient()
	cache := NewCache(fpc, "dc-1", "os-1", 5*time.Minute)

	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	cats := cache.Categories()
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(cats))
	}

	plans := cache.Plans(nil)
	if len(plans) != 3 {
		t.Fatalf("expected 3 plans, got %d", len(plans))
	}
}

func TestCache_FilterByCategory(t *testing.T) {
	fpc := newFakePricingClient()
	cache := NewCache(fpc, "dc-1", "os-1", 5*time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	plans := cache.Plans([]string{"Shared CPU"})
	if len(plans) != 2 {
		t.Fatalf("expected 2 Shared CPU plans, got %d", len(plans))
	}

	plans = cache.Plans([]string{"Compute Optimized"})
	if len(plans) != 1 {
		t.Fatalf("expected 1 Compute Optimized plan, got %d", len(plans))
	}
}

func TestCache_ResolveCategoryID(t *testing.T) {
	fpc := newFakePricingClient()
	cache := NewCache(fpc, "dc-1", "os-1", 5*time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	id, ok := cache.ResolveCategoryID("Shared CPU")
	if !ok {
		t.Fatal("expected to find 'Shared CPU'")
	}
	if id != "cat-shared" {
		t.Fatalf("expected 'cat-shared', got %q", id)
	}

	// Case-insensitive
	id, ok = cache.ResolveCategoryID("shared cpu")
	if !ok {
		t.Fatal("expected case-insensitive match")
	}
	if id != "cat-shared" {
		t.Fatalf("expected 'cat-shared', got %q", id)
	}

	_, ok = cache.ResolveCategoryID("NonExistent")
	if ok {
		t.Fatal("expected not to find 'NonExistent'")
	}
}

func TestCache_FindPlanByID(t *testing.T) {
	fpc := newFakePricingClient()
	cache := NewCache(fpc, "dc-1", "os-1", 5*time.Minute)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	plan, ok := cache.FindPlanByID("plan-2")
	if !ok {
		t.Fatal("expected to find plan-2")
	}
	if plan.Nickname != "s-4vcpu-8gb" {
		t.Fatalf("expected 's-4vcpu-8gb', got %q", plan.Nickname)
	}

	_, ok = cache.FindPlanByID("nonexistent")
	if ok {
		t.Fatal("expected not to find nonexistent plan")
	}
}

func TestCache_EnsureFresh(t *testing.T) {
	fpc := newFakePricingClient()
	cache := NewCache(fpc, "dc-1", "os-1", 1*time.Hour)

	// First call should refresh
	if err := cache.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("EnsureFresh failed: %v", err)
	}
	if len(cache.Plans(nil)) != 3 {
		t.Fatalf("expected 3 plans after first refresh")
	}

	// Second call should use cache (still fresh)
	fpc.plans = nil // Clear plans to prove it doesn't re-fetch
	if err := cache.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("EnsureFresh failed on cached data: %v", err)
	}
	if len(cache.Plans(nil)) != 3 {
		t.Fatalf("expected 3 cached plans, got %d", len(cache.Plans(nil)))
	}
}
