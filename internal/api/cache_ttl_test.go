package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveCacheTTL — the three-level resolution table per yaad-index
//. Sentinel rules at each level: 0 = no opinion (fall through),
// positive = N seconds, negative = infinite. All-zero → 0 (caller
// stamps nothing, lookup treats as infinite).
func TestResolveCacheTTL_Hierarchy(t *testing.T) {
	t.Parallel()

	intp := func(n int) *int { return &n }

	cases := []struct {
		name string
		entry *int
		plugin int
		global int
		want int
	}{
		{"all zero / all-absent → 0 (no stamp)", nil, 0, 0, 0},
		{"global only", nil, 0, 86400, 86400},
		{"plugin only", nil, 3600, 0, 3600},
		{"entry only", intp(60), 0, 0, 60},
		{"plugin overrides global", nil, 3600, 86400, 3600},
		{"entry overrides plugin and global", intp(60), 3600, 86400, 60},
		{"entry zero falls through to plugin", intp(0), 3600, 86400, 3600},
		{"explicit-zero plugin falls through to global", nil, 0, 86400, 86400},
		{"negative entry pins infinite", intp(-1), 3600, 86400, -1},
		{"negative plugin pins infinite even when global is positive", nil, -1, 86400, -1},
		{"negative global is the ultimate fallback", nil, 0, -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveCacheTTL(tc.entry, tc.plugin, tc.global)
			if got != tc.want {
				t.Fatalf("resolveCacheTTL(%v, %d, %d) = %d, want %d",
					tc.entry, tc.plugin, tc.global, got, tc.want)
			}
		})
	}
}

// resolveCacheExpires translates the resolved duration into the
// post- absolute-date stamp. Three cases: nil (no opinion),
// Never (negative TTL), and a finite Time at fetched_at + ttl.
func TestResolveCacheExpires_TranslatesResolvedTTL(t *testing.T) {
	t.Parallel()

	intp := func(n int) *int { return &n }
	fetchedAt := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)

	t.Run("all-zero → nil", func(t *testing.T) {
		t.Parallel()
		got := resolveCacheExpires(nil, 0, 0, fetchedAt)
		assert.Nil(t, got)
	})

	t.Run("entry zero, plugin zero, global positive → finite", func(t *testing.T) {
		t.Parallel()
		got := resolveCacheExpires(nil, 0, 3600, fetchedAt)
		require.NotNil(t, got)
		require.False(t, got.Never)
		assert.True(t, fetchedAt.Add(time.Hour).Equal(got.Time),
			"want fetched_at + 1h, got %s", got.Time)
	})

	t.Run("plugin negative → Never sentinel", func(t *testing.T) {
		t.Parallel()
		got := resolveCacheExpires(nil, -1, 3600, fetchedAt)
		require.NotNil(t, got)
		assert.True(t, got.Never)
	})

	t.Run("entry override 60s wins over plugin 3600", func(t *testing.T) {
		t.Parallel()
		got := resolveCacheExpires(intp(60), 3600, 0, fetchedAt)
		require.NotNil(t, got)
		require.False(t, got.Never)
		assert.True(t, fetchedAt.Add(60*time.Second).Equal(got.Time))
	})
}

// nil entry pointer is the same as absent — falls through to plugin.
func TestResolveCacheTTL_NilEntryFallsThrough(t *testing.T) {
	t.Parallel()
	if got := resolveCacheTTL(nil, 0, 0); got != 0 {
		t.Fatalf("got %d, want 0 (all-zero)", got)
	}
	if got := resolveCacheTTL(nil, 100, 0); got != 100 {
		t.Fatalf("got %d, want 100 (plugin)", got)
	}
}
