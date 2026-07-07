package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// exportRaw calls the export handler with includeSecret and returns the decoded body.
func exportRaw(t *testing.T, includeSecret bool) map[string]interface{} {
	t.Helper()
	b, _ := json.Marshal(map[string]interface{}{"includeSecret": includeSecret})
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	h := &Handler{}
	h.apiExportApiKeys(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("export unmarshal: %v", err)
	}
	return out
}

func importBody(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/import", strings.NewReader(body))
	w := httptest.NewRecorder()
	h := &Handler{}
	h.apiImportApiKeysAdmin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("import: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("import unmarshal: %v", err)
	}
	return out
}

// TestExportIncludeSecretRoundTrip exports with the secret, wipes the store, then
// re-imports the exported JSON and confirms the raw keys are restored verbatim.
func TestExportIncludeSecretRoundTrip(t *testing.T) {
	mustInitConfig(t)
	seedExportKeys(t)

	// Masked export must never carry the raw key.
	masked := exportRaw(t, false)
	mb, _ := json.Marshal(masked)
	if strings.Contains(string(mb), "sk-unlimited-secret") {
		t.Fatalf("masked export leaked raw key: %s", mb)
	}

	// includeSecret export carries the raw key in the "key" field.
	full := exportRaw(t, true)
	fb, _ := json.Marshal(full)
	if !strings.Contains(string(fb), "sk-unlimited-secret") {
		t.Fatalf("includeSecret export missing raw key: %s", fb)
	}

	// Re-import the same JSON into a fresh store.
	mustInitConfig(t)
	if len(config.ListApiKeys()) != 0 {
		t.Fatalf("expected empty store after re-init")
	}
	res := importBody(t, string(fb))
	if res["imported"].(float64) != 4 {
		t.Fatalf("expected 4 imported, got %v", res["imported"])
	}

	keys := make(map[string]config.ApiKeyEntry)
	for _, e := range config.ListApiKeys() {
		keys[e.Key] = e
	}
	if _, ok := keys["sk-unlimited-secret"]; !ok {
		t.Fatalf("raw key not restored: %+v", keys)
	}
	// Usage counters preserved (under key had 500 tokens / 5 credits).
	under, ok := keys["sk-under-secret"]
	if !ok {
		t.Fatalf("under key missing")
	}
	if under.TokensUsed != 500 || under.CreditsUsed != 5 {
		t.Fatalf("usage not preserved: %+v", under)
	}
	if under.TokenLimit != 1000 || under.CreditLimit != 10 {
		t.Fatalf("limits not preserved: %+v", under)
	}
}

// TestImportSkipsMaskedAndDuplicates verifies masked-only entries and duplicates
// are skipped rather than creating garbage keys.
func TestImportSkipsMaskedAndDuplicates(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "existing", Key: "sk-existing-key", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := `{"apiKeys":[
		{"name":"masked","key":"sk-***abc","enabled":true},
		{"name":"dup","key":"sk-existing-key","enabled":true},
		{"name":"empty","key":"","enabled":true},
		{"name":"fresh","key":"sk-fresh-key","enabled":true}
	]}`
	res := importBody(t, body)
	if res["imported"].(float64) != 1 {
		t.Fatalf("expected 1 imported, got %v", res["imported"])
	}
	if res["skipped"].(float64) != 3 {
		t.Fatalf("expected 3 skipped, got %v", res["skipped"])
	}
}

// TestImportBareArray accepts a top-level array (not just the wrapper).
func TestImportBareArray(t *testing.T) {
	mustInitConfig(t)
	body := `[{"name":"a","key":"sk-a-key","enabled":true}]`
	res := importBody(t, body)
	if res["imported"].(float64) != 1 {
		t.Fatalf("expected 1 imported, got %v", res["imported"])
	}
}
