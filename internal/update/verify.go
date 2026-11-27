package update

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Release assets are checked against an Ed25519 key built into Collie, not
// just HTTPS: whoever can swap a release asset (or the download path) must
// not be able to run code on every server that updates.
//
// A release carries SHA256SUMS (sha256sum format) and SHA256SUMS.sig: the
// base64 signature of SignedMessage(tag, SHA256SUMS).

//go:embed release.pub
var releasePub string

// Keys returns the release signing keys this build trusts.
func Keys() []ed25519.PublicKey {
	keys, _ := ParseKeys(releasePub)
	return keys
}

// ParseKeys reads base64 Ed25519 public keys, one per line (# comments).
// Several keys allow rotating to a new one: a release signed with the old
// key ships the new one.
func ParseKeys(s string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid public key %q", line)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}

// SignedMessage is what a release signature covers. The tag ties the
// checksums to one release, so an older release cannot be passed off as a
// newer one.
func SignedMessage(tag string, sums []byte) []byte {
	return append([]byte("collie-release-v1\n"+tag+"\n"), sums...)
}

// Sign returns the content of SHA256SUMS.sig.
func Sign(key ed25519.PrivateKey, tag string, sums []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, SignedMessage(tag, sums))) + "\n")
}

// ParsePrivateKey reads a base64 Ed25519 seed (32 bytes) or full private
// key (64 bytes).
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	switch {
	case err != nil:
		return nil, errors.New("signing key is not base64")
	case len(b) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, errors.New("signing key has the wrong length")
}

var errNoKeys = errors.New("this build has no release signing key, so updates cannot be verified")

// VerifySums checks the signature over a release's SHA256SUMS (the
// release tooling uses it to catch a signing key that does not match
// release.pub before publishing).
func VerifySums(keys []ed25519.PublicKey, tag string, sums, sig []byte) error {
	_, err := verifySums(keys, tag, sums, sig)
	return err
}

// verifySums checks the signature over a release's SHA256SUMS and returns
// the checksums by file name.
func verifySums(keys []ed25519.PublicKey, tag string, sums, sig []byte) (map[string]string, error) {
	if len(keys) == 0 {
		return nil, errNoKeys
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("malformed release signature")
	}
	msg := SignedMessage(tag, sums)
	valid := false
	for _, k := range keys {
		if ed25519.Verify(k, msg, raw) {
			valid = true
			break
		}
	}
	if !valid {
		return nil, errors.New("release signature does not match this build's signing key")
	}
	return parseSums(sums)
}

// parseSums reads `sha256sum` output ("<hex>  <name>", or "<hex> *<name>").
func parseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if h, err := hex.DecodeString(sum); !ok || err != nil || len(h) != 32 || name == "" {
			return nil, fmt.Errorf("malformed checksum line %q", line)
		}
		out[name] = strings.ToLower(sum)
	}
	if len(out) == 0 {
		return nil, errors.New("empty checksum list")
	}
	return out, sc.Err()
}
