package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxKeyFileBytes = 64 << 10
	minKeyBits      = 2048
	// jwtBackdate absorbs clock drift between this host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime keeps exp under GitHub's 10 minute maximum, measured from now.
	jwtLifetime = 9 * time.Minute
)

// LoadAppPrivateKey reads and parses a GitHub App private key file once. The
// file must be an absolute path to a regular file (no symlink), mode 0600 or
// stricter, owned by the current user, and not inside any excluded root (the
// data directory and its workspaces). Errors never contain key material.
func LoadAppPrivateKey(path string, excludedRoots ...string) (*rsa.PrivateKey, error) {
	if err := CheckAppKeyFile(path, excludedRoots...); err != nil {
		return nil, err
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, errors.New("private_key_file: cannot open")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.New("private_key_file: cannot stat")
	}
	linked, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, linked) {
		return nil, errors.New("private_key_file: changed while opening")
	}
	if err := checkKeyInfo(opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxKeyFileBytes+1))
	if err != nil || len(data) > maxKeyFileBytes {
		return nil, errors.New("private_key_file: cannot read or larger than 64 KiB")
	}
	defer clear(data)
	return ParseAppPrivateKey(data)
}

// CheckAppKeyFile reports whether the key file is safely placed without
// reading it. Doctor uses it; LoadAppPrivateKey repeats it on the open file.
func CheckAppKeyFile(path string, excludedRoots ...string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return errors.New("private_key_file: must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("private_key_file: not found or not accessible")
	}
	if err := checkKeyInfo(info); err != nil {
		return err
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return errors.New("private_key_file: cannot resolve its directory")
	}
	resolved := filepath.Join(resolvedDir, filepath.Base(path))
	for _, root := range excludedRoots {
		if root == "" {
			continue
		}
		candidates := []string{filepath.Clean(root)}
		if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
			candidates = append(candidates, resolvedRoot)
		}
		for _, candidate := range candidates {
			for _, p := range []string{path, resolved} {
				if within(candidate, p) {
					return errors.New("private_key_file: must not be inside the data directory or workspaces")
				}
			}
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func checkKeyInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("private_key_file: must be a regular file, not a symlink or device")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("private_key_file: mode must be 0600 or stricter")
	}
	return keyOwner(info)
}

// ParseAppPrivateKey accepts a PKCS#1 ("RSA PRIVATE KEY") or PKCS#8
// ("PRIVATE KEY") PEM block holding an RSA key of at least 2048 bits.
func ParseAppPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("private_key_file: no PEM block")
	}
	defer clear(block.Bytes)
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("private_key_file: invalid PKCS#1 RSA key")
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("private_key_file: invalid PKCS#8 key")
		}
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private_key_file: PKCS#8 key is not RSA")
		}
		key = rsaKey
	default:
		return nil, errors.New("private_key_file: unsupported PEM block type")
	}
	if key.N.BitLen() < minKeyBits {
		return nil, errors.New("private_key_file: RSA key must be at least 2048 bits")
	}
	if err := key.Validate(); err != nil {
		return nil, errors.New("private_key_file: RSA key failed validation")
	}
	return key, nil
}

// SignAppJWT builds a GitHub App JWT: RS256, iat = now-60s, exp = now+9m,
// iss = app ID. It uses only the standard library.
func SignAppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	if key == nil || appID <= 0 {
		return "", errors.New("app JWT requires a key and a positive app ID")
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, err := json.Marshal(struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}{now.Add(-jwtBackdate).Unix(), now.Add(jwtLifetime).Unix(), strconv.FormatInt(appID, 10)})
	if err != nil {
		return "", errors.New("cannot encode app JWT claims")
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("cannot sign app JWT")
	}
	return signing + "." + enc.EncodeToString(signature), nil
}
