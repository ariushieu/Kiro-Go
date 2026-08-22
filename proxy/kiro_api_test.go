package proxy

import (
	"io"
	"kiro-go/config"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveProfileArnReturnsCachedValueWithoutRequest(t *testing.T) {
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP request for cached profile ARN")
			return nil, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	account := &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/test "}
	got, err := ResolveProfileArn(account)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/test" {
		t.Fatalf("expected trimmed cached ARN, got %q", got)
	}
}

func TestResolveProfileArnFetchesAndCachesProfile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:           "acct-1",
		Email:        "user@example.com",
		AccessToken:  "access-token",
		Region:       "us-east-1",
		UsageCurrent: 7,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", req.Method)
			}
			if req.URL.Path != "/ListAvailableProfiles" {
				t.Fatalf("expected ListAvailableProfiles path, got %s", req.URL.Path)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("expected JSON content type, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"profiles":[{"arn":" arn:aws:codewhisperer:profile/fetched "}]} `)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	requestAccount := account
	requestAccount.UsageCurrent = 0
	got, err := ResolveProfileArn(&requestAccount)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "arn:aws:codewhisperer:profile/fetched" {
		t.Fatalf("expected fetched ARN, got %q", got)
	}
	if requestAccount.ProfileArn != got {
		t.Fatalf("expected account to be updated with fetched ARN, got %q", requestAccount.ProfileArn)
	}

	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected one persisted account, got %d", len(accounts))
	}
	if accounts[0].ProfileArn != got {
		t.Fatalf("expected persisted account profile ARN %q, got %q", got, accounts[0].ProfileArn)
	}
	if accounts[0].UsageCurrent != 7 {
		t.Fatalf("expected profile cache update to preserve usage fields, got usageCurrent=%v", accounts[0].UsageCurrent)
	}
}

func TestListAvailableModelsFetchesAllPages(t *testing.T) {
	account := &config.Account{
		ID:          "models-pages",
		AccessToken: "access-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:1:profile/test",
		Region:      "us-east-1",
	}
	requests := 0
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.URL.Path != "/ListAvailableModels" {
				t.Fatalf("unexpected path %s", req.URL.Path)
			}
			body := `{"models":[{"modelId":"first"},{"modelId":"shared"}],"nextToken":"page-2"}`
			if token := req.URL.Query().Get("nextToken"); token != "" {
				if token != "page-2" {
					t.Fatalf("unexpected nextToken %q", token)
				}
				body = `{"models":[{"modelId":"shared"},{"modelId":"second"}]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	models, err := ListAvailableModels(account)
	if err != nil {
		t.Fatalf("list models: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected two pages, got %d requests", requests)
	}
	if got := modelIDsFromInfo(models); strings.Join(got, ",") != "first,shared,second" {
		t.Fatalf("unexpected merged models: %v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestRegionFromProfileArn(t *testing.T) {
	cases := []struct {
		name string
		arn  string
		want string
	}{
		{"eu-central-1", "arn:aws:codewhisperer:eu-central-1:574548986629:profile/GHCRRAV4HW3E", "eu-central-1"},
		{"us-east-1", "arn:aws:codewhisperer:us-east-1:699475941385:profile/ABC", "us-east-1"},
		{"trimmed", "  arn:aws:codewhisperer:us-east-1:1:profile/X  ", "us-east-1"},
		{"empty", "", ""},
		{"malformed", "not-an-arn", ""},
		{"wrong-service", "arn:aws:iam:us-east-1:1:role/foo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := regionFromProfileArn(tc.arn); got != tc.want {
				t.Fatalf("regionFromProfileArn(%q) = %q, want %q", tc.arn, got, tc.want)
			}
		})
	}
}

func TestKiroRegionPrefersProfileArnRegion(t *testing.T) {
	// Auth/SSO region is us-east-1 but the profile lives in eu-central-1.
	// Data-plane calls must follow the profile ARN's region.
	account := &config.Account{
		Region:     "us-east-1",
		ProfileArn: "arn:aws:codewhisperer:eu-central-1:574548986629:profile/GHCRRAV4HW3E",
	}
	if got := kiroRegion(account); got != "eu-central-1" {
		t.Fatalf("kiroRegion = %q, want eu-central-1 (from profile ARN)", got)
	}
}

func TestProfileProbeRegionsOrderingAndDedup(t *testing.T) {
	// Configured region should come first, then the remaining supported regions
	// without duplicates.
	account := &config.Account{ApiRegion: "eu-central-1"}
	got := profileProbeRegions(account)
	if len(got) == 0 || got[0] != "eu-central-1" {
		t.Fatalf("expected configured region first, got %v", got)
	}
	seen := map[string]int{}
	for _, r := range got {
		seen[r]++
		if seen[r] > 1 {
			t.Fatalf("region %q duplicated in probe list %v", r, got)
		}
	}
	// us-east-1 must still be probed even though it is not the configured region.
	if seen["us-east-1"] == 0 {
		t.Fatalf("expected us-east-1 to be probed, got %v", got)
	}
}

func TestRegionalizeURLForRegion(t *testing.T) {
	base := "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableProfiles"
	if got := regionalizeURLForRegion(base, "us-east-1"); got != base {
		t.Fatalf("us-east-1 should be a no-op, got %q", got)
	}
	if got := regionalizeURLForRegion(base, ""); got != base {
		t.Fatalf("empty region should be a no-op, got %q", got)
	}
	want := "https://q.eu-central-1.amazonaws.com/ListAvailableProfiles"
	if got := regionalizeURLForRegion(base, "eu-central-1"); got != want {
		t.Fatalf("regionalizeURLForRegion eu-central-1 = %q, want %q", got, want)
	}
}

// TestResolveProfileArnProbesSecondaryRegion verifies the cross-region fix:
// when the configured region returns an empty profile list, resolution probes
// the other supported Kiro region, and on success persists both the ARN and the
// region where it was found (without clobbering the auth region).
func TestResolveProfileArnProbesSecondaryRegion(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(configPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID:          "acct-xregion",
		Email:       "user@example.com",
		AccessToken: "access-token",
		Region:      "us-east-1", // auth/SSO region
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	const euARN = "arn:aws:codewhisperer:eu-central-1:574548986629:profile/GHCRRAV4HW3E"
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"profiles":[]}`
			// us-east-1 host returns empty; eu-central-1 (q.*) returns the profile.
			if strings.Contains(req.URL.Host, "eu-central-1") {
				body = `{"profiles":[{"arn":"` + euARN + `"}]}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	})
	t.Cleanup(func() { InitKiroHttpClient("") })

	req := account
	got, err := ResolveProfileArn(&req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != euARN {
		t.Fatalf("expected eu-central-1 ARN, got %q", got)
	}

	persisted := config.GetAccounts()[0]
	if persisted.ProfileArn != euARN {
		t.Fatalf("expected persisted ARN %q, got %q", euARN, persisted.ProfileArn)
	}
	if persisted.ApiRegion != "eu-central-1" {
		t.Fatalf("expected ApiRegion pinned to eu-central-1, got %q", persisted.ApiRegion)
	}
	if persisted.Region != "us-east-1" {
		t.Fatalf("auth region must be preserved as us-east-1, got %q", persisted.Region)
	}
}
