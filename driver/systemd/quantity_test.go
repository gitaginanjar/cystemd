// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import "testing"

func TestCPUQuotaPerSecUSec(t *testing.T) {
	for in, want := range map[string]uint64{
		"200m":  200_000,
		"1500m": 1_500_000,
		"2":     2_000_000,
		"0.5":   500_000,
		"20%":   200_000,
		"150%":  1_500_000,
		"0.1%":  1_000,
		"1m":    1_000,
	} {
		got, err := cpuQuotaPerSecUSec(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "abc", "NaN", "1e30", "0m", "x%"} {
		if _, err := cpuQuotaPerSecUSec(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestCPUQuotaPercent(t *testing.T) {
	for in, want := range map[uint64]string{
		200_000:   "20%",
		1_500_000: "150%",
		1_000:     "0.1%",
		2_500:     "0.3%", // rounded to one decimal, which systemd stores exactly
	} {
		if got := cpuQuotaPercent(in); got != want {
			t.Errorf("%d = %q, want %q", in, got, want)
		}
	}
}

func TestMemoryBytes(t *testing.T) {
	for in, want := range map[string]uint64{
		"1Gi":        1 << 30,
		"512Mi":      512 << 20,
		"512M":       512_000_000,
		"1073741824": 1073741824,
		"1.5Gi":      1610612736,
		"64k":        64_000,
	} {
		got, err := memoryBytes(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Errorf("%q = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "0", "-1Gi", "lots", "Inf", "100Ei"} {
		if _, err := memoryBytes(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}
