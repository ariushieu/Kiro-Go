package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestScanLocalKiroCredentialsIdC(t *testing.T) {
	dir := t.TempDir()
	SetLocalCacheDirForTest(dir)
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	hash := "abc123hash"
	writeFile(t, dir, "kiro-auth-token.json", `{
		"accessToken": "at-value",
		"refreshToken": "rt-value-1234567890",
		"expiresAt": "2026-06-22T17:00:00Z",
		"clientIdHash": "`+hash+`",
		"authMethod": "IdC",
		"provider": "Enterprise",
		"region": "us-east-1"
	}`)
	writeFile(t, dir, hash+".json", `{
		"clientId": "client-abc",
		"clientSecret": "secret-xyz"
	}`)

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(creds))
	}
	c := creds[0]
	if c.AuthMethod != "idc" {
		t.Fatalf("authMethod = %q, want idc", c.AuthMethod)
	}
	if !c.HasClient || c.ClientID != "client-abc" || c.ClientSecret != "secret-xyz" {
		t.Fatalf("client not resolved: %+v", c)
	}
	if c.RefreshToken != "rt-value-1234567890" {
		t.Fatalf("refresh token mismatch: %q", c.RefreshToken)
	}
	if c.Region != "us-east-1" {
		t.Fatalf("region = %q", c.Region)
	}
	if c.Fingerprint == "" {
		t.Fatalf("expected a fingerprint")
	}
}

func TestScanMissingClientFile(t *testing.T) {
	dir := t.TempDir()
	SetLocalCacheDirForTest(dir)
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	writeFile(t, dir, "kiro-auth-token.json", `{
		"refreshToken": "rt-noclient-987654321",
		"clientIdHash": "missinghash",
		"authMethod": "IdC",
		"provider": "Enterprise"
	}`)

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 credential, got %d", len(creds))
	}
	if creds[0].HasClient {
		t.Fatalf("expected HasClient=false when client file absent")
	}
	// Still classified as idc by provider hint even without client material.
	if creds[0].AuthMethod != "idc" {
		t.Fatalf("authMethod = %q, want idc", creds[0].AuthMethod)
	}
}

func TestScanSocialNeedsNoClient(t *testing.T) {
	dir := t.TempDir()
	SetLocalCacheDirForTest(dir)
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	writeFile(t, dir, "kiro-auth-token.json", `{
		"refreshToken": "rt-social-abcdef1234",
		"authMethod": "social",
		"provider": "Google"
	}`)

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(creds) != 1 || creds[0].AuthMethod != "social" {
		t.Fatalf("expected one social credential, got %+v", creds)
	}
}

func TestScanDeduplicatesByRefreshToken(t *testing.T) {
	dir := t.TempDir()
	SetLocalCacheDirForTest(dir)
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	// Same refresh token in two files → one credential.
	body := `{"refreshToken":"rt-dup-111122223333","authMethod":"social","provider":"Google"}`
	writeFile(t, dir, "kiro-auth-token.json", body)
	writeFile(t, dir, "other-token.json", body)

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected dedup to 1, got %d", len(creds))
	}
	// kiro-auth-token.json must be the surviving source (processed first).
	if creds[0].SourceFile != "kiro-auth-token.json" {
		t.Fatalf("expected kiro-auth-token.json first, got %q", creds[0].SourceFile)
	}
}

func TestScanHandlesBOMAndIgnoresNonToken(t *testing.T) {
	dir := t.TempDir()
	SetLocalCacheDirForTest(dir)
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	// BOM-prefixed token file (Windows tooling).
	bom := "\xEF\xBB\xBF" + `{"refreshToken":"rt-bom-5566778899","authMethod":"social","provider":"Google"}`
	writeFile(t, dir, "kiro-auth-token.json", bom)
	// A pure client registration (no refreshToken) must be ignored.
	writeFile(t, dir, "deadbeef.json", `{"clientId":"c","clientSecret":"s"}`)

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("expected 1 credential (client-only ignored), got %d", len(creds))
	}
	if creds[0].RefreshToken != "rt-bom-5566778899" {
		t.Fatalf("BOM not stripped / wrong token: %q", creds[0].RefreshToken)
	}
}

func TestScanMissingDirReturnsEmpty(t *testing.T) {
	SetLocalCacheDirForTest(filepath.Join(t.TempDir(), "does-not-exist"))
	t.Cleanup(func() { SetLocalCacheDirForTest("") })

	creds, err := ScanLocalKiroCredentials()
	if err != nil {
		t.Fatalf("expected no error for missing dir, got %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("expected empty result, got %d", len(creds))
	}
}

func TestLocalCacheDirUsesHome(t *testing.T) {
	SetLocalCacheDirForTest("")
	dir, err := LocalCacheDir()
	if err != nil {
		t.Fatalf("LocalCacheDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".aws", "sso", "cache")
	if dir != want {
		t.Fatalf("LocalCacheDir = %q, want %q", dir, want)
	}
}
