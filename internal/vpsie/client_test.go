package vpsie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchCategories(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/plans/category" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Vpsie-Auth") != "test-key" {
			t.Fatalf("missing or wrong Vpsie-Auth header: %s", r.Header.Get("Vpsie-Auth"))
		}
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}

		resp := categoriesResponse{
			Data: []PlanCategory{
				{Identifier: "cat-1", Name: "Shared CPU"},
				{Identifier: "cat-2", Name: "Compute Optimized"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
		apiKey:     "test-key",
	}

	cats, err := c.FetchCategories(context.Background())
	if err != nil {
		t.Fatalf("FetchCategories failed: %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(cats))
	}
	if cats[0].Name != "Shared CPU" {
		t.Fatalf("expected 'Shared CPU', got %q", cats[0].Name)
	}
	if cats[1].Identifier != "cat-2" {
		t.Fatalf("expected 'cat-2', got %q", cats[1].Identifier)
	}
}

func TestFetchPlans(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/v2/resources" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Fatalf("expected form-urlencoded content type, got %q", ct)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatalf("failed to parse form body: %v", err)
		}
		if r.PostForm.Get("dcIdentifier") != "dc-1" {
			t.Fatalf("expected dcIdentifier=dc-1, got %q", r.PostForm.Get("dcIdentifier"))
		}
		if r.PostForm.Get("osIdentifier") != "os-1" {
			t.Fatalf("expected osIdentifier=os-1, got %q", r.PostForm.Get("osIdentifier"))
		}
		if r.PostForm.Get("planCatIdentifier") != "cat-1" {
			t.Fatalf("expected planCatIdentifier=cat-1, got %q", r.PostForm.Get("planCatIdentifier"))
		}

		resp := plansResponse{
			Data: []planData{
				{Identifier: "plan-a", Nickname: "s-2vcpu-4gb", CPU: 2, RAM: 4096, SSD: 80, Traffic: 4096, Price: 24.0},
				{Identifier: "plan-b", Nickname: "s-4vcpu-8gb", CPU: 4, RAM: 8192, SSD: 160, Traffic: 5120, Price: 48.0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	c := &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
		apiKey:     "test-key",
	}

	plans, err := c.FetchPlans(context.Background(), "dc-1", "os-1", "cat-1")
	if err != nil {
		t.Fatalf("FetchPlans failed: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(plans))
	}
	if plans[0].Nickname != "s-2vcpu-4gb" {
		t.Fatalf("expected 's-2vcpu-4gb', got %q", plans[0].Nickname)
	}
	if plans[0].CPU != 2 || plans[0].RAM != 4096 {
		t.Fatalf("unexpected plan specs: cpu=%d ram=%d", plans[0].CPU, plans[0].RAM)
	}
	if plans[0].PriceMonthly != 24.0 {
		t.Fatalf("expected price 24.0, got %f", plans[0].PriceMonthly)
	}
	if plans[0].CategoryID != "cat-1" {
		t.Fatalf("expected categoryID 'cat-1', got %q", plans[0].CategoryID)
	}
}

func TestFetchCategories_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("unauthorized"))
	}))
	defer server.Close()

	c := &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
		apiKey:     "bad-key",
	}

	_, err := c.FetchCategories(context.Background())
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestNewClient_EmptyKey(t *testing.T) {
	_, err := NewClient("")
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
}
