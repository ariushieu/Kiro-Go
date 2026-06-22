package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// localCacheDirOverride lets tests point the scanner at a temp directory.
var localCacheDirOverride string

// SetLocalCacheDirForTest overrides the SSO cache directory used by the scanner.
// Pass "" to restore the OS default. Test-only.
func SetLocalCacheDirForTest(dir string) { localCacheDirOverride = dir }

// LocalCacheDir returns the directory where the Kiro IDE / AWS SSO CLI store
// their cached tokens. The location is the same on every platform relative to
// the user's home directory:
//
//	<home>/.aws/sso/cache
//
// On Windows <home> is %USERPROFILE% (resolved by os.UserHomeDir), on macOS and
// Linux it is $HOME. Returning an absolute, cleaned path keeps later joins safe.
func LocalCacheDir() (string, error) {
	if localCacheDirOverride != "" {
		return localCacheDirOverride, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".aws", "sso", "cache"), nil
}

// kiroTokenFile is the canonical token file the Kiro IDE writes for the active
// session. Other files in the cache are hashed SSO/OIDC registrations.
const kiroTokenFile = "kiro-auth-token.json"

// rawTokenFile mirrors the on-disk shape of kiro-auth-token.json. Unknown fields
// are ignored; absent fields decode to their zero value.
type rawTokenFile struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	ClientIDHash string `json:"clientIdHash"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider"`
	Region       string `json:"region"`
	// External IdP fields, present only for Kiro Hosted SSO (Microsoft Entra etc.)
	IssuerURL   string `json:"issuerUrl"`
	IdPClientID string `json:"idpClientId"`
	Scopes      string `json:"scopes"`
	LoginHint   string `json:"loginHint"`
	StartURL    string `json:"startUrl"`
}

// rawClientFile mirrors the on-disk shape of a {hash}.json OIDC client file.
type rawClientFile struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// LocalCredential is one fully-resolved credential discovered in the local
// cache, ready to be handed to the credential import flow. It intentionally
// carries the secret material so the caller can import without re-reading disk;
// use Masked* helpers for any value shown to a user.
type LocalCredential struct {
	SourceFile   string `json:"sourceFile"`   // token file name (e.g. kiro-auth-token.json)
	AccessToken  string `json:"-"`            // never serialized
	RefreshToken string `json:"-"`            // never serialized
	ClientID     string `json:"-"`            // never serialized
	ClientSecret string `json:"-"`            // never serialized
	AuthMethod   string `json:"authMethod"`   // idc | social | external_idp
	Provider     string `json:"provider"`     // BuilderId | Enterprise | Google | ...
	Region       string `json:"region"`       // auth region from the token file
	IssuerURL    string `json:"issuerUrl,omitempty"`
	IdPClientID  string `json:"idpClientId,omitempty"`
	Scopes       string `json:"scopes,omitempty"`
	LoginHint    string `json:"loginHint,omitempty"`
	HasClient    bool   `json:"hasClient"`    // whether clientId/secret were resolved
	HasRefresh   bool   `json:"hasRefresh"`   // whether a refresh token is present
	Fingerprint  string `json:"fingerprint"`  // short, non-secret id for de-dup/selection
}

// normalizeAuthMethod maps the token file's authMethod/provider onto the values
// the importer expects: "idc", "social", or "external_idp".
func normalizeAuthMethod(authMethod, provider string, hasClient bool) string {
	am := strings.ToLower(strings.TrimSpace(authMethod))
	switch am {
	case "external_idp", "externalidp":
		return "external_idp"
	case "idc", "builderid", "enterprise":
		return "idc"
	case "social", "google", "github":
		return "social"
	}
	// Fall back to provider hints, then to client-credential presence.
	p := strings.ToLower(strings.TrimSpace(provider))
	if strings.Contains(p, "entra") || strings.Contains(p, "microsoft") {
		return "external_idp"
	}
	if hasClient {
		return "idc"
	}
	return "social"
}

