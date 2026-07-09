package cli

import (
	"strconv"
	"strings"
)

func parseVersionTag(tag string) (int, bool) {
	tag = strings.TrimSpace(tag)
	if !strings.HasPrefix(tag, "v") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tag, "v"))
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func versionTagNumbers(tags []string) (map[int]bool, int) {
	seen := make(map[int]bool)
	maxN := 0
	for _, tag := range tags {
		n, ok := parseVersionTag(tag)
		if !ok {
			continue
		}
		seen[n] = true
		if n > maxN {
			maxN = n
		}
	}
	return seen, maxN
}
