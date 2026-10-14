package testutil

import (
	"strings"
	"testing"
)

func TestOwnerPIDFromDirName(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"ad-sock-72399-1234567890", 72399},
		{"ad-tmux-42-7", 42},
		{"ad-sock-3687802701", 0},
		{"ad-tmux-3687802701", 0},
		{"ad-sock-12-34-56", 0},
		{"ad-sock-12-", 0},
		{"ad-sock--12", 0},
		{"ad-sock-0-12", 0},
		{"ad-sock-abc-12", 0},
		{"ad-sock-12-abc", 0},
		{"agent-deck-test-sock-12", 0},
	}
	for _, tc := range cases {
		for _, prefix := range ownedDirPrefixes {
			want := 0
			if strings.HasPrefix(tc.name, prefix) {
				want = tc.want
			}
			if got := ownerPIDFromDirName(tc.name, prefix); got != want {
				t.Errorf("ownerPIDFromDirName(%q, %q) = %d, want %d", tc.name, prefix, got, want)
			}
		}
	}
}
