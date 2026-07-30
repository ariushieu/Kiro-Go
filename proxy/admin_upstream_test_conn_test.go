package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
	"kiro-go/pool"
)

// newUpstreamTestHandler builds the minimal Handler the admin route needs.
func newUpstreamTestHandler(t *testing.T) *Handler {
	t.Helper()
	config.SetPassword("s3cret")
	return &Handler{
		pool:          pool.GetPool(),
		adminGuard:    newAdminAuthGuard(10, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
	}
}

func postUpstreamTest(t *testing.T, h *Handler, body string) (int, upstreamTestResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		config.GetAdminPath()+"/api/upstream/test", strings.NewReader(body))
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	var out upstreamTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response (%d): %v; body=%s", w.Code, err, w.Body.String())
	}
	return w.Code, out
}

func stepStatus(resp upstreamTestResponse, step string) string {
	for _, s := range resp.Steps {
		if s.Step == step {
			return s.Status
		}
	}
	return ""
}

// fakeUpstream serves the two endpoints the probe uses. models==nil makes
// /v1/models 404, mimicking a gateway that does not implement it.
func fakeUpstream(t *testing.T, wantKey string, models []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		xkey := r.Header.Get("x-api-key")
		if auth != "Bearer "+wantKey && xkey != wantKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			if models == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			payload := map[string]interface{}{"data": []map[string]string{}}
			list := make([]map[string]string, 0, len(models))
			for _, m := range models {
				list = append(list, map[string]string{"id": m})
			}
			payload["data"] = list
			_ = json.NewEncoder(w).Encode(payload)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages"):
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUpstreamTestHappyPath: both steps pass and the model list comes back so the
// UI can offer to fill it in.
func TestUpstreamTestHappyPath(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	srv := fakeUpstream(t, "sk-good", []string{"gpt-4.1", "gpt-4.1-mini"})

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(
		`{"baseURL":%q,"apiKey":"sk-good","apiFormat":"openai","model":"gpt-4.1"}`, srv.URL))

	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %+v", resp)
	}
	if got := stepStatus(resp, "listModels"); got != "ok" {
		t.Errorf("listModels = %q, want ok", got)
	}
	if got := stepStatus(resp, "chat"); got != "ok" {
		t.Errorf("chat = %q, want ok", got)
	}
	if len(resp.Models) != 2 {
		t.Errorf("models = %v, want 2 entries", resp.Models)
	}
	if resp.Reply != "ok" {
		t.Errorf("reply = %q, want \"ok\"", resp.Reply)
	}
}

// TestUpstreamTestAnthropicFormat exercises the x-api-key/messages branch.
func TestUpstreamTestAnthropicFormat(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	srv := fakeUpstream(t, "sk-ant", []string{"claude-sonnet-4"})

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(
		`{"baseURL":%q,"apiKey":"sk-ant","apiFormat":"anthropic","model":"claude-sonnet-4"}`, srv.URL))

	if code != http.StatusOK || !resp.Success {
		t.Fatalf("anthropic probe failed: %d %+v", code, resp)
	}
	if resp.Reply != "ok" {
		t.Errorf("reply = %q", resp.Reply)
	}
}

// TestUpstreamTestMissingModelsEndpointStillPasses: a gateway without /v1/models
// must report the step as skipped, not fail the whole test.
func TestUpstreamTestMissingModelsEndpointStillPasses(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	srv := fakeUpstream(t, "sk-good", nil)

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(
		`{"baseURL":%q,"apiKey":"sk-good","apiFormat":"openai","model":"gpt-4.1"}`, srv.URL))

	if code != http.StatusOK || !resp.Success {
		t.Fatalf("expected success despite missing /v1/models: %d %+v", code, resp)
	}
	if got := stepStatus(resp, "listModels"); got != "skipped" {
		t.Errorf("listModels = %q, want skipped", got)
	}
	if got := stepStatus(resp, "chat"); got != "ok" {
		t.Errorf("chat = %q, want ok", got)
	}
}

// TestUpstreamTestWrongKeyFails reports failure in the body with HTTP 200, so the
// panel can render per-step detail rather than a bare error status.
func TestUpstreamTestWrongKeyFails(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	srv := fakeUpstream(t, "sk-good", []string{"gpt-4.1"})

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(
		`{"baseURL":%q,"apiKey":"sk-wrong","apiFormat":"openai","model":"gpt-4.1"}`, srv.URL))

	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with failure in body", code)
	}
	if resp.Success {
		t.Fatal("a rejected key must not report success")
	}
	if got := stepStatus(resp, "chat"); got != "failed" {
		t.Errorf("chat = %q, want failed", got)
	}
	// The admin needs the provider's own wording to fix their config.
	if !strings.Contains(resp.Error, "invalid api key") {
		t.Errorf("error should carry the upstream message, got %q", resp.Error)
	}
}

