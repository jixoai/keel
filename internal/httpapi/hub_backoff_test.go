package httpapi

import (
	"testing"
	"time"
)

// TestNextNotifyBackoffDoublesAndCaps pins the reconnect delay that keeps a
// down database from turning the notify listener into a busy log loop.
func TestNextNotifyBackoffDoublesAndCaps(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{0, 2 * time.Second},
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{8 * time.Second, 16 * time.Second},
		{16 * time.Second, notifyMaxBackoff},
		{notifyMaxBackoff, notifyMaxBackoff},
		{notifyMaxBackoff * 2, notifyMaxBackoff},
	}
	for _, tc := range cases {
		if got := nextNotifyBackoff(tc.in); got != tc.want {
			t.Fatalf("nextNotifyBackoff(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
