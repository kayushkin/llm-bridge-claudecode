package main

import "testing"

func TestNegotiateProtocolVersion(t *testing.T) {
	tests := []struct {
		name   string
		client string
		want   string
	}{
		{
			name:   "exact match — newest",
			client: "2025-11-25",
			want:   "2025-11-25",
		},
		{
			name:   "exact match — older but still supported",
			client: "2025-06-18",
			want:   "2025-06-18",
		},
		{
			name:   "exact match — oldest streamable HTTP",
			client: "2025-03-26",
			want:   "2025-03-26",
		},
		{
			name:   "unknown version — fall back to newest we support",
			client: "2099-12-31",
			want:   supportedProtocolVersions[0],
		},
		{
			name:   "ancient unsupported — fall back to newest we support",
			client: "2024-11-05",
			want:   supportedProtocolVersions[0],
		},
		{
			name:   "empty (client did not send) — fall back to newest",
			client: "",
			want:   supportedProtocolVersions[0],
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := negotiateProtocolVersion(tc.client); got != tc.want {
				t.Errorf("negotiateProtocolVersion(%q) = %q, want %q", tc.client, got, tc.want)
			}
		})
	}
}
