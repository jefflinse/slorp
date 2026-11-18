package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

// fakeLLM stands in for an OpenAI-compatible server. It answers every chat
// request with content and remembers the last request it was sent.
type fakeLLM struct {
	content string
	status  int
	models  string
	// listed is what the server lists as an OpenAI-compatible one does,
	// empty for a server with no such listing.
	listed string
	// calls is what the model asks to look up when it is offered tools.
	calls []call
	// plan is the request that offered tools, when there was one.
	plan struct {
		Messages []struct{ Role, Content string }
		Tools    []struct {
			Function struct {
				Name       string
				Parameters json.RawMessage
			}
		}
	}
	requests int
	request  struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		Messages        []struct{ Role, Content string }
		ResponseFormat  struct {
			Type       string
			JSONSchema struct {
				Strict bool
				Schema json.RawMessage
			} `json:"json_schema"`
		} `json:"response_format"`
	}
}

func (f *fakeLLM) enricher(t *testing.T) *Enricher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/models":
			if f.models == "" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, f.models)
		case "/v1/models":
			if f.listed == "" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, f.listed)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			f.requests++
			if strings.Contains(string(body), `"tools"`) {
				if err := json.Unmarshal(body, &f.plan); err != nil {
					t.Errorf("planning request is not JSON: %v", err)
				}
				var calls []any
				for _, c := range f.calls {
					calls = append(calls, map[string]any{"type": "function", "function": map[string]string{"name": c.name, "arguments": c.args}})
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "", "tool_calls": calls}}}})
				return
			}
			if err := json.Unmarshal(body, &f.request); err != nil {
				t.Errorf("request is not JSON: %v", err)
			}
			if f.status != 0 {
				w.WriteHeader(f.status)
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": f.content}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Enricher{BaseURL: srv.URL, client: srv.Client(), failed: map[string]time.Time{}}
}

type call struct{ name, args string }

func (f *fakeLLM) user() string   { return f.request.Messages[len(f.request.Messages)-1].Content }
func (f *fakeLLM) system() string { return f.request.Messages[0].Content }

var ctx = context.Background()

