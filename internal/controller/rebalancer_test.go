package controller

import (
	"testing"
	"time"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
)

func TestCooldownForAggressiveness(t *testing.T) {
	rb := &Rebalancer{}

	tests := []struct {
		agg      optv1.Aggressiveness
		expected time.Duration
	}{
		{optv1.AggressivenessConservative, 30 * time.Minute},
		{optv1.AggressivenessModerate, 15 * time.Minute},
		{optv1.AggressivenessAggressive, 5 * time.Minute},
	}

	for _, tt := range tests {
		got := rb.cooldownForAggressiveness(tt.agg)
		if got != tt.expected {
			t.Errorf("cooldown for %s: got %v, want %v", tt.agg, got, tt.expected)
		}
	}
}

func TestReplicasMatch(t *testing.T) {
	one := int32(1)
	two := int32(2)
	oneAgain := int32(1)

	tests := []struct {
		a, b     *int32
		expected bool
	}{
		{nil, nil, true},
		{nil, &one, false},
		{&one, nil, false},
		{&one, &oneAgain, true},
		{&one, &two, false},
	}

	for _, tt := range tests {
		got := replicasMatch(tt.a, tt.b)
		if got != tt.expected {
			t.Errorf("replicasMatch(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expected)
		}
	}
}

func TestRebalancer_NeedLeaderElection(t *testing.T) {
	rb := &Rebalancer{}
	if !rb.NeedLeaderElection() {
		t.Fatal("expected NeedLeaderElection to return true")
	}
}
