// Command vendor-vue downloads the exact Vue build and verifies its SRI pin.
package main

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aryanpappu/patchbot/internal/web"
)

func install(client *http.Client, integrity, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, web.VueURL, http.NoBody)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("vue download: HTTP %d", response.StatusCode)
	}
	const limit = 2 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return fmt.Errorf("vue download exceeds %d bytes", limit)
	}
	hash := sha512.Sum384(data)
	actual := "sha384-" + base64.StdEncoding.EncodeToString(hash[:])
	if actual != integrity {
		return fmt.Errorf("vue SRI mismatch: got %s, expected %s; nothing installed", actual, integrity)
	}
	f, err := os.CreateTemp(dir, ".vue-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "vue.global.prod.js"))
}

func main() {
	client := &http.Client{Timeout: 60 * time.Second}
	if err := install(client, web.VueIntegrity, filepath.Join("internal", "web", "assets")); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Verified and installed Vue " + web.VueVersion + "; rebuild patchbot to embed it.")
}
