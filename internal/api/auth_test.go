package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// testKey builds a syntactically valid key from a repeated byte —
// constructed, never a literal, so the CI secret grep stays clean.
func testKey(fill byte) string {
	raw := bytes.Repeat([]byte{fill}, keyRandomBytes)
	return keyMarker + base64.RawURLEncoding.EncodeToString(raw)
}

func TestGenerateKeyShapeAndDerivation(t *testing.T) {
	t.Parallel()
	entropy := bytes.Repeat([]byte{0x5a}, keyRandomBytes)
	plaintext, prefix, hashHex, err := generateKey(bytes.NewReader(entropy))
	if err != nil {
		t.Fatalf("generateKey: %v", err)
	}
	if len(plaintext) != keyTotalLen || !strings.HasPrefix(plaintext, keyMarker) {
		t.Fatalf("plaintext %q: want %d chars starting %q", plaintext, keyTotalLen, keyMarker)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(plaintext[len(keyMarker):])
	if err != nil || !bytes.Equal(decoded, entropy) {
		t.Fatalf("plaintext does not encode the entropy bytes: %v", err)
	}
	if prefix != plaintext[:keyLookupLen] {
		t.Errorf("prefix = %q, want the first %d chars of the plaintext", prefix, keyLookupLen)
	}
	sum := sha256.Sum256([]byte(plaintext))
	if hashHex != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %q, want hex sha256 of the plaintext", hashHex)
	}
	if !keyShapeOK(plaintext) {
		t.Error("generated key fails its own shape check")
	}

	// Entropy exhaustion surfaces as an error, never a short key.
	if _, _, _, err := generateKey(bytes.NewReader(entropy[:8])); err == nil {
		t.Error("short entropy reader: want error, got nil")
	}
}

func TestKeyShapeOK(t *testing.T) {
	t.Parallel()
	valid := testKey('x')
	cases := map[string]bool{
		valid:                                  true,
		"":                                     false,
		"sk_":                                  false,
		valid[:keyTotalLen-1]:                  false, // too short
		valid + "a":                            false, // too long
		"pk" + valid[2:]:                       false, // wrong marker
		valid[:keyTotalLen-1] + "+":            false, // not base64url
		strings.ToUpper(valid[:3]) + valid[3:]: false, // marker is case-sensitive
	}
	for token, want := range cases {
		if got := keyShapeOK(token); got != want {
			t.Errorf("keyShapeOK(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestHasScope(t *testing.T) {
	t.Parallel()
	if !hasScope([]string{"submit", "read"}, ScopeRead) {
		t.Error("direct scope not honored")
	}
	if hasScope([]string{"submit", "read"}, ScopeAdmin) {
		t.Error("admin granted without the admin scope")
	}
	if !hasScope([]string{"admin"}, ScopeSubmit) {
		t.Error("admin does not imply submit (ADR-007 says it must)")
	}
	if hasScope(nil, ScopeRead) {
		t.Error("empty grant set granted a scope")
	}
}

func TestValidateScopes(t *testing.T) {
	t.Parallel()
	if err := validateScopes([]string{"submit", "read", "approve", "admin"}); err != nil {
		t.Errorf("full vocabulary rejected: %v", err)
	}
	for name, scopes := range map[string][]string{
		"empty":     {},
		"nil":       nil,
		"unknown":   {"submit", "root"},
		"duplicate": {"read", "read"},
	} {
		if err := validateScopes(scopes); err == nil {
			t.Errorf("%s scope set accepted", name)
		}
	}
}

func TestNewRejectsMalformedRootKey(t *testing.T) {
	t.Parallel()
	_, err := New(nil, nil, nil, "not-a-key", RateLimitOptions{})
	if err == nil {
		t.Fatal("malformed root key accepted")
	}
	if strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("error %q echoes the root key value", err)
	}
}
