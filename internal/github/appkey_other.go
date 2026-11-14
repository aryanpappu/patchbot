//go:build !unix

package github

import (
	"errors"
	"os"
)

func keyOwner(os.FileInfo) error {
	return errors.New("private_key_file: GitHub App keys require a Unix host")
}

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304 -- operator-configured, validated key path.
}
