package github

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestClientRecommendedPollDelayUsesRateLimitHeaders(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := 30 * time.Second

	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{
			name: "ten percent doubles cadence",
			header: http.Header{
				"X-Ratelimit-Limit":     []string{"5000"},
				"X-Ratelimit-Remaining": []string{"500"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
			},
			want: 2 * base,
		},
		{
			name: "five percent quadruples cadence",
			header: http.Header{
				"X-Ratelimit-Limit":     []string{"5000"},
				"X-Ratelimit-Remaining": []string{"250"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
			},
			want: 4 * base,
		},
		{
			name: "one percent uses maximum widening",
			header: http.Header{
				"X-Ratelimit-Limit":     []string{"5000"},
				"X-Ratelimit-Remaining": []string{"50"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
			},
			want: 8 * base,
		},
		{
			name: "exhausted quota waits through reset",
			header: http.Header{
				"X-Ratelimit-Limit":     []string{"5000"},
				"X-Ratelimit-Remaining": []string{"0"},
				"X-Ratelimit-Reset":     []string{strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)},
			},
			want: 10*time.Minute + time.Second,
		},
		{
			name: "secondary limit honors retry after",
			header: http.Header{
				"Retry-After": []string{"90"},
			},
			want: 91 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(ClientOptions{})
			client.observeRateLimitHeaders(tt.header, now)
			if got := client.RecommendedPollDelay(now, base); got != tt.want {
				t.Fatalf("RecommendedPollDelay() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestClientRecommendedPollDelayDropsExpiredSnapshot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	client := NewClient(ClientOptions{})
	client.observeRateLimitHeaders(http.Header{
		"X-Ratelimit-Limit":     []string{"5000"},
		"X-Ratelimit-Remaining": []string{"0"},
		"X-Ratelimit-Reset":     []string{strconv.FormatInt(now.Add(time.Minute).Unix(), 10)},
	}, now)

	if got := client.RecommendedPollDelay(now.Add(time.Minute+time.Second), 30*time.Second); got != 30*time.Second {
		t.Fatalf("RecommendedPollDelay() after reset = %s, want 30s", got)
	}
}
