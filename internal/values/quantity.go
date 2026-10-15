// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package values

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ValidateCPU checks a Kubernetes CPU quantity ("200m", "0.5", "2") or a
// systemd percentage ("150%") — what the systemd driver's cpu_quota accepts.
func ValidateCPU(s string) error {
	s = strings.TrimSpace(s)
	var f float64
	var err error
	switch {
	case strings.HasSuffix(s, "%"):
		f, err = strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	case strings.HasSuffix(s, "m"):
		f, err = strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
	default:
		f, err = strconv.ParseFloat(s, 64)
	}
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return fmt.Errorf("invalid cpu limit %q", s)
	}
	return nil
}

// ValidateMemory checks a Kubernetes memory quantity ("1Gi", "512M", bytes).
func ValidateMemory(s string) error {
	s = strings.TrimSpace(s)
	mant := s
	for _, suf := range []string{"Ei", "Pi", "Ti", "Gi", "Mi", "Ki", "E", "P", "T", "G", "M", "k", "K"} {
		if rest, ok := strings.CutSuffix(s, suf); ok {
			mant = rest
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(mant), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return fmt.Errorf("invalid memory limit %q", s)
	}
	return nil
}