// ScanLocalKiroCredentials inspects the local SSO cache and returns every
// importable Kiro credential it can fully resolve. The Kiro IDE token file
// (kiro-auth-token.json) is tried first; the function then scans any remaining
// *.json files that themselves look like token files (carry a refreshToken),
// so multiple logged-in identities can be surfaced.
//
// For IdC tokens the matching {clientIdHash}.json client file is loaded to
// recover clientId/clientSecret. A credential is returned even if the client
// file is missing (HasClient=false) so the caller can decide; social tokens do
// not need one.
//
// Returns an empty slice (not an error) when the cache directory is absent —
// that simply means no local Kiro IDE is present (e.g. containerized deploys).
func ScanLocalKiroCredentials() ([]LocalCredential, error) {
	dir, err := LocalCacheDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []LocalCredential{}, nil
		}
		return nil, fmt.Errorf("read cache dir: %w", err)
	}

	// Order files so kiro-auth-token.json is processed first, then the rest
	// alphabetically for deterministic output.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == kiroTokenFile {
			return true
		}
		if names[j] == kiroTokenFile {
			return false
		}
		return names[i] < names[j]
	})

	var creds []LocalCredential
	seen := make(map[string]bool) // de-dup by refresh-token fingerprint
	for _, name := range names {
		tok, ok := readTokenFile(filepath.Join(dir, name))
		if !ok || strings.TrimSpace(tok.RefreshToken) == "" {
			// Not a usable token file (likely a pure client registration).
			continue
		}

		cred := LocalCredential{
			SourceFile:   name,
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			Provider:     tok.Provider,
			Region:       strings.TrimSpace(tok.Region),
			IssuerURL:    tok.IssuerURL,
			IdPClientID:  tok.IdPClientID,
			Scopes:       tok.Scopes,
			LoginHint:    tok.LoginHint,
			HasRefresh:   true,
		}
		if cred.Region == "" {
			cred.Region = "us-east-1"
		}

		// Resolve the OIDC client file for IdC tokens via clientIdHash.
		if hash := strings.TrimSpace(tok.ClientIDHash); hash != "" {
			if cli, ok := readClientFile(filepath.Join(dir, hash+".json")); ok {
				cred.ClientID = cli.ClientID
				cred.ClientSecret = cli.ClientSecret
				cred.HasClient = cli.ClientID != "" && cli.ClientSecret != ""
			}
		}

		cred.AuthMethod = normalizeAuthMethod(tok.AuthMethod, tok.Provider, cred.HasClient)
		cred.Fingerprint = fingerprint(tok.RefreshToken)

		if seen[cred.Fingerprint] {
			continue
		}
		seen[cred.Fingerprint] = true
		creds = append(creds, cred)
	}

	if creds == nil {
		creds = []LocalCredential{}
	}
	return creds, nil
}

// readTokenFile loads and decodes a candidate token file. ok is false when the
// file cannot be read or parsed as JSON.
func readTokenFile(path string) (rawTokenFile, bool) {
	var t rawTokenFile
	data, err := os.ReadFile(path)
	if err != nil {
		return t, false
	}
	if err := json.Unmarshal(stripBOM(data), &t); err != nil {
		return t, false
	}
	return t, true
}

// readClientFile loads and decodes an OIDC client registration file.
func readClientFile(path string) (rawClientFile, bool) {
	var c rawClientFile
	data, err := os.ReadFile(path)
	if err != nil {
		return c, false
	}
	if err := json.Unmarshal(stripBOM(data), &c); err != nil {
		return c, false
	}
	return c, true
}

// stripBOM removes a leading UTF-8 byte-order mark if present. Some Windows
// tooling writes BOM-prefixed JSON which the standard decoder rejects.
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// fingerprint returns a short, non-secret identifier for a token, used to
// de-duplicate the same identity appearing in multiple files and to let the UI
// reference a credential without exposing the secret.
func fingerprint(secret string) string {
	s := strings.TrimSpace(secret)
	if len(s) <= 8 {
		return "tok-" + s
	}
	return "tok-" + s[:4] + s[len(s)-4:]
}
