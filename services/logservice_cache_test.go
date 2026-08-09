package services

import (
	"math"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func TestCacheHitRate(t *testing.T) {
	tests := []struct {
		name         string
		inputTokens  int64
		cachedTokens int64
		want         *float64
	}{
		{name: "no input usage", inputTokens: 0, cachedTokens: 0, want: nil},
		{name: "zero hits", inputTokens: 100, cachedTokens: 0, want: float64Ptr(0)},
		{name: "weighted hits", inputTokens: 900, cachedTokens: 100, want: float64Ptr(0.1)},
		{name: "all input cached", inputTokens: 0, cachedTokens: 100, want: float64Ptr(1)},
		{name: "negative uncached input is invalid", inputTokens: -1, cachedTokens: 1, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cacheHitRate(tt.inputTokens, tt.cachedTokens)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("cacheHitRate() = %v, want nil", *got)
				}
				return
			}
			if got == nil || math.Abs(*got-*tt.want) > 1e-12 {
				if got == nil {
					t.Fatalf("cacheHitRate() = nil, want %v", *tt.want)
				}
				t.Fatalf("cacheHitRate() = %v, want %v", *got, *tt.want)
			}
		})
	}
}

func TestProviderDailyStatsMergesAliasesAndCalculatesCacheHitRate(t *testing.T) {
	setupRenameTestEnv(t)
	withFixedLocal(t)
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("get database: %v", err)
	}
	if err := ensureRequestLogTableWithDB(db); err != nil {
		t.Fatalf("complete request log schema: %v", err)
	}

	providerService := NewProviderService()
	provider := Provider{ID: 1, Name: "current"}
	saveProviderFixture(t, providerService, []Provider{provider})
	if _, err := db.Exec(
		`INSERT INTO provider_alias (platform, provider_id, alias_name, canonical_name, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		CodexPlatform, provider.ID, "legacy", provider.Name,
		time.Now().Add(time.Hour).UTC().Format(timeLayout),
	); err != nil {
		t.Fatalf("insert provider alias: %v", err)
	}

	createdAt := time.Now().UTC().Format(timeLayout)
	rows := []struct {
		provider     string
		httpCode     int
		inputTokens  int
		cachedTokens int
		outputTokens int
	}{
		{provider: "legacy", httpCode: 200, inputTokens: 900, cachedTokens: 100, outputTokens: 10},
		{provider: "current", httpCode: 502, inputTokens: 100, cachedTokens: 0, outputTokens: 20},
		// This request has no input usage. It remains in request totals but must
		// not turn an otherwise valid cache rate into a miss.
		{provider: "legacy", httpCode: 499, inputTokens: 0, cachedTokens: 0, outputTokens: 30},
	}
	for _, row := range rows {
		if _, err := db.Exec(
			`INSERT INTO request_log
			 (platform, model, provider, http_code, input_tokens, output_tokens,
			  cache_read_tokens, reasoning_tokens, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
			CodexPlatform, "gpt-5", row.provider, row.httpCode,
			row.inputTokens, row.outputTokens, row.cachedTokens, createdAt,
		); err != nil {
			t.Fatalf("insert request log: %v", err)
		}
	}

	stats, err := NewLogService(providerService).ProviderDailyStats(CodexPlatform)
	if err != nil {
		t.Fatalf("ProviderDailyStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("ProviderDailyStats returned %d rows, want 1", len(stats))
	}
	stat := stats[0]
	if stat.Provider != "current" {
		t.Fatalf("provider = %q, want canonical name current", stat.Provider)
	}
	if stat.TotalRequests != 3 || stat.SuccessfulRequests != 1 || stat.FailedRequests != 2 {
		t.Fatalf("unexpected request totals: %+v", stat)
	}
	if stat.InputTokens != 1000 || stat.CacheReadTokens != 100 || stat.OutputTokens != 60 {
		t.Fatalf("unexpected token totals: %+v", stat)
	}
	if stat.CacheHitRate == nil {
		t.Fatal("cache hit rate = nil, want a value")
	}
	if want := 100.0 / 1100.0; math.Abs(*stat.CacheHitRate-want) > 1e-12 {
		t.Fatalf("cache hit rate = %v, want %v", *stat.CacheHitRate, want)
	}
}

func TestProviderDailyStatsReturnsNilCacheHitRateWithoutInputUsage(t *testing.T) {
	setupRenameTestEnv(t)
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("get database: %v", err)
	}
	if err := ensureRequestLogTableWithDB(db); err != nil {
		t.Fatalf("complete request log schema: %v", err)
	}
	createdAt := time.Now().UTC().Format(timeLayout)
	if _, err := db.Exec(
		`INSERT INTO request_log
		 (platform, model, provider, http_code, input_tokens, output_tokens,
		  cache_read_tokens, reasoning_tokens, created_at)
		 VALUES (?, ?, ?, 200, 0, 40, 0, 0, ?)`,
		CodexPlatform, "gpt-5", "no-usage", createdAt,
	); err != nil {
		t.Fatalf("insert request log: %v", err)
	}

	stats, err := NewLogService().ProviderDailyStats(CodexPlatform)
	if err != nil {
		t.Fatalf("ProviderDailyStats: %v", err)
	}
	if len(stats) != 1 || stats[0].CacheHitRate != nil {
		t.Fatalf("unexpected no-usage stats: %+v", stats)
	}
}

func float64Ptr(value float64) *float64 {
	return &value
}
