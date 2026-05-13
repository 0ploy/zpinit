package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a uint64 that decodes from human-friendly TOML strings:
// "256MiB", "1GB", "512K", or bare bytes "104857600". Binary suffixes
// (Ki/Mi/Gi) use 1024; SI suffixes (K/M/G/KB/MB/GB) use 1000. The
// trailing "B" is optional in both. Zero value means "unset"; the
// loader applies defaults afterwards.
type ByteSize uint64

func (b ByteSize) Bytes() uint64 { return uint64(b) }

func (b *ByteSize) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		*b = 0
		return nil
	}
	// Split numeric prefix from unit suffix.
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if (c >= '0' && c <= '9') || c == '.' {
			break
		}
		i--
	}
	numStr := strings.TrimSpace(s[:i])
	unit := strings.TrimSpace(strings.ToLower(s[i:]))

	n, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return fmt.Errorf("byte size %q: %w", s, err)
	}
	if n < 0 {
		return fmt.Errorf("byte size %q: must be non-negative", s)
	}
	var mult uint64
	switch unit {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1000
	case "ki", "kib":
		mult = 1024
	case "m", "mb":
		mult = 1000 * 1000
	case "mi", "mib":
		mult = 1024 * 1024
	case "g", "gb":
		mult = 1000 * 1000 * 1000
	case "gi", "gib":
		mult = 1024 * 1024 * 1024
	default:
		return fmt.Errorf("byte size %q: unknown unit %q", s, unit)
	}
	*b = ByteSize(n * float64(mult))
	return nil
}

func (b ByteSize) MarshalText() ([]byte, error) {
	return []byte(strconv.FormatUint(uint64(b), 10)), nil
}
