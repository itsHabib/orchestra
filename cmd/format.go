package cmd

import (
	"fmt"
	"strconv"
)

// fmtTokens formats a token count as a human-readable string (e.g. "284K", "1.2M").
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}
