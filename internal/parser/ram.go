package parser

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	reRAMRange = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*[-–—]\s*(\d+(?:[.,]\d+)?)\s*(tb|tib|gb|gib|mb|mib)\b`)
	reRAMSingle = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*(tb|tib|gb|gib|mb|mib)\b`)
)

// ParseRAMGB extracts RAM capacity in gigabytes from a Steam Memory spec string.
// Supports GB/GiB/MB/MiB/TB/TiB, comma/dot decimals, and ranges (picking the
// lower bound). Returns (0, false) if unparseable or outside (0, 4096] GB.
func ParseRAMGB(raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}

	if m := reRAMRange.FindStringSubmatch(s); len(m) == 4 {
		if gb, ok := convertRAMToGB(m[1], m[3]); ok {
			return gb, true
		}
	}

	if m := reRAMSingle.FindStringSubmatch(s); len(m) == 3 {
		if gb, ok := convertRAMToGB(m[1], m[2]); ok {
			return gb, true
		}
	}

	return 0, false
}

func convertRAMToGB(numStr, unit string) (float64, bool) {
	numStr = strings.ReplaceAll(numStr, ",", ".")
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil || val <= 0 {
		return 0, false
	}

	switch strings.ToLower(unit) {
	case "tb", "tib":
		val *= 1024.0
	case "gb", "gib":
		// already in GB
	case "mb", "mib":
		val /= 1024.0
	default:
		return 0, false
	}

	if val <= 0 || val > 4096 {
		return 0, false
	}
	return val, true
}
