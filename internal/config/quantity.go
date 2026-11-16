package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Sizes are written like Kubernetes quantities: 512Mi, 4Gi, 1Ti (binary) or 4G (decimal).
var units = []struct {
	suffix string
	bytes  float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
}

// parseBytes reads a quantity. A bare number is refused: "4" could mean bytes or gigabytes.
func parseBytes(s string) (float64, error) {
	v := strings.TrimSpace(s)
	for _, u := range units {
		if num, ok := strings.CutSuffix(v, u.suffix); ok {
			n, err := strconv.ParseFloat(num, 64)
			if err != nil || n <= 0 {
				break
			}
			return n * u.bytes, nil
		}
	}
	return 0, fmt.Errorf("%q is not a size: write a number with a unit, like 512Mi, 4Gi or 1Ti", s)
}

// MiB converts a quantity to whole mebibytes, rounding up.
func MiB(s string) (int, error) {
	b, err := parseBytes(s)
	if err != nil {
		return 0, err
	}
	return int(math.Ceil(b / (1 << 20))), nil
}

// GiB converts a quantity to whole gibibytes, rounding up.
func GiB(s string) (int, error) {
	b, err := parseBytes(s)
	if err != nil {
		return 0, err
	}
	return int(math.Ceil(b / (1 << 30))), nil
}
