// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// maxQuantity rejects values that would overflow int64 (below MaxInt64 with a
// margin for float rounding); it is checked before any float→int conversion,
// whose out-of-range result is platform-dependent.
const maxQuantity = 9.0e18

// cpuQuotaPerSecUSec converts a CPU limit to systemd's CPUQuotaPerSecUSec
// (µs of CPU time per second; 1_000_000 = one core). Accepted forms:
//
//	"20%" "150%"            systemd CPUQuota percentages
//	"200m" "1500m"          Kubernetes millicores
//	"0.5" "2"               Kubernetes cores
//
// Rounded to whole millicores (10 µs/s granularity is below what systemd
// enforces). Non-positive, non-finite and overflowing input is rejected.
func cpuQuotaPerSecUSec(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cpu value")
	}
	var milli float64
	switch {
	case strings.HasSuffix(s, "%"):
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu percentage %q", s)
		}
		milli = f * 10 // 100% = 1000m
	case strings.HasSuffix(s, "m"):
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "m")), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu millicores %q", s)
		}
		milli = f
	default:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu cores %q", s)
		}
		milli = f * 1000
	}
	if math.IsNaN(milli) || math.IsInf(milli, 0) {
		return 0, fmt.Errorf("cpu %q is not a finite number", s)
	}
	milli = math.Round(milli)
	if milli <= 0 {
		return 0, fmt.Errorf("cpu %q resolves to a non-positive quota", s)
	}
	if milli >= maxQuantity/1000 {
		return 0, fmt.Errorf("cpu %q exceeds the maximum supported quota", s)
	}
	return uint64(milli) * 1000, nil
}

// cpuQuotaPercent renders CPUQuotaPerSecUSec as the CPUQuota= directive
// value with at most one decimal: 200000 → "20%", 1000 → "0.1%".
func cpuQuotaPercent(perSecUSec uint64) string {
	tenths := (perSecUSec + 500) / 1000 // one tenth of a percent = 1000 µs/s
	if tenths%10 == 0 {
		return fmt.Sprintf("%d%%", tenths/10)
	}
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

// memoryBytes converts a memory limit to bytes: Ki…Ei are powers of 1024,
// k/K…E powers of 1000, a bare number is bytes ("1Gi" → 1073741824,
// "512M" → 512000000). Systemd's own "1G" spelling means 1024³, so the
// Kubernetes reading is applied deliberately and documented.
func memoryBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	// Longest suffix first so "Mi" wins over "M".
	suffixes := []struct {
		suf  string
		mult float64
	}{
		{"Ei", 1 << 60}, {"Pi", 1 << 50}, {"Ti", 1 << 40},
		{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"E", 1e18}, {"P", 1e15}, {"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"k", 1e3}, {"K", 1e3},
	}
	mant, mult := s, 1.0
	for _, u := range suffixes {
		if rest, ok := strings.CutSuffix(s, u.suf); ok {
			mant, mult = rest, u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(mant), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q", s)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("memory %q is not a finite number", s)
	}
	v := f * mult
	if v <= 0 {
		return 0, fmt.Errorf("memory %q resolves to a non-positive byte count", s)
	}
	if v >= maxQuantity {
		return 0, fmt.Errorf("memory %q exceeds the maximum supported byte count", s)
	}
	return uint64(math.Round(v)), nil
}
