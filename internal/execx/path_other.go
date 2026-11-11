//go:build !unix

package execx

import "os"

func trustedOwner(_ os.FileInfo) error     { return nil }
func trustedDirectory(_ os.FileInfo) error { return nil }
