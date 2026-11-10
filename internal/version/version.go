// Package version holds build metadata supplied through linker flags.
package version

import "fmt"

var (
	// Version is the release version or Git description.
	Version = "dev"
	// Commit is the short Git commit ID.
	Commit = "none"
	// Date is the UTC build timestamp.
	Date = "unknown"
)

// String returns the version and build metadata.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