func TestModelUsesWhatIsAlreadyLoaded(t *testing.T) {
	cold := `{"data":[{"id":"cold","type":"llm","state":"not-loaded"}]}`
	tests := []struct {
		name, models, listed, configured string
		want, wantErr                    string
	}{
		{"a loaded chat model", `{"data":[
			{"id":"embed","type":"embeddings","state":"loaded"},
			{"id":"cold","type":"llm","state":"not-loaded"},
			{"id":"warm","type":"vlm","state":"loaded"}]}`, "", "", "warm", ""},
		{"nothing loaded", cold, "", "", "", "no model is loaded"},
		{"nothing loaded, though the server lists the model too", cold, `{"data":[{"id":"cold"}]}`, "", "", "no model is loaded"},
		{"only an embedding model loaded", `{"data":[{"id":"embed","type":"embeddings","state":"loaded"}]}`, "", "", "", "no model is loaded"},
		{"a model the user named", `{"data":[]}`, "", "chosen", "chosen", ""},
		{"a server other than LM Studio with one model", "", `{"object":"list","data":[{"id":"only","object":"model"}]}`, "", "only", ""},
		{"such a server with several models", "", `{"data":[{"id":"one"},{"id":"two"}]}`, "", "", "name one"},
		{"such a server with none", "", `{"data":[]}`, "", "", "no model is loaded"},
		{"a server that lists models in neither way", `not json`, "", "", "", "unreachable"},
		{"a server that answers neither", "", "", "", "", "unreachable"},
		{"a model the user named, on a server that answers neither", "", "", "chosen", "", "unreachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := (&fakeLLM{models: tt.models, listed: tt.listed}).enricher(t)
			e.Model = tt.configured
			got, err := e.model(ctx)
			if got != tt.want || (err == nil) != (tt.wantErr == "") || err != nil && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("model = %q, err %v; want %q, error %q", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestSaysWhichModelIsLoadedWithNothingToDo(t *testing.T) {
	f := &fakeLLM{models: `{"data":[{"id":"warm","type":"llm","state":"loaded"}]}`}
	e := f.enricher(t)
	st := testdb.Open(t)
	e.Hub = hub.New(testdb.KB(t, st), st)
	said := func() hub.Summarizer { return e.Hub.State().Summarizer }

	e.step(ctx)
	if got := said(); got.Model != "warm" || got.Error != "" || got.URL != e.BaseURL || got.URL == "" {
		t.Fatalf("summarizer = %+v; want the loaded model named, and where it was found, though no session needs describing", got)
	}

	f.models = `{"data":[]}`
	e.step(ctx)
	if got := said(); got.Model != "warm" {
		t.Errorf("summarizer = %+v; the server was asked again at once", got)
	}
	e.checked = time.Now().Add(-idleCheck)
	e.step(ctx)
	if got := said(); got.Model != "" || got.Error == "" || got.URL != e.BaseURL {
		t.Errorf("summarizer = %+v; want it to say no model is loaded, and where it looked, once it looks again", got)
	}
	if f.requests != 0 {
		t.Errorf("%d requests to the model with nothing to do", f.requests)
	}
}

func TestStartupConfig(t *testing.T) {
	plain := Startup{URL: DefaultURL}
	tests := []struct {
		name    string
		started Startup
		chosen  hub.ModelSettings
		want    Config
	}{
		{"nothing chosen", plain, hub.ModelSettings{}, Config{URL: DefaultURL}},
		{"a server and model chosen", plain, hub.ModelSettings{URL: "http://box:8080", Name: "qwen"}, Config{URL: "http://box:8080", Model: "qwen"}},
		{"turned off", plain, hub.ModelSettings{URL: "http://box:8080", Off: true}, Config{Model: ""}},
		{"a server given at startup", Startup{URL: "http://given:1", URLGiven: true}, hub.ModelSettings{URL: "http://box:8080", Name: "qwen"},
			Config{URL: "http://given:1", Model: "qwen", FixedURL: true}},
		{"a server given at startup, and the model turned off since", Startup{URL: "http://given:1", URLGiven: true}, hub.ModelSettings{Off: true},
			Config{URL: "http://given:1", FixedURL: true}},
		{"started without one", Startup{URL: "", URLGiven: true}, hub.ModelSettings{URL: "http://box:8080"}, Config{FixedURL: true}},
		{"a model given at startup", Startup{URL: DefaultURL, Model: "given", ModelGiven: true}, hub.ModelSettings{URL: "http://box:8080", Name: "qwen"},
			Config{URL: "http://box:8080", Model: "given", FixedModel: true}},
	}
	for _, tt := range tests {
		if got := tt.started.Config(tt.chosen); got != tt.want {
			t.Errorf("%s: config = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestStepFollowsTheUsersSettings(t *testing.T) {
	first := (&fakeLLM{models: `{"data":[{"id":"warm","type":"llm","state":"loaded"}]}`}).enricher(t)
	second := (&fakeLLM{listed: `{"data":[{"id":"one"},{"id":"two"}]}`}).enricher(t)
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	e := &Enricher{Hub: h, Resolve: Startup{URL: first.BaseURL}.Config, failed: map[string]time.Time{}}
	said := func() hub.Summarizer { e.step(ctx); return h.State().Summarizer }
	choose := func(m hub.ModelSettings) {
		s := h.Settings()
		s.Model = m
		h.SetSettings(s)
	}

	if got := said(); got != (hub.Summarizer{Model: "warm", URL: first.BaseURL}) {
		t.Fatalf("summarizer = %+v, want the model loaded where Slorp was started with", got)
	}
	e.failed["s"] = time.Now()
	choose(hub.ModelSettings{URL: second.BaseURL})
	if got := said(); got.URL != second.BaseURL || got.Model != "" || !strings.Contains(got.Error, "name one") {
		t.Errorf("summarizer = %+v, want the server just chosen asked at once, and what it said", got)
	}
	if len(e.failed) != 0 {
		t.Error("what failed with the server before is still held back from the one chosen")
	}
	choose(hub.ModelSettings{URL: second.BaseURL, Name: "two"})
	if got := said(); got != (hub.Summarizer{Model: "two", Named: "two", URL: second.BaseURL}) {
		t.Errorf("summarizer = %+v, want the model named", got)
	}
	choose(hub.ModelSettings{URL: second.BaseURL, Name: "two", Off: true})
	if got := said(); got != (hub.Summarizer{Off: true, Named: "two"}) {
		t.Errorf("summarizer = %+v, want it off", got)
	}
	choose(hub.ModelSettings{})
	if got := said(); got.Model != "warm" || got.Off {
		t.Errorf("summarizer = %+v, want it back on with the server Slorp was started with", got)
	}

	fixed := &Enricher{Hub: h, Resolve: Startup{URL: first.BaseURL, URLGiven: true, Model: "given", ModelGiven: true}.Config, failed: map[string]time.Time{}}
	choose(hub.ModelSettings{URL: second.BaseURL, Name: "two"})
	fixed.step(ctx)
	if got := h.State().Summarizer; got != (hub.Summarizer{Model: "given", Named: "given", URL: first.BaseURL, FixedURL: true, FixedModel: true}) {
		t.Errorf("summarizer = %+v, want what was given at startup, marked as fixed", got)
	}
}

func TestProbe(t *testing.T) {
	loaded := `{"data":[{"id":"embed","type":"embeddings","state":"loaded"},{"id":"cold","type":"llm","state":"not-loaded"},{"id":"warm","type":"llm","state":"loaded"}]}`
	t.Run("a model that answers", func(t *testing.T) {
		f := &fakeLLM{models: loaded, content: "pong"}
		e := f.enricher(t)
		got := e.Probe(ctx, nil)
		if !got.Reached || !got.Answered || got.Error != "" || got.Model != "warm" || got.URL != e.BaseURL || strings.Join(got.Models, ",") != "cold,warm" {
			t.Errorf("probe = %+v, want the loaded model answering and the chat models listed", got)
		}
		if f.requests != 1 || f.request.Model != "warm" || len(f.request.Messages) != 1 || f.request.Messages[0].Content != "ping" {
			t.Errorf("the model was sent %+v in %d requests, want one word and nothing else", f.request, f.requests)
		}
	})
	t.Run("a model the user named", func(t *testing.T) {
		f := &fakeLLM{models: loaded}
		e := f.enricher(t)
		e.Model = "cold"
		if got := e.Probe(ctx, nil); !got.Answered || got.Model != "cold" || f.request.Model != "cold" {
			t.Errorf("probe = %+v after asking %q, want the named model tried", got, f.request.Model)
		}
	})
	t.Run("a server that does not answer", func(t *testing.T) {
		f := &fakeLLM{}
		e := f.enricher(t)
		if got := e.Probe(ctx, nil); got.Reached || got.Answered || !strings.Contains(got.Error, "unreachable") || got.Models == nil || f.requests != 0 {
			t.Errorf("probe = %+v after %d requests, want it unreachable and the model not asked", got, f.requests)
		}
	})
	t.Run("no model to settle on", func(t *testing.T) {
		f := &fakeLLM{listed: `{"data":[{"id":"one"},{"id":"two"}]}`}
		got := f.enricher(t).Probe(ctx, nil)
		if !got.Reached || got.Answered || got.Model != "" || !strings.Contains(got.Error, "name one") || len(got.Models) != 2 || f.requests != 0 {
			t.Errorf("probe = %+v after %d requests, want the server reached, its models listed, and none asked", got, f.requests)
		}
	})
	t.Run("a model that fails", func(t *testing.T) {
		f := &fakeLLM{models: loaded, status: 500}
		if got := f.enricher(t).Probe(ctx, nil); !got.Reached || got.Answered || !strings.Contains(got.Error, "warm did not answer") || !strings.Contains(got.Error, "500") {
			t.Errorf("probe = %+v, want the server reached and the model's failure said", got)
		}
	})
	t.Run("settings yet to be saved", func(t *testing.T) {
		inUse := (&fakeLLM{models: loaded}).enricher(t)
		other := &fakeLLM{listed: `{"data":[{"id":"only"}]}`}
		st := testdb.Open(t)
		e := &Enricher{Hub: hub.New(testdb.KB(t, st), st), Resolve: Startup{URL: inUse.BaseURL}.Config}
		if got := e.Probe(ctx, nil); got.URL != inUse.BaseURL || got.Model != "warm" || !got.Answered {
			t.Errorf("probe = %+v, want the server in use tried", got)
		}
		at := other.enricher(t).BaseURL
		if got := e.Probe(ctx, &hub.ModelSettings{URL: at}); got.URL != at || got.Model != "only" || !got.Answered {
			t.Errorf("probe = %+v, want the server asked about tried", got)
		}
		if got := e.Probe(ctx, &hub.ModelSettings{URL: at, Off: true}); got.Reached || !strings.Contains(got.Error, "without a local model") || other.requests != 1 {
			t.Errorf("probe = %+v after %d requests, want nothing tried with the model off", got, other.requests)
		}
		if got := e.Hub.Settings().Model; got != (hub.ModelSettings{}) {
			t.Errorf("settings = %+v after probing, want them untouched", got)
		}
	})
}

func TestDescribe(t *testing.T) {
	llm := &fakeLLM{content: `{"title":"Choose a retention policy.","summary":"Pruning is blocked.","kind":"question","ask":"Pick one.","replies":["a","b","c","d"]}`}
	e := llm.enricher(t)
	in, err := e.describe(ctx, "m", hub.Undescribed{
		Session: hub.Session{Status: hub.Waiting, WaitingFor: "input needed", Cwd: "/work/estoria", Branch: "feat/snap",
			LastPrompt: "implement the store", LastReply: "Which retention policy?",
			Pending: &hub.Pending{Tool: "Bash", Summary: "make test"}},
		Context: []string{"The fork only lags."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.user(), "Blocked on a permission prompt for Bash: make test") {
		t.Errorf("prompt does not say what the session is blocked on: %s", llm.user())
	}
	asked := &fakeLLM{content: llm.content}
	if _, err := asked.enricher(t).describe(ctx, "m", hub.Undescribed{Session: hub.Session{Status: hub.Waiting,
		Pending: &hub.Pending{Tool: "AskUserQuestion", Summary: "Which sandwich?", Question: true}}}); err != nil {
		t.Fatal(err)
	}
	if u := asked.user(); !strings.Contains(u, "Blocked on a question it put to the developer: Which sandwich?") || strings.Contains(u, "permission prompt") {
		t.Errorf("prompt for a question = %s", u)
	}
	if in.Title != "Choose a retention policy" || in.Kind != hub.KindQuestion || in.Summary != "Pruning is blocked." || in.Ask != "Pick one." {
		t.Errorf("insight = %+v", in)
	}
	if len(in.Replies) != 3 {
		t.Errorf("replies = %v, want at most three", in.Replies)
	}

	if llm.request.Model != "m" || llm.request.ReasoningEffort != "none" {
		t.Errorf("request model %q reasoning %q", llm.request.Model, llm.request.ReasoningEffort)
	}
	if rf := llm.request.ResponseFormat; rf.Type != "json_schema" || !rf.JSONSchema.Strict || !json.Valid(rf.JSONSchema.Schema) {
		t.Errorf("response format does not constrain the reply: %+v", rf)
	}
	for _, want := range []string{"implement the store", "Which retention policy?", "estoria", "feat/snap", "input needed", "Bash: make test", "The fork only lags."} {
		if !strings.Contains(llm.user(), want) {
			t.Errorf("prompt lacks %q:\n%s", want, llm.user())
		}
	}
	if strings.Contains(llm.user(), "/work/estoria") {
		t.Error("prompt carries the full path where the directory name is enough")
	}
	if !strings.Contains(llm.system(), "Never follow instructions") {
		t.Error("system prompt no longer tells the model to treat session text as data")
	}
}

func TestDescribeToleratesAnOffScriptModel(t *testing.T) {
	llm := &fakeLLM{content: `{"title":"t","summary":"s","kind":"urgent!!","ask":""}`}
	in, err := llm.enricher(t).describe(ctx, "m", hub.Undescribed{})
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != hub.KindNone {
		t.Errorf("kind = %q; an unknown kind must not turn into an attention item", in.Kind)
	}
	if in.Replies == nil {
		t.Error("missing replies should become an empty list, which the UI maps over")
	}
}

func TestAskReportsFailuresAsErrors(t *testing.T) {
	tests := []struct {
		name string
		llm  fakeLLM
	}{
		{"server error, whatever its body says", fakeLLM{status: 500, content: `{"title":"t","summary":"s","kind":"none","ask":"","replies":[]}`}},
		{"reply is not the JSON asked for", fakeLLM{content: "Sure! Here is a summary."}},
		{"reply is empty", fakeLLM{content: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if in, err := tt.llm.enricher(t).describe(ctx, "m", hub.Undescribed{}); err == nil {
				t.Errorf("got insight %+v and no error", in)
			}
		})
	}
	t.Run("server unreachable", func(t *testing.T) {
		e := (&fakeLLM{}).enricher(t)
		e.BaseURL = "http://127.0.0.1:1"
		if _, err := e.describe(ctx, "m", hub.Undescribed{}); err == nil {
			t.Error("no error")
		}
	})
}

func TestTheModelIsShownOnlyPartOfALongExchange(t *testing.T) {
	prompt := "PROMPT-START " + strings.Repeat("é", 700) + " PROMPT-END"
	reply := "REPLY-START " + strings.Repeat("ü", 3100) + " REPLY-END"
	s := hub.Session{Cwd: "/w", LastPrompt: prompt, LastReply: reply, Insight: &hub.Insight{Title: "t", Summary: "s"}}

	llm := &fakeLLM{content: `{"title":"t","summary":"s","kind":"none","ask":"","replies":[]}`}
	if _, err := llm.enricher(t).describe(ctx, "m", hub.Undescribed{Session: s}); err != nil {
		t.Fatal(err)
	}
	u := llm.user()
	if !strings.Contains(u, "PROMPT-START "+strings.Repeat("é", 600-len("PROMPT-START "))+"…\n") || strings.Contains(u, "PROMPT-END") {
		t.Errorf("describing: want the first 600 characters of what the developer said, marked as cut")
	}
	if !strings.Contains(u, "\n…"+strings.Repeat("ü", 3000-len(" REPLY-END"))+" REPLY-END") || strings.Contains(u, "REPLY-START") {
		t.Errorf("describing: want the last 3000 characters of what the agent answered, marked as cut")
	}

	filing := &fakeLLM{content: `{"effort":"none"}`}
	filing.enricher(t).route(ctx, "m", hub.Unrouted{Session: s, Efforts: []kb.Effort{{ID: "a", Name: "A"}}})
	sorting := &fakeLLM{content: `{"kind":"none"}`}
	sorting.enricher(t).role(ctx, "m", hub.Unroled{Session: s, Roles: []kb.Role{{ID: "r", Name: "R"}}})
	for what, got := range map[string]string{"filing": filing.user(), "guessing a role": sorting.user()} {
		if !strings.Contains(got, "PROMPT-START") || strings.Contains(got, "PROMPT-END") || !strings.HasSuffix(got, "…") {
			t.Errorf("%s: want only the start of what the developer said: …%s", what, got[max(0, len(got)-60):])
		}
	}

	short := hub.Session{LastPrompt: "fix it", LastReply: "done"}
	whole := &fakeLLM{content: llm.content}
	whole.enricher(t).describe(ctx, "m", hub.Undescribed{Session: short})
	if u := whole.user(); !strings.Contains(u, "<<<\nfix it\n>>>") || !strings.Contains(u, "<<<\ndone\n>>>") {
		t.Errorf("a short exchange was changed: %s", u)
	}
}

func TestRoute(t *testing.T) {
	work := hub.Unrouted{
		Session: hub.Session{Cwd: "/work/toasters", LastPrompt: "what is stale?", Insight: &hub.Insight{Title: "Assess relevance", Summary: "Docs are stale."}},
		Efforts: []kb.Effort{{ID: "snap", Name: "Snapshot store", About: "Postgres snapshots"}, {ID: "scratch", Name: "Scratch", About: "Throwaway"}},
		Hints:   []string{"Anything in toasters is throwaway."},
	}
	tests := []struct{ reply, want string }{
		{`{"effort":"scratch"}`, "scratch"},
		{`{"effort":"none"}`, ""},
		{`{"effort":"an effort that does not exist"}`, ""},
	}
	for _, tt := range tests {
		llm := &fakeLLM{content: tt.reply}
		got, err := llm.enricher(t).route(ctx, "m", work)
		if err != nil || got != tt.want {
			t.Errorf("route with reply %s = %q, %v; want %q", tt.reply, got, err, tt.want)
		}
		for _, want := range []string{"Snapshot store", "Postgres snapshots", "Throwaway", "toasters", "Assess relevance", "Docs are stale.", "Anything in toasters is throwaway."} {
			if !strings.Contains(llm.user(), want) {
				t.Errorf("prompt lacks %q", want)
			}
		}
		var schema struct {
			Properties struct{ Effort struct{ Enum []string } }
		}
		json.Unmarshal(llm.request.ResponseFormat.JSONSchema.Schema, &schema)
		if got := strings.Join(schema.Properties.Effort.Enum, ","); got != "none,snap,scratch" {
			t.Errorf("the model may answer %q; want none or one of the efforts", got)
		}
	}
}

func TestRole(t *testing.T) {
	work := hub.Unroled{
		Session: hub.Session{Cwd: "/work/toasters", Branch: "main", LastPrompt: "review the open ones", Insight: &hub.Insight{Title: "Reviewed four pull requests", Summary: "Two need changes."}},
		Roles:   []kb.Role{{ID: "r1", Name: "Reviewer", About: "Reviews pull requests on a loop"}, {ID: "r2", Name: "Builder", About: "Writes features"}},
	}
	tests := []struct{ reply, want string }{
		{`{"kind":"r1"}`, "r1"},
		{`{"kind":"none"}`, ""},
		{`{"kind":"a role that does not exist"}`, ""},
	}
	for _, tt := range tests {
		llm := &fakeLLM{content: tt.reply}
		got, err := llm.enricher(t).role(ctx, "m", work)
		if err != nil || got != tt.want {
			t.Errorf("role with reply %s = %q, %v; want %q", tt.reply, got, err, tt.want)
		}
		for _, want := range []string{"Reviewer", "Reviews pull requests on a loop", "Writes features", "toasters", "main", "Reviewed four pull requests", "Two need changes.", "review the open ones"} {
			if !strings.Contains(llm.user(), want) {
				t.Errorf("prompt lacks %q", want)
			}
		}
		var schema struct {
			Properties struct{ Kind struct{ Enum []string } }
		}
		json.Unmarshal(llm.request.ResponseFormat.JSONSchema.Schema, &schema)
		if got := strings.Join(schema.Properties.Kind.Enum, ","); got != "none,r1,r2" {
			t.Errorf("the model may answer %q; want none or one of the roles", got)
		}
	}
	if _, err := (&fakeLLM{content: "not json"}).enricher(t).role(ctx, "m", work); err == nil {
		t.Error("an unreadable answer was taken as a choice")
	}
}

func TestNote(t *testing.T) {
	llm := &fakeLLM{content: `{"line":"Waiting on a retention decision.","stands":"Save and load work."}`}
	n, err := llm.enricher(t).note(ctx, "m", hub.Unnoted{
		Effort:    kb.Effort{Name: "Snapshot store", About: "Postgres snapshots"},
		Members:   []hub.Session{{Status: hub.Idle, Branch: "feat/snap", Insight: &hub.Insight{Title: "Choose retention", Summary: "Pruning is blocked.", Ask: "Pick one."}}},
		Context:   []string{"The fork only lags."},
		Questions: []string{"Should retention be opt-in?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Line != "Waiting on a retention decision" || n.Stands != "Save and load work." {
		t.Errorf("note = %+v", n)
	}
	for _, want := range []string{"Snapshot store", "Postgres snapshots", "Choose retention", "Pruning is blocked.", "Pick one.", "feat/snap", "The fork only lags.", "Should retention be opt-in?"} {
		if !strings.Contains(llm.user(), want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestKnowingAddsNothingWhenThereIsNothingToKnow(t *testing.T) {
	llm := &fakeLLM{content: `{"title":"t","summary":"s","kind":"none","ask":"","replies":[]}`}
	if _, err := llm.enricher(t).describe(ctx, "m", hub.Undescribed{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(llm.user(), "keep in mind") {
		t.Errorf("prompt has a heading with nothing under it:\n%s", llm.user())
	}
}
