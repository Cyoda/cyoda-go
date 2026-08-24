package app

import (
	"os"
	"testing"
)

// TestDefaultConfig_SearchAsyncMaxPerTenant asserts the
// CYODA_SEARCH_ASYNC_MAX_PER_TENANT default is derived from the worker count
// (so it tracks a resized pool instead of drifting from it), and that the
// env var actually binds.
func TestDefaultConfig_SearchAsyncMaxPerTenant(t *testing.T) {
	t.Setenv("CYODA_SEARCH_ASYNC_WORKERS", "")
	os.Unsetenv("CYODA_SEARCH_ASYNC_WORKERS")
	t.Setenv("CYODA_SEARCH_ASYNC_MAX_PER_TENANT", "")
	os.Unsetenv("CYODA_SEARCH_ASYNC_MAX_PER_TENANT")

	cfg := DefaultConfig()
	if cfg.SearchAsync.MaxPerTenant != cfg.SearchAsync.Workers {
		t.Errorf("default SearchAsync.MaxPerTenant = %d, want %d (the worker count)",
			cfg.SearchAsync.MaxPerTenant, cfg.SearchAsync.Workers)
	}

	t.Setenv("CYODA_SEARCH_ASYNC_WORKERS", "4")
	if got := DefaultConfig().SearchAsync.MaxPerTenant; got != 4 {
		t.Errorf("MaxPerTenant with 4 workers = %d, want 4 (the default must track Workers)", got)
	}

	t.Setenv("CYODA_SEARCH_ASYNC_MAX_PER_TENANT", "2")
	if got := DefaultConfig().SearchAsync.MaxPerTenant; got != 2 {
		t.Errorf("MaxPerTenant override = %d, want 2", got)
	}

	// 0 is the documented "no per-tenant cap" value and must survive
	// DefaultConfig unchanged rather than being re-defaulted.
	t.Setenv("CYODA_SEARCH_ASYNC_MAX_PER_TENANT", "0")
	if got := DefaultConfig().SearchAsync.MaxPerTenant; got != 0 {
		t.Errorf("MaxPerTenant=0 = %d, want 0 (explicitly disables the cap)", got)
	}
}

// TestSearchAsyncConfig_ValidateRejectsNegativeMaxPerTenant asserts a
// negative cap is a hard startup error. Zero is legal — it disables the cap
// — so only a value that is neither a cap nor the disable sentinel is
// rejected.
func TestSearchAsyncConfig_ValidateRejectsNegativeMaxPerTenant(t *testing.T) {
	if err := ValidateSearchAsync(SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: -1}); err == nil {
		t.Error("ValidateSearchAsync(MaxPerTenant=-1) = nil, want an error")
	}
	if err := ValidateSearchAsync(SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: 0}); err != nil {
		t.Errorf("ValidateSearchAsync(MaxPerTenant=0) = %v, want nil (0 disables the cap)", err)
	}
	if err := ValidateSearchAsync(SearchAsyncConfig{Workers: 8, QueueLen: 256, MaxPerTenant: 4}); err != nil {
		t.Errorf("ValidateSearchAsync(MaxPerTenant=4) = %v, want nil", err)
	}
}
