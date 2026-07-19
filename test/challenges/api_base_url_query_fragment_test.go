package challenges

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vasic-digital/claude-code-router/internal/config"
	"github.com/vasic-digital/claude-code-router/internal/proxy"
)

func init() {
	registerChallenge(ChallengeMeta{
		ID:       "api-base-url-query-fragment",
		TestName: "TestChallenge_APIBaseURLWithQueryStringAndFragment",
		Hypothesis: "config.Validate only checks the http(s):// prefix, so an api_base_url " +
			"carrying a query string (e.g. Azure-style ?api-version=...) or a #fragment must " +
			"still validate; proxy.Client.Do must send the query string to the upstream verbatim " +
			"(providers legitimately use it), while a fragment -- which HTTP defines as " +
			"client-side-only and never transmitted -- is correctly NOT sent over the wire.",
		ExpectedSafeOutcome: "No panic building the request; the fake upstream observes the exact " +
			"query string; the fragment never reaches the upstream at all (standard HTTP " +
			"behaviour, not a bug).",
	})
}

func TestChallenge_APIBaseURLWithQueryStringAndFragment(t *testing.T) {
	var gotPath, gotQuery, gotRawURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotRawURI = r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	base := upstream.URL + "/v1/chat/completions?api-version=2024-05-01&api-key=redacted-not-a-real-secret#ignored-client-side-fragment"
	provider := &config.Provider{Name: "azure-like", APIBaseURL: base, APIKey: "k"}

	cfg := &config.Config{Providers: []config.Provider{*provider}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected an api_base_url with a query string and fragment: %v", err)
	}

	client := proxy.New(5 * time.Second)
	resp, err := client.Do(t.Context(), provider, []byte(`{}`), false)
	if err != nil {
		t.Fatalf("proxy.Client.Do failed on a query+fragment URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream status = %d, want 200", resp.StatusCode)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream saw path %q, want /v1/chat/completions", gotPath)
	}
	if gotQuery != "api-version=2024-05-01&api-key=redacted-not-a-real-secret" {
		t.Errorf("upstream saw query %q, want the full query string preserved", gotQuery)
	}
	if containsFragmentMarker(gotRawURI) {
		t.Errorf("the fragment leaked onto the wire in the raw request URI %q -- HTTP must never send it", gotRawURI)
	}
	t.Logf("safe: query string reached the upstream verbatim (%q); fragment correctly never sent (raw request-URI: %q)", gotQuery, gotRawURI)
}

func containsFragmentMarker(rawURI string) bool {
	for _, r := range rawURI {
		if r == '#' {
			return true
		}
	}
	return false
}
