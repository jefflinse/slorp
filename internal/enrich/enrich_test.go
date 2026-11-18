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
			io.WriteString(w, f.models)
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
	tests := []struct {
		name, models, configured string
		want                     string
		wantErr                  bool
	}{
		{"a loaded chat model", `{"data":[
			{"id":"embed","type":"embeddings","state":"loaded"},
			{"id":"cold","type":"llm","state":"not-loaded"},
			{"id":"warm","type":"vlm","state":"loaded"}]}`, "", "warm", false},
		{"nothing loaded", `{"data":[{"id":"cold","type":"llm","state":"not-loaded"}]}`, "", "", true},
		{"only an embedding model loaded", `{"data":[{"id":"embed","type":"embeddings","state":"loaded"}]}`, "", "", true},
		{"a model the user named", `{"data":[]}`, "chosen", "chosen", false},
		{"a server that is not LM Studio", `not json`, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := (&fakeLLM{models: tt.models}).enricher(t)
			e.Model = tt.configured
			got, err := e.model(ctx)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("model = %q, err %v; want %q, error %v", got, err, tt.want, tt.wantErr)
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
