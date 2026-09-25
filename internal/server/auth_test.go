package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kawai/px/internal/controller"
	"github.com/kawai/px/internal/store"
)

const testToken = "sekrit-token"

func newAuthedTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/px.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctl := controller.New(st, nopProv{}, slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(RequireBearer(testToken,
		New(st, ctl, nopProv{}, slog.New(slog.DiscardHandler)).Handler()))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequireBearerRejectsMissingAndWrong(t *testing.T) {
	srv := newAuthedTestServer(t)
	for _, tc := range []struct{ name, header string }{
		{"no header", ""},
		{"wrong scheme", "Basic " + testToken},
		{"wrong token", "Bearer nope"},
		{"prefix only", "Bearer"},
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/tasks", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d: %s", tc.name, resp.StatusCode, body)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("%s: missing WWW-Authenticate", tc.name)
		}
	}
}

func TestRequireBearerAcceptsValidToken(t *testing.T) {
	srv := newAuthedTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestRequireBearerHealthzStaysOpen(t *testing.T) {
	srv := newAuthedTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("healthz must not require the token, got %d: %s", resp.StatusCode, body)
	}
}

// An empty token would match any "Bearer " prefix; the wrapper must refuse
// to install itself and pass traffic through unchanged instead.
func TestRequireBearerEmptyTokenIsNoOp(t *testing.T) {
	var called bool
	h := RequireBearer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("empty token must not install auth")
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return string(data)
}
