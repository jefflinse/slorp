package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	const secret = "s3cr3t-sentinel"
	for in, says := range map[string]string{
		"ftp://" + secret + "@localhost:21":    "http or https",
		"http://user:" + secret + "@localhost": "user name or password",
		"http://local host/" + secret:          "not a URL",
		"http://" + secret + ":99999999":       "port",
		"http:///" + secret:                    "no host",
		"http://localhost/" + secret + "%zz":   "not a URL",
	} {
		_, err := (Startup{URL: in, URLGiven: true}).Checked()
		if err == nil || !strings.Contains(err.Error(), says) || strings.Contains(err.Error(), secret) {
			t.Errorf("starting with %q = %v, want an error saying it %s without repeating it", in, err, says)
		}
	}
}

// The address kept is the one the user meant: what is escaped in its path
// stays escaped in every request, and its host can be read back.
func TestAModelServerAddressKeepsWhatIsEscapedInIt(t *testing.T) {
	asked := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.RequestURI
		io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	e := &Enricher{}
	for _, path := range []string{"/model%23blue", "/team%2Fmodel", "/100%25", "/a%20b/v1/"} {
		base, ok := ServerURL(srv.URL + path)
		if !ok {
			t.Errorf("%s was refused", path)
			continue
		}
		if err := e.get(context.Background(), base, "/v1/models", &struct{}{}); err != nil {
			t.Errorf("listing from %s: %v", base, err)
			continue
		}
		want := strings.TrimSuffix(strings.TrimSuffix(path, "/"), "/v1") + "/v1/models"
		if got := <-asked; got != want {
			t.Errorf("the server at %s was asked for %s, want %s", path, got, want)
		}
	}

	base, ok := ServerURL("http://[fe80::1%25lo0]:8080/v1")
	u, err := url.Parse(base + "/v1/models")
	if !ok || err != nil || u.Hostname() != "fe80::1%lo0" || u.Port() != "8080" {
		t.Errorf("a scoped IPv6 address was kept as %q, which reads back as %v, %v", base, u, err)
	}
	if _, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil); err != nil {
		t.Errorf("a request to the kept scoped IPv6 address cannot be made: %v", err)
	}
}

func TestARedirectIsFollowedOnlyWithinTheOriginFirstAsked(t *testing.T) {
	request := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tt := range []struct {
		from, to string
		follows  bool
	}{
		{"http://localhost:1234/v1/models", "http://localhost:1234/v1/models/", true},
		{"https://MODEL.example/v1/models", "https://model.example/v1/models", true},
		{"https://localhost:443/v1/models", "https://localhost/v1/models", true},
		{"https://localhost/v1/models", "https://localhost:443/v1/models", true},
		{"http://model.example/v1/models", "http://model.example:80/v1/models", true},
		{"http://[::1]:80/v1/models", "http://[::1]/v1/models", true},
		{"http://model.example/v1/models", "https://model.example/v1/models", false},
		{"https://model.example/v1/models", "http://model.example:443/v1/models", false},
		{"http://localhost:1234/v1/models", "http://localhost:1235/v1/models", false},
		{"https://localhost:8443/v1/models", "https://localhost/v1/models", false},
		{"http://model.example/v1/models", "http://other.example/v1/models", false},
		{"http://localhost:1234/v1/models", "http://127.0.0.1:1234/v1/models", false},
	} {
		err := sameServer(request(tt.to), []*http.Request{request(tt.from)})
		if (err == nil) != tt.follows {
			t.Errorf("a redirect from %s to %s = %v, want followed %v", tt.from, tt.to, err, tt.follows)
		}
	}
	first := request("http://localhost:1234/a")
	via := []*http.Request{first}
	for len(via) < 10 {
		via = append(via, request("http://localhost:1234/b"))
	}
	if err := sameServer(request("http://localhost:1234/c"), via); err == nil {
		t.Error("an eleventh redirect within the model server was followed")
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
