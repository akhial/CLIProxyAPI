package main

import "testing"

func TestSortBackendsByPenaltyDeprioritizesFailures(t *testing.T) {
	resetBackendPenaltiesForTest()
	t.Cleanup(resetBackendPenaltiesForTest)
	recordBackendFailure(backendTavily)
	recordBackendFailure(backendTavily)
	ordered := sortBackendsByPenalty([]routeBackend{
		backendTavily,
		backendCodexWebSearch,
		backendXAIWebSearch,
	})
	if ordered[0] != backendCodexWebSearch {
		t.Fatalf("ordered = %v, want codex first after Tavily penalty", ordered)
	}
}
