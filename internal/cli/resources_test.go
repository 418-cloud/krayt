package cli

import (
	"strings"
	"testing"
)

func TestCheckHostResources(t *testing.T) {
	cases := []struct {
		name                    string
		freeMemMiB, freeDiskGiB uint64
		wantMemMiB, wantDiskGiB uint64
		wantErr                 bool
		wantErrContains         string
	}{
		{
			name: "plenty of both", freeMemMiB: 16384, freeDiskGiB: 200,
			wantMemMiB: 4096, wantDiskGiB: 20, wantErr: false,
		},
		{
			name: "short on memory only", freeMemMiB: 5000, freeDiskGiB: 200,
			wantMemMiB: 4096, wantDiskGiB: 20, wantErr: true,
			wantErrContains: "insufficient free memory",
		},
		{
			name: "short on disk only", freeMemMiB: 16384, freeDiskGiB: 22,
			wantMemMiB: 4096, wantDiskGiB: 20, wantErr: true,
			wantErrContains: "insufficient free disk",
		},
		{
			// Both memory and disk are short; the memory check runs first, so its error wins.
			name: "short on both returns the memory error first", freeMemMiB: 100, freeDiskGiB: 1,
			wantMemMiB: 4096, wantDiskGiB: 20, wantErr: true,
			wantErrContains: "insufficient free memory",
		},
		{
			// Exactly at the margin boundary (free == want+margin) must pass, not off-by-one fail.
			name:        "exactly at the margin boundary passes",
			freeMemMiB:  4096 + memMarginMiB,
			freeDiskGiB: 20 + diskMarginGiB,
			wantMemMiB:  4096, wantDiskGiB: 20, wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHostResources(tc.freeMemMiB, tc.freeDiskGiB, tc.wantMemMiB, tc.wantDiskGiB)
			if tc.wantErr && err == nil {
				t.Fatalf("checkHostResources() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkHostResources() = %v, want nil", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("checkHostResources() = %q, want it to contain %q", err.Error(), tc.wantErrContains)
			}
		})
	}
}
