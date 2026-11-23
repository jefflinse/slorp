package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  http://localhost:1234  ", "http://localhost:1234", true},
		{"http://localhost:11434/v1", "http://localhost:11434", true},
		{"http://localhost:11434/v1/", "http://localhost:11434", true},
		{"localhost:8080", "http://localhost:8080", true},
		{"https://models.example/openai/v1?key=1#top", "https://models.example/openai", true},
		{"http://127.0.0.1:1234/", "http://127.0.0.1:1234", true},
		{"ftp://localhost:21", "", false},
		{"http://", "", false},
		{"http://user:secret@localhost:1234", "", false},
		{"http://local host", "", false},
	}
	for _, tt := range tests {
		if got, ok := ServerURL(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("ServerURL(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestStartupAddressesAreCheckedAsSettingsAre(t *testing.T) {
	for in, want := range map[string]string{"": "", "localhost:1234/v1/": "http://localhost:1234", DefaultURL: DefaultURL} {
		got, err := Startup{URL: in, URLGiven: true}.Checked()
		if err != nil || got.URL != want {
			t.Errorf("starting with %q = %q, %v; want %q", in, got.URL, err, want)
		}
	}
	for _, in := range []string{"ftp://localhost:21", "http://local host", "http://user:secret@localhost"} {
		if _, err := (Startup{URL: in, URLGiven: true}).Checked(); err == nil || !strings.Contains(err.Error(), in) {
			t.Errorf("starting with %q = %v, want an error naming it", in, err)
		}
	}
}

func TestRequestsGoNowhereButTheModelServer(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		io.WriteString(w, `{"data":[],"choices":[{"message":{"content":"stolen"}}]}`)
	}))
	t.Cleanup(other.Close)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		case "/v1/models":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		case "/here/v1/models":
			http.Redirect(w, r, "/v1/models/", http.StatusFound)
		case "/v1/models/":
			io.WriteString(w, `{"data":[{"id":"m"}]}`)
		}
	}))
	t.Cleanup(model.Close)
	e := &Enricher{}
	ctx := context.Background()

	if _, err := e.chat(ctx, model.URL, map[string]any{"model": "m", "messages": []map[string]string{{"role": "user", "content": "private"}}}); err == nil {
		t.Error("a completion redirected to another server succeeded")
	}
	if err := e.get(ctx, model.URL, "/v1/models", &struct{}{}); err == nil {
		t.Error("a listing redirected to another server succeeded")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the other server was sent %d requests, want none", n)
	}
	var listed struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := e.get(ctx, model.URL+"/here", "/v1/models", &listed); err != nil || len(listed.Data) != 1 {
		t.Errorf("a listing redirected within the model server = %+v, %v; want it followed", listed, err)
	}
}

// A server address kept before Settings checked it fails each request rather
// than the dashboard.
func TestARequestToAnUnusableAddressFailsWithoutPanicking(t *testing.T) {
	e := &Enricher{}
	for _, base := range []string{"http://local host", "http://[::1", "\x7f"} {
		if err := e.get(context.Background(), base, "/v1/models", &struct{}{}); err == nil {
			t.Errorf("GET from %q succeeded", base)
		}
		if _, err := e.chat(context.Background(), base, map[string]any{"model": "m"}); err == nil {
			t.Errorf("chat with %q succeeded", base)
		}
	}
}
