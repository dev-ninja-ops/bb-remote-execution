package resourcepool

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCPUString parses a user-supplied CPU quantity into millicores.
// Accepted forms:
//
//	"2"          -> 2 cores      = 2000 millicores
//	"2.5"        -> 2.5 cores    = 2500 millicores
//	"500m"       -> 0.5 cores    = 500 millicores
//	"2000m"      -> 2.0 cores    = 2000 millicores
//
// Empty input returns 0 with no error (caller decides if that's fine).
// Trailing/leading whitespace is tolerated.
func ParseCPUString(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu %q (expected integer millicores before the 'm' suffix): %w", s, err)
		}
		return uint32(n), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu %q (expected core count, integer or decimal, or '<N>m' for millicores): %w", s, err)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid cpu %q: must be non-negative", s)
	}
	return uint32(f*1000 + 0.5), nil
}

// ParseMemoryString parses a user-supplied memory quantity into bytes.
// Accepts both binary (Ki, Mi, Gi, Ti) and decimal (K, M, G, T)
// suffixes, plus bare bytes. Empty input returns 0 with no error.
func ParseMemoryString(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(s, "Ki"):
		multiplier = 1 << 10
		s = strings.TrimSuffix(s, "Ki")
	case strings.HasSuffix(s, "Mi"):
		multiplier = 1 << 20
		s = strings.TrimSuffix(s, "Mi")
	case strings.HasSuffix(s, "Gi"):
		multiplier = 1 << 30
		s = strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "Ti"):
		multiplier = 1 << 40
		s = strings.TrimSuffix(s, "Ti")
	case strings.HasSuffix(s, "K"):
		multiplier = 1_000
		s = strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		multiplier = 1_000_000
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		multiplier = 1_000_000_000
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "T"):
		multiplier = 1_000_000_000_000
		s = strings.TrimSuffix(s, "T")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q (expected bytes, optionally with K/M/G/T or Ki/Mi/Gi/Ti suffix): %w", s, err)
	}
	return n * multiplier, nil
}
