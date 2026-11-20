package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestResumeRecoversAuthoritativeForkChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_fork" {
			t.Error(r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"id":"ses_fork","agent":"review","model":{"providerID":"provider","id":"org/model","variant":"high"},"location":{"directory":"/project"},"time":{"created":200,"updated":300}}}`)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name, agent                       string
		model                             *nativeModel
		wantAgent, wantModel, wantVariant string
	}{
		{"missing both", "", nil, "review", "org/model", "high"},
		{"stale agent", "custom", nil, "review", "org/model", "high"},
		{"stale model", "", &nativeModel{ProviderID: "explicit", ID: "chosen", Variant: "default"}, "review", "org/model", "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &runtime{base: srv.URL, dir: t.TempDir(), client: srv.Client(), session: nativeSession{ID: "ses_fork", Agent: tc.agent, Model: tc.model}}
			if err := r.restoreChoices(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.session.Agent != tc.wantAgent || r.session.Model.ID != tc.wantModel || r.session.Model.Variant != tc.wantVariant {
				t.Fatalf("choices: %+v %+v", r.session, r.session.Model)
			}
		})
	}
}

func TestResumeChoiceFetchErrorsPropagate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	r := &runtime{base: srv.URL, dir: t.TempDir(), client: srv.Client(), session: nativeSession{ID: "ses_fork"}}
	if err := r.restoreChoices(context.Background()); err == nil || !strings.Contains(err.Error(), "recovering") {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAcceptedPromptIsNotFailedByLaterContextCancellation(t *testing.T) {
	a := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &runtime{base: "http://127.0.0.1", dir: a.Home, private: t.TempDir(), done: make(chan struct{}), token: "token", job: "job", session: nativeSession{ID: "ses_accepted"}}
	r.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if err := a.activate(ctx, r, "accepted"); err != nil {
		t.Fatalf("accepted prompt reported as failure: %v", err)
	}
	a.Hub.CloseManaged("token")
}

func TestNativeWriteResponseWithheldIsUnknown(t *testing.T) {
	for _, path := range []string{"/api/session", "/api/session/ses_known/prompt"} {
		t.Run(path, func(t *testing.T) {
			recorded := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(recorded)
				<-r.Context().Done()
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &runtime{base: srv.URL, dir: t.TempDir(), client: srv.Client()}
			done := make(chan error, 1)
			go func() {
				done <- r.request(ctx, http.MethodPost, path, map[string]string{"prompt": "recorded once"}, nil)
			}()
			select {
			case <-recorded:
			case <-time.After(time.Second):
				t.Fatal("request not recorded")
			}
			cancel()
			if err := <-done; !errors.Is(err, hub.ErrLifecycleUnknown) || !errors.Is(err, context.Canceled) {
				t.Fatalf("ambiguous write: %v", err)
			}
		})
	}
}

func TestNativeClientDeadlineAndTruncatedCreationAreUnknown(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if headers {
					_, _ = io.WriteString(w, `{"id":"ses_`)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer srv.Close()
			client := srv.Client()
			client.Timeout = 40 * time.Millisecond
			r := &runtime{base: srv.URL, dir: t.TempDir(), client: client}
			var created nativeSession
			err := r.request(context.Background(), http.MethodPost, "/api/session", map[string]string{"title": "created once"}, &created)
			if !errors.Is(err, hub.ErrLifecycleUnknown) || !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), srv.URL) {
				t.Fatal(err)
			}
		})
	}
}

type notifiedBody struct {
	io.ReadCloser
	started chan struct{}
}

func (b *notifiedBody) Read(p []byte) (int, error) {
	if b.started != nil {
		close(b.started)
		b.started = nil
	}
	return b.ReadCloser.Read(p)
}

func TestPromptBodyFailuresPreserveAdmissionUncertainty(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, `{"accepted":`)
				w.(http.Flusher).Flush()
				if cancelled {
					<-r.Context().Done()
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			client := srv.Client()
			transport := client.Transport
			client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				res, err := transport.RoundTrip(req)
				if err == nil {
					res.Body = &notifiedBody{ReadCloser: res.Body, started: started}
				}
				return res, err
			})
			r := &runtime{base: srv.URL, dir: t.TempDir(), client: client}
			done := make(chan error, 1)
			go func() {
				done <- r.request(ctx, http.MethodPost, "/api/session/ses_known/prompt", map[string]string{"text": "admitted once"}, nil)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("response body was not read")
			}
			cause := error(io.ErrUnexpectedEOF)
			if cancelled {
				cancel()
				cause = context.Canceled
			}
			if err := <-done; !errors.Is(err, hub.ErrLifecycleUnknown) || !errors.Is(err, cause) {
				t.Fatalf("admission uncertainty or cause lost: %v", err)
			}
		})
	}
}