// TestUpstreamTestBlankKeyReusesStored is the whole point of the masked-key UI:
// re-testing a saved account works without the browser ever holding the secret.
func TestUpstreamTestBlankKeyReusesStored(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	srv := fakeUpstream(t, "sk-stored", []string{"gpt-4.1"})

	const accID = "stored-upstream"
	if err := config.AddAccount(config.Account{
		ID: accID, Email: "stored@example.com",
		Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatOpenAI,
		BaseURL: srv.URL + "/v1", ApiKey: "sk-stored",
		Models: []string{"gpt-4.1"}, Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(`{"accountId":%q}`, accID))
	if code != http.StatusOK || !resp.Success {
		t.Fatalf("stored-key probe failed: %d %+v", code, resp)
	}
	if resp.Reply != "ok" {
		t.Errorf("reply = %q", resp.Reply)
	}
}

// TestUpstreamTestRejectsPlainHTTP keeps this endpoint from being used as an SSRF
// probe: it must enforce the same https-only rule (loopback excepted) as account
// creation, and reject before making any request.
func TestUpstreamTestRejectsPlainHTTP(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer srv.Close()

	code, resp := postUpstreamTest(t, h,
		`{"baseURL":"http://169.254.169.254/latest/meta-data","apiKey":"x","apiFormat":"openai","model":"m"}`)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for plain-http non-loopback", code)
	}
	if !strings.Contains(strings.ToLower(resp.Error), "https") {
		t.Errorf("error should explain the https rule, got %q", resp.Error)
	}
	if reached {
		t.Error("no request should have been made")
	}
}

// TestUpstreamTestRequiresKey: with no stored account and no key in the payload
// there is nothing to authenticate with.
func TestUpstreamTestRequiresKey(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	code, resp := postUpstreamTest(t, h,
		`{"baseURL":"https://api.example.com/v1","apiFormat":"openai","model":"m"}`)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(resp.Error, "apiKey") {
		t.Errorf("error = %q, want it to mention apiKey", resp.Error)
	}
}

// TestUpstreamTestUnknownAccount must 404-style reject rather than silently probe
// with an empty config.
func TestUpstreamTestUnknownAccount(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	code, resp := postUpstreamTest(t, h, `{"accountId":"does-not-exist"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Errorf("error = %q", resp.Error)
	}
}

// TestUpstreamTestFallsBackToConfiguredModel: with no model in the request the
// probe uses one the admin configured on the account.
func TestUpstreamTestFallsBackToConfiguredModel(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	const accID = "fallback-upstream"
	if err := config.AddAccount(config.Account{
		ID: accID, Email: "fallback@example.com",
		Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatOpenAI,
		BaseURL: srv.URL + "/v1", ApiKey: "sk-x",
		Models: []string{"my-configured-model"}, Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	code, resp := postUpstreamTest(t, h, fmt.Sprintf(`{"accountId":%q}`, accID))
	if code != http.StatusOK || !resp.Success {
		t.Fatalf("probe failed: %d %+v", code, resp)
	}
	if gotModel != "my-configured-model" {
		t.Errorf("probed model = %q, want the configured one", gotModel)
	}
}

// TestUpstreamTestRouting guards the switch ordering in handleAdminAPI.
func TestUpstreamTestRouting(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	r := httptest.NewRequest(http.MethodPost,
		config.GetAdminPath()+"/api/upstream/test", strings.NewReader(`{}`))
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	// An empty body is a 400 from the handler, not a 404 from the router.
	if w.Code == http.StatusNotFound {
		t.Fatalf("route not reached: %d %s", w.Code, w.Body.String())
	}
}

// TestUpstreamTestRequiresAdminPassword: this endpoint dials arbitrary URLs with
// stored credentials, so it must never be reachable unauthenticated.
func TestUpstreamTestRequiresAdminPassword(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	r := httptest.NewRequest(http.MethodPost,
		config.GetAdminPath()+"/api/upstream/test",
		strings.NewReader(`{"baseURL":"https://api.example.com/v1","apiKey":"x","model":"m"}`))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without the admin password", w.Code)
	}
}

// TestAccountsResponseMasksUpstreamKey: the panel must receive only a mask, since
// blank-means-keep removes any need for the cleartext in the browser.
func TestAccountsResponseMasksUpstreamKey(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	const secret = "sk-upstream-do-not-leak-1234"
	if err := config.AddAccount(config.Account{
		ID: "masked-upstream", Email: "masked@example.com",
		Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatOpenAI,
		BaseURL: "https://api.example.com/v1", ApiKey: secret,
		Models: []string{"gpt-4.1"}, Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, config.GetAdminPath()+"/api/accounts", nil)
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatal("cleartext upstream apiKey leaked into the accounts response")
	}
	if !strings.Contains(body, "apiKeyMasked") {
		t.Fatalf("expected apiKeyMasked in the response, got %s", body)
	}
}

// TestUpdateAccountKeepsKeyWhenBlank is the server half of the blank-means-keep
// contract the detail modal relies on.
func TestUpdateAccountKeepsKeyWhenBlank(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	const accID = "edit-upstream"
	if err := config.AddAccount(config.Account{
		ID: accID, Email: "edit@example.com",
		Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatOpenAI,
		BaseURL: "https://old.example.com/v1", ApiKey: "sk-original",
		Models: []string{"old-model"}, Enabled: true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	r := httptest.NewRequest(http.MethodPut, config.GetAdminPath()+"/api/accounts/"+accID,
		strings.NewReader(`{"baseURL":"https://new.example.com/v1","apiKey":"","models":["new-a","new-b"]}`))
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT failed: %d %s", w.Code, w.Body.String())
	}

	var updated *config.Account
	for _, a := range config.GetAccounts() {
		if a.ID == accID {
			copied := a
			updated = &copied
		}
	}
	if updated == nil {
		t.Fatal("account vanished")
	}
	if updated.ApiKey != "sk-original" {
		t.Errorf("blank apiKey must keep the stored one, got %q", updated.ApiKey)
	}
	if updated.BaseURL != "https://new.example.com/v1" {
		t.Errorf("baseURL not updated: %q", updated.BaseURL)
	}
	if strings.Join(updated.Models, ",") != "new-a,new-b" {
		t.Errorf("models not updated: %v", updated.Models)
	}
}
