package gitx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var gitVersionPattern = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)\.([0-9]+)(?:$|[. +\-])`)

// parseGitVersion accepts vendor suffixes, including Windows and Apple builds.
func parseGitVersion(out string) ([3]int, error) {
	var version [3]int
	parts := gitVersionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if parts == nil {
		return version, fmt.Errorf("unrecognized git version %q", strings.TrimSpace(out))
	}
	for i := range version {
		n, err := strconv.Atoi(parts[i+1])
		if err != nil {
			return version, fmt.Errorf("invalid git version component: %w", err)
		}
		version[i] = n
	}
	return version, nil
}

// CheckVersion validates the git --version output against the clone safety floor.
func CheckVersion(out string) error {
	return checkGitVersion(out)
}

func checkGitVersion(out string) error {
	found, err := parseGitVersion(out)
	if err == nil {
		required, parseErr := parseGitVersion("git version " + MinimumGitVersion)
		if parseErr != nil {
			return parseErr
		}
		for i := range found {
			if found[i] > required[i] {
				return nil
			}
			if found[i] < required[i] {
				break
			}
			if i == len(found)-1 {
				return nil
			}
		}
	}
	return fmt.Errorf("found %q; git >= %s is required", strings.TrimSpace(out), MinimumGitVersion)
}
