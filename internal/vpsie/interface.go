package vpsie

import "context"

// PricingClient defines the interface for fetching VPSie pricing data.
// Controllers use this interface for dependency injection (testing with fakes).
type PricingClient interface {
	// FetchCategories returns all available plan categories.
	FetchCategories(ctx context.Context) ([]PlanCategory, error)

	// FetchPlans returns VM plans for the given datacenter, OS image, and plan category.
	FetchPlans(ctx context.Context, dcID, osID, planCatID string) ([]Plan, error)
}
