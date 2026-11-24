// Package hub holds Slorp's in-memory view of every agent session and the
// outbox of replies waiting to be delivered into them.
package hub

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
)

type Status string

const (
	Working Status = "working"
	Waiting Status = "waiting"
	Idle    Status = "idle"
	Done    Status = "done"
	Failed  Status = "failed"
)

const (
	// bridgeTTL is how long after its last poll a session's bridge still counts as connected.
	bridgeTTL = 10 * time.Second
	// quiet is how long a session's content must hold still before it is worth describing.
	quiet = 3 * time.Second
	// pendingGrace is how long a new permission request is trusted before the roster confirms it.
	pendingGrace = 3 * time.Second
	// lookupTTL is how long an insight that relied on a lookup is trusted
	// before the lookup is worth repeating.
	lookupTTL = 10 * time.Minute
	// keptLookups is how many recent lookups the UI can show.
	keptLookups = 30
)

// Session is one agent conversation, normalized across agents.
type Session struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	// Job is what the agent's own commands call the session when it runs in
	// the background, empty otherwise.
	Job       string `json:"job,omitempty"`
	CanStop   bool   `json:"canStop"`
	managed   bool
	run       string
	persisted [sha256.Size]byte
	// indexed is which of its subagents its stored record lists.
	indexed string
	// Name and Title are what the session's agent calls it, and Named what
	// the user called it there, if they did.
	Name  string `json:"name"`
	Title string `json:"title"`
	Named string `json:"-"`
	// Label is the name the session goes by here, which outlasts what it
	// is doing: the one the user gave it here, else the one they gave it in
	// its agent, else its agent's title, else the first title the model
	// wrote for it. LabelVia says whether the user or the model chose it.
	Label      string    `json:"label"`
	LabelVia   string    `json:"labelVia,omitempty"`
	Cwd        string    `json:"cwd"`
	Branch     string    `json:"branch"`
	Status     Status    `json:"status"`
	WaitingFor string    `json:"waitingFor,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	ActiveAt   time.Time `json:"activeAt"`
	LastPrompt string    `json:"lastPrompt"`
	LastReply  string    `json:"lastReply"`
	// PromptAt is when the user said LastPrompt, zero when its agent did not say.
	PromptAt time.Time `json:"-"`
	// Compacted is the latest compaction its agent is known to have made,
	// nil when none is.
	Compacted *Compaction `json:"-"`
	// Pull names the pull request made from the branch the session is on,
	// as "owner/name#number"; empty when there is none.
	Pull string `json:"pull,omitempty"`
	// Timeline is what the user has said in the session since Slorp began
	// watching it, oldest first.
	Timeline []Moment `json:"timeline,omitempty"`
	// Backfilling counts the earlier exchanges still waiting for the model.
	Backfilling    int      `json:"backfilling,omitempty"`
	Pending        *Pending `json:"pending,omitempty"`
	CanReply       bool     `json:"canReply"`
	PluginVersion  string   `json:"pluginVersion,omitempty"`
	PluginRevision string   `json:"pluginRevision,omitempty"`
	PluginTUI      bool     `json:"pluginTui,omitempty"`
	Replies        []Reply  `json:"replies,omitempty"`
	Insight        *Insight `json:"insight,omitempty"`
	// Effort is the effort this session is filed under, empty when unsorted.
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`
	// Role is the role this session has, empty for none.
	Role    string `json:"role"`
	RoleVia string `json:"roleVia,omitempty"`
	// Model is the model that gave the session's last response, as its agent
	// names it, and Reasoning how hard it was asked to think.
	Model     string `json:"model,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	// ContextTokens is how much the last response was answered over, and
	// ContextWindow how much the model can take. Zero is unknown.
	ContextTokens int `json:"contextTokens,omitempty"`
	ContextWindow int `json:"contextWindow,omitempty"`
	// Used is what the session's responses have taken so far, as far as its
	// agent recorded them.
	Used Tokens `json:"used"`
	// CostUSD is the agent's own estimate of what the session has cost, zero
	// when it gave none.
	CostUSD float64 `json:"costUsd,omitempty"`
	// Subagents are the agents the session has started to do parts of its
	// work, the first started first. Used counts what they took as well.
	Subagents []Subagent `json:"subagents,omitempty"`

	bridgeSeen     time.Time
	providerLimits providerLimitSnapshot
	// live is how the session's bridge last said its subagents stand.
	live       []Subagent
	liveSeen   time.Time
	key        string
	keySince   time.Time
	insightKey string
	guess      string
	guessKey   string
	insightAt  time.Time
	roleGuess  string
	roleKey    string
	given      string
	pinned     string
	// took is what this session was handed of each session it took up.
	took []Taken
	// attention is what the session's role lets through, when the user chose it.
	attention string
}

// Plugin says whether an agent has the Slorp plugin, without which its
// sessions can be watched but not answered.
type Plugin struct {
	Installed bool `json:"installed"`
	// Enabled is false for one that is installed and turned off.
	Enabled bool   `json:"enabled"`
	Version string `json:"version,omitempty"`
	// Latest is the version its marketplace offers, when newer than Version.
	Latest string `json:"latest,omitempty"`
	// Source and Path identify a managed copy or an existing local plugin.
	Source   string `json:"source,omitempty"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
	// NeedsSetup means the server plugin is present but its TUI companion is not.
	NeedsSetup      bool   `json:"needsSetup,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Tokens counts what a session's responses took, by how each is charged.
type Tokens struct {
	// In was sent fresh, CacheWrite sent and kept, CacheRead reused.
	In         int `json:"in"`
	CacheWrite int `json:"cacheWrite"`
	CacheRead  int `json:"cacheRead"`
	// Out is what the model wrote.
	Out int `json:"out"`
}

// Moment is one thing the user said in a session, and the model's line on
// where the session stood once it had answered.
type Moment struct {
	At     time.Time `json:"at"`
	Prompt string    `json:"prompt"`
	// Recorded marks a prompt whose timestamp was read from its transcript.
	Recorded bool          `json:"recorded,omitempty"`
	Aliases  []PromptAlias `json:"aliases,omitempty"`
	// Line is empty when the model never described the exchange, and so
	// is Summary, its fuller account of where things stood.
	Line    string `json:"line,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Compacted is set when the moment is no word of the user's but the
	// session's agent compacting the conversation.
	Compacted *Compaction `json:"compacted,omitempty"`
}

// PromptAlias identifies a harness-generated replay of an original user prompt.
type PromptAlias struct {
	At     time.Time `json:"at"`
	Prompt string    `json:"prompt"`
}

// Compaction is one time a session's agent replaced the conversation so far
// with a summary of it, to make room in its context.
type Compaction struct {
	ID string    `json:"id,omitempty"`
	At time.Time `json:"at"`
	// ObservedAt identifies a legacy live marker reconciled with this record.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// Auto is true when the agent did it for want of room, and false when
	// the user asked.
	Auto bool `json:"auto,omitempty"`
	// Before and After are the tokens in its context either side of the
	// compaction, zero when its agent did not say.
	Before int `json:"before,omitempty"`
	After  int `json:"after,omitempty"`
}

func sameCompaction(a, b *Compaction) bool {
	if a == nil || b == nil {
		return false
	}
	if a.ID != "" && b.ID != "" {
		return a.ID == b.ID
	}
	return a.At.Equal(b.At) || !a.ObservedAt.IsZero() && a.ObservedAt.Equal(b.At) || !b.ObservedAt.IsZero() && b.ObservedAt.Equal(a.At)
}

func legacyCompactionNear(a, b *Compaction) bool {
	return a != nil && b != nil && (a.ID == "") != (b.ID == "") && a.At.Sub(b.At).Abs() <= 5*time.Second
}

const (
	// momentLimit is how much of what the user said a moment keeps, in runes.
	momentLimit = 240
	// timelineLimit is how many moments a session keeps.
	timelineLimit = 40
	// untimedLookback is how many of the latest moments a prompt with no
	// time of its own is checked against before it counts as new.
	untimedLookback = 3
)

// ViaGuess marks a filing the model chose, as opposed to the user or a rule.
const ViaGuess = "guess"

// Effort is a declared effort with the model's account of where it stands.
type Effort struct {
	kb.Effort
	Line   string `json:"line"`
	Stands string `json:"stands"`
	// ActiveAt is when a session filed under it last did anything, or the
	// last of them closed; zero when none ever has.
	ActiveAt time.Time `json:"activeAt,omitzero"`
}

// Note is a model-written status for one effort.
type Note struct {
	Line   string `json:"line"`
	Stands string `json:"stands"`
}

// Unrouted is a described session that neither the user nor a rule has filed.
type Unrouted struct {
	Session Session
	Key     string
	Efforts []kb.Effort
	// Hints are the user's standing context and routing hints.
	Hints []string
}

// Unroled is a described session the user has given no role, for the model
// to guess one.
type Unroled struct {
	Session Session
	Key     string
	Roles   []kb.Role
}

// Unnoted is an effort whose sessions have changed since its note was written.
type Unnoted struct {
	Effort  kb.Effort
	Members []Session
	Key     string
	Context []string
	// Questions are what the user has left undecided about this effort.
	Questions []string
}

// Insight is a model-written description of a session's current state.
type Insight struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Kind    string   `json:"kind"`
	Ask     string   `json:"ask"`
	Replies []string `json:"replies"`
	// Lookups names what the model looked up through an integration to write this.
	Lookups []string `json:"lookups"`
}

// Lookup is one thing the model looked up through an integration.
type Lookup struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	What    string    `json:"what"`
	OK      bool      `json:"ok"`
	// Detail is what came back, or why nothing did.
	Detail string `json:"detail"`
}

// Insight kinds, and the item kinds the hub derives without a model.
const (
	KindQuestion   = "question"
	KindPermission = "permission"
	KindReview     = "review"
	KindNone       = "none"
	KindFailed     = "failed"
	// KindContext is a session whose context window is nearly full.
	KindContext = "context"

	// contextWarn is how full, in percent, a context window is before the
	// user is told.
	contextWarn = 90
)

// Item is one thing that needs the user: a session waiting on them, or
// something an integration's feed found.
type Item struct {
	ID string `json:"id"`
	// SessionID is empty for an item from a feed.
	SessionID string `json:"sessionId,omitempty"`
	Kind      string `json:"kind"`
	// Urgency is "blocking" while the user is holding up another person,
	// "awaiting" while something of the user's own is stopped until they
	// act, and "whenever" when nothing is stopped.
	Urgency string `json:"urgency"`
	Title   string `json:"title"`
	// Subtitle is the model's line on what a session is doing right now,
	// or on what a point of a discussion says.
	Subtitle string    `json:"subtitle,omitempty"`
	Ask      string    `json:"ask"`
	Summary  string    `json:"summary"`
	Replies  []string  `json:"replies"`
	At       time.Time `json:"at"`
	// Source is the integration whose feed found the item.
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	// Repo is the repository a feed item belongs to, and Pull the pull
	// request it is about, as "owner/name#number": it is filed with the
	// pull request, or by the repository when it is about none.
	Repo string `json:"repo,omitempty"`
	Pull string `json:"pull,omitempty"`
	// Point is the point of the pull request's discussion an item was raised for.
	Point string `json:"point,omitempty"`
	// Effort is the effort the item is filed under, empty when unsorted.
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`
}

// FeedStatus is how an integration's last collection went.
type FeedStatus struct {
	At time.Time `json:"at"`
	// Error is why the last collection failed; the items are then older.
	Error string `json:"error,omitempty"`
	Count int    `json:"count"`
}

type feed struct {
	items []Item
	pulls []integrations.PullRequest
	FeedStatus
}

// PullRequest is an open pull request an integration listed, filed where
// the user put it or else with its repository.
type PullRequest struct {
	integrations.PullRequest
	Source    string `json:"source"`
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`
	// Points are the things in its discussion that wait on the user, the
	// latest first; empty until the discussion has been read, which ReadAt
	// says when it was.
	Points []PullPoint `json:"points"`
	ReadAt time.Time   `json:"readAt,omitzero"`
	// Brief is the model's account of the discussion, nil until it has
	// written one.
	Brief *PullBrief `json:"brief,omitempty"`
	// Behind is true while the discussion, or what the model wrote of it,
	// is older than the listing.
	Behind bool `json:"behind,omitempty"`
}

// Limit is how much of one of the user's usage windows with an agent's
// provider is spent.
type Limit struct {
	Agent string `json:"agent"`
	// Kind is the window as the agent names it, such as "five_hour".
	Kind        string  `json:"kind"`
	PercentUsed float64 `json:"percentUsed"`
	// ResetsAt is when the window starts over, zero when the agent did not say.
	ResetsAt time.Time `json:"resetsAt,omitzero"`
	// At is when the agent measured it, zero for a reading of unknown age.
	At time.Time `json:"at,omitzero"`
}

// Summarizer reports which model, if any, is writing insights.
type Summarizer struct {
	Model string `json:"model"`
	Error string `json:"error,omitempty"`
	// URL is where the local model server is looked for.
	URL string `json:"url,omitempty"`
	// Named is the model asked for by name, empty when whichever is loaded
	// is used.
	Named string `json:"named,omitempty"`
	// Off says Slorp is running without a local model to look for.
	Off bool `json:"off,omitempty"`
	// FixedURL and FixedModel say the address and the model's name were
	// given when Slorp started, where the user's settings do not reach.
	FixedURL   bool `json:"fixedUrl,omitempty"`
	FixedModel bool `json:"fixedModel,omitempty"`
	// Doing names the piece of work the model is on, empty while it is idle.
	Doing string `json:"doing,omitempty"`
}

// Subagent is an agent a session started to do part of its work.
type Subagent struct {
	ID string `json:"id"`
	// ReportRevision changes whenever a retained record is accepted.
	ReportRevision int64 `json:"reportRevision,omitempty"`
	// Type is the kind of agent it was started as, and Description the few
	// words its session gave the task.
	Type        string `json:"type"`
	Description string `json:"description"`
	// Background is true for one its session did not wait on.
	Background bool `json:"background,omitempty"`
	// Depth is 1 for one the session started itself, 2 for one such a
	// subagent started, and so on.
	Depth int `json:"depth,omitempty"`
	// Done is true once it has finished.
	Done      bool      `json:"done"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	// ActiveAt is when it last did anything, which for one that is done is
	// when it finished.
	ActiveAt time.Time `json:"activeAt,omitzero"`
	// Model is the model that gave its last response, and Effort how hard
	// it was asked to think.
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	// Tools counts the tool calls it has made.
	Tools int    `json:"tools"`
	Used  Tokens `json:"used"`

	// Status is how its loop stands: "running", "waiting", "idle",
	// "completed", "failed", "killed" or "pending". Retained terminal
	// statuses outlive the bridge; other statuses, Doing and Samples expire.
	Status string `json:"status,omitempty"`
	// Doing is its own latest account of what it is doing.
	Doing string `json:"doing,omitempty"`
	// Samples are its latest token readings, oldest first and a few seconds
	// apart: at each, the size of its last request and all it has written.
	Samples []int `json:"samples,omitempty"`
	// CostUSD is its agent's estimate of what it has cost, zero for none.
	CostUSD float64 `json:"costUsd,omitempty"`
	// ParentID is the subagent that started it, empty when its session did.
	ParentID string `json:"parentId,omitempty"`
}

// ended reports whether a status a bridge gave says the subagent's work is
// over, however it went.
func ended(status string) bool {
	return status == "completed" || status == "failed" || status == "killed"
}

// withLive returns the subagents read from a session's record with what its
// bridge says of each, and after them those only the bridge knows of yet.
func withLive(known, live []Subagent) []Subagent {
	out := cloneSubagents(known)
	if len(live) == 0 {
		return out
	}
	at := map[string]int{}
	for i, a := range out {
		at[a.ID] = i
	}
	for _, l := range live {
		l.Samples = slices.Clone(l.Samples)
		i, ok := at[l.ID]
		if !ok {
			l.Done = ended(l.Status)
			out = append(out, l)
			continue
		}
		a := &out[i]
		if a.ReportRevision > 0 && a.Done {
			continue
		}
		a.Status, a.Doing, a.Samples, a.CostUSD, a.ParentID = l.Status, l.Doing, l.Samples, l.CostUSD, l.ParentID
		a.Type, a.Description, a.Model = cmp.Or(a.Type, l.Type), cmp.Or(a.Description, l.Description), cmp.Or(a.Model, l.Model)
		a.Done = a.Done || ended(l.Status)
	}
	return out
}

// Delegated records how a session's bridge says its subagents stand, in
// place of what it said before.
func (s *Session) Delegated(live []Subagent) {
	s.live, s.liveSeen = cloneSubagents(live), time.Now()
}

// SubagentReport is what a subagent was asked and what came of it.
type SubagentReport struct {
	Prompt string `json:"prompt"`
	// Report is what it handed back to its session when Handed is true,
	// and otherwise the last thing it said.
	Report string `json:"report"`
	Handed bool   `json:"handed"`
}

// ModelSettings is the user's choice of local model.
type ModelSettings struct {
	// URL is the address of an OpenAI-compatible server, empty for the
	// usual one.
	URL string `json:"url"`
	// Name is the model to use, empty for whichever one is loaded.
	Name string `json:"name"`
	// Off runs Slorp without a local model.
	Off bool `json:"off"`
}

// ModelCheck is how one try of a local model server went.
type ModelCheck struct {
	URL string `json:"url"`
	// Reached says the server answered, and Models lists what it offers.
	Reached bool     `json:"reached"`
	Models  []string `json:"models"`
	// Model is the one that would be used, empty when none could be
	// settled on.
	Model string `json:"model,omitempty"`
	// Answered says Model replied to a one-word message, and Millis how
	// long it took.
	Answered bool  `json:"answered"`
	Millis   int64 `json:"millis,omitempty"`
	// Error is what stopped the try, empty when the model answered.
	Error string `json:"error,omitempty"`
}

// Undescribed is a settled session whose current content has no insight yet.
type Undescribed struct {
	Session Session
	Key     string
	// Context is the user's standing context.
	Context []string
}

// Pending is a request a session is blocked on: for permission to act, or
// for an answer to a question it put to the user.
type Pending struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
	// Question is true when the session asked the user something, which is
	// answered by picking from its options and not by an allow.
	Question bool `json:"question"`
	// Ordered answers questions by position, preserving individual selections.
	Ordered bool `json:"ordered,omitempty"`
	// Input is the tool's full input, shown to the user before they decide.
	Input string `json:"input"`
	// Answerable is true while Slorp may answer the request as well as the
	// session's own terminal, whose dialog is asking at the same time.
	Answerable bool `json:"answerable"`
	// AgentID is the subagent that asks, empty when the session itself does.
	AgentID string    `json:"agentId,omitempty"`
	At      time.Time `json:"at"`

	// questions is what a question asks, read from its input.
	questions []asked
}

// asked is one question put to the user and the options it offers, by label.
type asked struct {
	text   string
	labels []string
	// many is true when more than one option may be picked.
	many   bool
	custom bool
}

// questionsIn reads the questions a prompt's input holds. It returns nil
// unless each has text and labelled options or accepts custom text. Legacy
// prompts require distinct question text because their answers are keyed by it.
func questionsIn(input string, ordered bool) []asked {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Custom      bool   `json:"custom"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return nil
	}
	var out []asked
	for _, q := range in.Questions {
		a := asked{text: q.Question, many: q.MultiSelect, custom: ordered && q.Custom}
		for _, o := range q.Options {
			if o.Label == "" || slices.Contains(a.labels, o.Label) {
				return nil
			}
			a.labels = append(a.labels, o.Label)
		}
		if q.Question == "" || (len(a.labels) == 0 && !a.custom) || (!ordered && slices.ContainsFunc(out, func(b asked) bool { return b.text == q.Question })) {
			return nil
		}
		out = append(out, a)
	}
	return out
}

// Decision is the user's answer, given in Slorp, to a request.
type Decision struct {
	Behavior string `json:"state"`
	Message  string `json:"message,omitempty"`
	// Answers holds, for a question, what the user picked for each of its
	// questions by its text; several picks are joined with ", ".
	Answers map[string]string `json:"answers,omitempty"`
	// Selections preserves answers by question position for ordered prompts.
	Selections [][]string `json:"selections,omitempty"`
}

// Decision behaviors. Terminal means Slorp will not answer: the session's
// own dialog decides.
const (
	Allow    = "allow"
	Deny     = "deny"
	Terminal = "terminal"
)

// Settings are the user's preferences for how Slorp behaves.
type Settings struct {
	// AnswerPermissions lets permission prompts be answered in Slorp as well
	// as in the session's terminal.
	AnswerPermissions bool `json:"answerPermissions"`
	// Model is which local model to use and where it is.
	Model ModelSettings `json:"model"`
	// Integrations are the ones the user has turned on, by ID.
	Integrations map[string]bool `json:"integrations"`
	// IntegrationsRevision counts changes to Integrations.
	IntegrationsRevision int `json:"integrationsRevision"`
	// Terminal names the terminal emulator sessions are opened in, empty
	// for whichever Slorp was started from.
	Terminal string `json:"terminal,omitempty"`
	// Prompts is what the user has the local model told.
	Prompts Prompts `json:"prompts"`
	// Rewritten counts, by task, the times the model has been asked to
	// write again what it wrote for it.
	Rewritten map[string]int `json:"rewritten"`
}

// Whether what the model wrote is written again when its instructions
// change, as a Prompts' Rewrite.
const (
	// RewriteAsk leaves it to be asked of the user.
	RewriteAsk    = "ask"
	RewriteAlways = "always"
	RewriteNever  = "never"
)

// PromptLookup is the task of choosing what to look up before a session is
// described. The other tasks go by the Kind of the Action they end in.
const PromptLookup = "lookup"

// Prompts is what the user has the local model told, in place of and ahead
// of Slorp's own instructions.
type Prompts struct {
	// Global goes ahead of the instructions for every task.
	Global string `json:"global"`
	// Tasks holds the instructions the user wrote for a task, by the task,
	// in place of Slorp's own. A task with none goes by Slorp's.
	Tasks map[string]string `json:"tasks"`
	// Rewrite is whether what the model already wrote is written again
	// when any of this changes.
	Rewrite string `json:"rewrite"`
}

// behind lists the tasks for which what the model wrote was written under
// instructions that differ between was and now, in the form Rewrite takes
// them. The tasks done only when asked for leave nothing behind.
func (was Prompts) behind(now Prompts) []string {
	differs := func(tasks ...string) bool {
		return slices.ContainsFunc(tasks, func(t string) bool { return was.Tasks[t] != now.Tasks[t] })
	}
	var out []string
	for _, t := range []struct {
		task    string
		changed bool
	}{
		{ActDescribe, was.Global != now.Global || differs(ActDescribe, PromptLookup)},
		{ActFile, differs(ActFile)}, {ActRole, differs(ActRole)}, {ActNote, differs(ActNote)},
		{ActBrief, was.Global != now.Global || differs(ActBrief)},
	} {
		if t.changed {
			out = append(out, t.task)
		}
	}
	return leading(out)
}

// leading returns tasks without those that follow from another of them: a
// session described again is then filed, given a role and noted again.
func leading(tasks []string) []string {
	if !slices.Contains(tasks, ActDescribe) {
		return tasks
	}
	return slices.DeleteFunc(slices.Clone(tasks), func(t string) bool { return t == ActFile || t == ActRole || t == ActNote })
}

// Cache is where the hub keeps what should outlive a restart.
type Cache interface {
	Get(kind, id string, v any) (bool, error)
	Put(kind, id string, v any) error
	PutAll(docs ...store.Doc) error
	Each(kind string, fn func(id string, body []byte)) error
	Delete(kind, id string) error
}

// What the model wrote about one session, kept so a restart need not ask again.
type sessionDoc struct {
	Managed    bool      `json:"managed,omitempty"`
	Title      string    `json:"title,omitempty"`
	Model      string    `json:"model,omitempty"`
	Reasoning  string    `json:"reasoning,omitempty"`
	ActiveAt   time.Time `json:"activeAt,omitzero"`
	LastPrompt string    `json:"lastPrompt,omitempty"`
	LastReply  string    `json:"lastReply,omitempty"`
	PromptAt   time.Time `json:"promptAt,omitzero"`
	Insight    *Insight  `json:"insight"`
	InsightKey string    `json:"insightKey"`
	Guess      string    `json:"guess"`
	GuessKey   string    `json:"guessKey"`
	InsightAt  time.Time `json:"insightAt"`
	RoleGuess  string    `json:"roleGuess"`
	RoleKey    string    `json:"roleKey"`
	Given      string    `json:"given"`
	Pinned     string    `json:"pinned"`
	Took       []Taken   `json:"took,omitempty"`
	// Children lists the session's own subagents as last seen.
	Children []Subagent `json:"children,omitempty"`
	Timeline []Moment   `json:"timeline,omitempty"`
	// The rest is the session as it was when last stored, for when it is
	// no longer open to ask. Ended is zero while it is open.
	Label     string    `json:"label,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Ended     time.Time `json:"ended,omitzero"`
	Used      Tokens    `json:"used,omitzero"`
	CostUSD   float64   `json:"costUsd,omitempty"`
}

type ReplyState string

const (
	Queued    ReplyState = "queued"
	Claimed   ReplyState = "claimed"
	Delivered ReplyState = "delivered"
	Dropped   ReplyState = "dropped"
)

// Reply is text the user wrote in Slorp for delivery into a session.
type Reply struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionId"`
	Text      string     `json:"text"`
	State     ReplyState `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
	// Owner is what ran the session when the reply was queued: OwnerRun for
	// a run Slorp started, which ends with Slorp, and OwnerBridge for one run
	// elsewhere. It is empty for a reply queued before Slorp kept it.
	Owner string `json:"owner,omitempty"`
}

// What ran a session when a reply was queued for it, as a Reply's Owner.
const (
	OwnerRun    = "run"
	OwnerBridge = "bridge"
)

type Hub struct {
	mu                  sync.Mutex
	runs                map[string]*managedRun
	permissionRoots     map[string]*permissionReference
	replyRuns           map[string]string
	unmanagedAdmissions map[string]int
	lifecycle           map[string]lifecycleRecord
	lifecycleActive     map[string]bool
	lifecycleErr        error
	sessions            map[string]*Session
	replies             []*Reply
	subs                map[chan struct{}]struct{}
	last                string
	summ                Summarizer
	kb                  *kb.Store
	notes               map[string]note
	cache               Cache
	settings            Settings
	waiters             map[string]chan Decision
	lookups             []Lookup
	feeds               map[string]*feed
	clones              map[string][]string
	// checkouts is, for each session's directory, the repositories checked
	// out there.
	checkouts map[string][]string
	// limits is each agent's latest reading of the user's usage windows.
	limits map[string][]Limit
	// wrapups is what has been written, or asked to be, about each day.
	wrapups map[string]Wrapup
	// discussed is what is kept of each pull request's discussion, by the
	// id of its PullRef, and discussFailed when one last could not be read.
	discussed     map[string]*pullDoc
	discussFailed map[string]time.Time
	// wantedPull is the id of the pull request the user is looking at.
	wantedPull string
	// archive holds the sessions that have closed. unsettled holds those
	// that were open when Slorp last stopped, until their agent's open
	// sessions are known.
	archive   map[string]sessionDoc
	unsettled map[string]sessionDoc
	// unread holds the sessions reported open whose kept record could not
	// be read, until it can be, their agent's roster leaves them out, or
	// their bridge falls silent.
	unread  map[string]unreadReport
	started time.Time
	// backfill holds, by session, the exchanges waiting for the model to
	// say how each came out.
	backfill map[string][]Exchange
	// rostered holds the agents whose open sessions a roster lists.
	rostered map[string]bool
	// plugins is what each agent last said of its Slorp plugin.
	plugins map[string]Plugin
	// release is the release of Slorp running, as last reported.
	release Release
	// actions is what the local model has done, the oldest first.
	actions []Action
	// origins holds, for each session something is known of, the session
	// it continues, empty for one that continues none.
	origins map[string]string
	// retained holds durable child records by root and child ID.
	retained map[string]map[string]subagentRecord
}

// unreadReport is which agent last reported a session whose kept record
// could not be read, and when.
type unreadReport struct {
	agent string
	at    time.Time
}

type note struct {
	Key string `json:"key"`
	Note
}

// Open returns a hub restored from what c holds. When the sessions, what
// each continues, the replies or the retained subagents kept cannot all be
// read, it returns an error, having changed nothing, and no hub.
func Open(k *kb.Store, c Cache) (*Hub, error) {
	h := &Hub{sessions: map[string]*Session{}, subs: map[chan struct{}]struct{}{}, kb: k, notes: map[string]note{},
		cache: c, settings: Settings{AnswerPermissions: true}, waiters: map[string]chan Decision{}, feeds: map[string]*feed{}, wrapups: map[string]Wrapup{},
		archive: map[string]sessionDoc{}, unsettled: map[string]sessionDoc{}, unread: map[string]unreadReport{}, started: time.Now(), origins: map[string]string{}, plugins: map[string]Plugin{},
		discussed: map[string]*pullDoc{}, discussFailed: map[string]time.Time{}}
	var err error
	if h.retained, err = h.readSubagents(); err != nil {
		return nil, err
	}
	if h.replies, err = h.readReplies(); err != nil {
		return nil, err
	}
	if err := h.readSessions(); err != nil {
		return nil, err
	}
	if err := h.readOrigins(); err != nil {
		return nil, err
	}
	h.loadLifecycle()
	h.loadActions()
	h.warn(c.Each("pull", func(id string, body []byte) {
		var d pullDoc
		if json.Unmarshal(body, &d) == nil {
			h.discussed[id] = &d
		}
	}))
	h.warn(c.Each("wrapup", func(day string, body []byte) {
		var w Wrapup
		if json.Unmarshal(body, &w) == nil {
			h.wrapups[day] = w
		}
	}))
	h.warn(c.Each("note", func(id string, body []byte) {
		var n note
		if json.Unmarshal(body, &n) == nil {
			h.notes[id] = n
		}
	}))
	for _, r := range h.replies {
		managed := h.unsettled[r.SessionID].Managed || h.archive[r.SessionID].Managed
		if (r.State == Queued || r.State == Claimed) && (r.Owner == OwnerRun || r.Owner == "" && managed) {
			r.State = Dropped
			h.warn(h.cache.Put("reply", r.ID, r))
		}
	}
	_, err = c.Get("setting", "main", &h.settings)
	h.warn(err)
	if h.settings.Integrations == nil {
		h.settings.Integrations = map[string]bool{}
	}
	if h.settings.Prompts.Tasks == nil {
		h.settings.Prompts.Tasks = map[string]string{}
	}
	if h.settings.Rewritten == nil {
		h.settings.Rewritten = map[string]int{}
	}
	h.settings.Prompts.Rewrite = cmp.Or(h.settings.Prompts.Rewrite, RewriteAsk)
	return h, nil
}

// New is Open for what is known to be readable: it panics where Open fails.
func New(k *kb.Store, c Cache) *Hub {
	h, err := Open(k, c)
	if err != nil {
		panic(err)
	}
	return h
}

// readSessions reads the sessions kept, those ended into the archive and
// those that were open into the unsettled, and says why when what it read is
// not all of them.
func (h *Hub) readSessions() error {
	var bad error
	err := h.cache.Each("session", func(id string, body []byte) {
		var kept *sessionDoc
		if err := json.Unmarshal(body, &kept); err != nil || kept == nil {
			bad = fmt.Errorf("decoding session %s: %w", id, cmp.Or(err, errNull))
			return
		}
		d := *kept
		switch {
		case !d.Ended.IsZero():
			if h.worthKeeping(id, d) {
				h.archive[id] = d
			}
		case d.Agent != "":
			h.unsettled[id] = d
		}
	})
	if err != nil {
		return fmt.Errorf("loading sessions: %w", err)
	}
	return bad
}

// errNull is why a kept document that is null is not one.
var errNull = errors.New("null")

// readOrigins reads what each session kept continues, and says why when what
// it read is not all of it.
func (h *Hub) readOrigins() error {
	var bad error
	err := h.cache.Each("origin", func(id string, body []byte) {
		var from *string
		if err := json.Unmarshal(body, &from); err != nil || from == nil {
			bad = fmt.Errorf("decoding the origin of %s: %w", id, cmp.Or(err, errNull))
			return
		}
		h.origins[id] = *from
	})
	if err != nil {
		return fmt.Errorf("loading origins: %w", err)
	}
	return bad
}

func (h *Hub) warn(err error) {
	if err != nil {
		log.Printf("hub: store: %v", err)
	}
}

// HeldOpen reports whether a session is reported open though what is kept of
// it could not yet be read.
func (h *Hub) HeldOpen(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, held := h.unread[id]
	return held
}

// restore gives a session agent reports, seen for the first time, what the
// model last wrote about it. It reports false, with s and what is kept of it
// unchanged, when that cannot be read; the session is then not to be
// admitted, and is held as unread.
func (h *Hub) restore(agent string, s *Session) bool {
	found, err := h.recall(s)
	if err != nil {
		h.warn(fmt.Errorf("session %s could not be read: %w", s.ID, err))
		h.unread[s.ID] = unreadReport{agent: agent, at: time.Now()}
		return false
	}
	delete(h.unread, s.ID)
	if found {
		delete(h.unsettled, s.ID)
		if _, back := h.archive[s.ID]; back {
			delete(h.archive, s.ID)
			h.remember(s)
		}
	}
	return true
}

// recall gives s what was kept of it, and reports whether anything was. When
// what was kept cannot be read, s is unchanged and the error says why.
func (h *Hub) recall(s *Session) (bool, error) {
	var d sessionDoc
	if ok, err := h.cache.Get("session", s.ID, &d); err != nil || !ok {
		return false, err
	}
	s.Insight, s.insightKey, s.guess, s.guessKey, s.insightAt = d.Insight, d.InsightKey, d.Guess, d.GuessKey, d.InsightAt
	s.roleGuess, s.roleKey = d.RoleGuess, d.RoleKey
	s.given, s.pinned, s.took, s.Timeline = d.Given, d.Pinned, d.Took, tidy(d.Timeline)
	if d.Managed || s.managed {
		s.managed = true
		s.Cwd, s.Branch, s.Title = d.Cwd, d.Branch, d.Title
		s.StartedAt, s.ActiveAt = d.StartedAt, d.ActiveAt
		if s.ActiveAt.IsZero() {
			s.ActiveAt = d.Ended
		}
		s.Used, s.CostUSD = d.Used, d.CostUSD
		s.Model, s.Reasoning = d.Model, d.Reasoning
		s.LastPrompt, s.LastReply, s.PromptAt = d.LastPrompt, d.LastReply, d.PromptAt
	}
	return true, nil
}

func (h *Hub) remember(s *Session) {
	h.warn(h.checkpoint(s))
}

func (h *Hub) checkpoint(s *Session) error {
	doc := h.doc(s)
	fingerprint, err := fingerprint(doc)
	if err != nil {
		return err
	}
	if s.persisted == fingerprint {
		s.indexed = childKey(doc.Children)
		return nil
	}
	if err := h.cache.Put("session", s.ID, doc); err != nil {
		return err
	}
	s.persisted, s.indexed = fingerprint, childKey(doc.Children)
	return nil
}

// fingerprint identifies what is stored of a session, as checkpoint compares it.
func fingerprint(d sessionDoc) ([sha256.Size]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// Taken is a session another took up: its id, and the subagents it handed
// over, which the one that took it up lists and reports as its own.
type Taken struct {
	ID       string     `json:"id"`
	Children []Subagent `json:"children,omitempty"`
}

// filing is the knowledge base as a session's filing is read from it.
type filing interface {
	Resolve(sessionID, cwd string) (effortID, via string, ok bool)
	Active(id string) bool
}

// doc is what is stored of a session while it is open.
func (h *Hub) doc(s *Session) sessionDoc { return docIn(h.kb, s) }

// docIn is what is stored of a session, filed as k has it.
func docIn(k filing, s *Session) sessionDoc {
	label, _ := s.label()
	effort, _ := filedIn(k, s)
	return sessionDoc{Children: childIndex(s.Subagents), Managed: s.managed, Title: s.Title, Model: s.Model, Reasoning: s.Reasoning, ActiveAt: s.ActiveAt,
		LastPrompt: s.LastPrompt, LastReply: s.LastReply, PromptAt: s.PromptAt,
		Insight: s.Insight, InsightKey: s.insightKey, Guess: s.guess, GuessKey: s.guessKey, InsightAt: s.insightAt,
		RoleGuess: s.roleGuess, RoleKey: s.roleKey, Given: s.given, Pinned: s.pinned, Took: s.took, Timeline: s.Timeline, Label: label, Effort: effort,
		Agent: s.Agent, Cwd: s.Cwd, Branch: s.Branch, StartedAt: s.StartedAt, Used: s.Used, CostUSD: s.CostUSD}
}

// filed returns the effort a session is filed under and how it came to be,
// or "" when it is unsorted.
func (h *Hub) filed(s *Session) (effort, via string) { return filedIn(h.kb, s) }

// filedIn is filed, as k has it.
func filedIn(k filing, s *Session) (effort, via string) {
	if id, via, ok := k.Resolve(s.ID, s.Cwd); ok {
		return id, via
	}
	// A guess made on older content stands until a newer one replaces it.
	if k.Active(s.guess) {
		return s.guess, ViaGuess
	}
	return "", ""
}

// SetRoster replaces the known sessions of one agent with the live set an
// adapter observed. Fields only a bridge can report survive the replacement.
// A session first seen whose kept record cannot be read is left out until it can.
func (h *Hub) SetRoster(agent string, live []Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prunePermissionDecisions(time.Now())
	seen := map[string]bool{}
	for i := range live {
		s := live[i]
		s.Subagents = cloneSubagents(s.Subagents)
		s.live = cloneSubagents(s.live)
		seen[s.ID] = true
		if old, ok := h.sessions[s.ID]; ok {
			s.providerLimits = old.providerLimits
			s.bridgeSeen, s.live = old.bridgeSeen, old.live
			s.liveSeen = old.liveSeen
			s.key, s.keySince, s.insightKey, s.Insight = old.key, old.keySince, old.insightKey, old.Insight
			s.guess, s.guessKey, s.insightAt = old.guess, old.guessKey, old.insightAt
			s.roleGuess, s.roleKey = old.roleGuess, old.roleKey
			s.given, s.pinned, s.took, s.Timeline = old.given, old.pinned, old.took, old.Timeline
			s.persisted, s.indexed = old.persisted, old.indexed
			s.ContextWindow, s.CostUSD = old.ContextWindow, old.CostUSD
			// The roster stops saying "waiting" once the terminal's dialog is
			// answered. A request it has not caught up with yet is kept.
			if p := old.Pending; p != nil && (s.Status == Waiting || time.Since(p.At) < pendingGrace) {
				s.Pending, s.Status, s.WaitingFor = p, Waiting, "permission prompt"
			} else if p != nil {
				h.settle(p.ID)
			}
			if s.LastReply == "" {
				s.LastReply = old.LastReply
			}
			if s.LastPrompt == "" {
				s.LastPrompt, s.PromptAt = old.LastPrompt, old.PromptAt
			}
		} else if !h.restore(agent, &s) {
			continue
		}
		h.sessions[s.ID] = &s
	}
	for id, u := range h.unread {
		if u.agent == agent && !seen[id] {
			delete(h.unread, id)
		}
	}
	for id, s := range h.sessions {
		if s.Agent == agent && !seen[id] {
			h.leave(s)
			delete(h.sessions, id)
		}
	}
	h.sweep(agent)
	h.fold(agent)
	h.changed()
}

// Release says which release of Slorp is running and whether a newer one
// is out.
type Release struct {
	// Version is the release running, empty for a build that is not one.
	Version string `json:"version,omitempty"`
	// Latest is the newest release, when it is newer than Version.
	Latest string `json:"latest,omitempty"`
	// Manual says why Slorp cannot install Latest itself, empty when it can.
	Manual string `json:"manual,omitempty"`
}

// SetRelease records which release of Slorp is running and whether a newer
// one is out.
func (h *Hub) SetRelease(r Release) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release == r {
		return
	}
	h.release = r
	h.last = ""
	h.changed()
}

// SetPlugin records whether agent has the Slorp plugin.
func (h *Hub) SetPlugin(agent string, p Plugin) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if was, ok := h.plugins[agent]; ok && was == p {
		return
	}
	h.plugins[agent] = p
	h.last = ""
	h.changed()
}

// Rostered declares that the open sessions of agent are the ones SetRoster
// lists, whatever a bridge reports.
func (h *Hub) Rostered(agent string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rostered == nil {
		h.rostered = map[string]bool{}
	}
	h.rostered[agent] = true
}

// Apply records a bridge's report about one session. It creates the session
// if nothing has listed it yet, and reports whether it did. A report about a
// session its agent's roster does not list is dropped, as is one about a
// session first seen whose kept record cannot be read.
func (h *Hub) Apply(agent, id string, fn func(*Session)) (created bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok && h.rostered[agent] {
		return false
	}
	if !ok {
		now := time.Now()
		s = &Session{ID: id, Agent: agent, Kind: "interactive", Status: Idle, StartedAt: now, ActiveAt: now}
		if !h.restore(agent, s) {
			return false
		}
		h.sessions[id] = s
	}
	s.bridgeSeen = time.Now()
	previous := s.Pending
	if fn != nil {
		fn(s)
	}
	if previous != nil && (s.Pending == nil || s.Pending.ID != previous.ID) {
		h.settle(previous.ID)
	}
	h.changed()
	return !ok
}

// Retire drops the sessions of an agent whose bridge has reported nothing
// for longer than silence. It stands in for a roster where a session's
// bridge is the only witness that the session is still open.
func (h *Hub) Retire(agent string, silence time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prunePermissionDecisions(time.Now())
	for id, u := range h.unread {
		if u.agent == agent && time.Since(u.at) > silence {
			delete(h.unread, id)
		}
	}
	for id, s := range h.sessions {
		if s.Agent == agent && s.run == "" && time.Since(s.bridgeSeen) > silence {
			h.leave(s)
			delete(h.sessions, id)
		}
	}
	// A session that was open before a restart has had its chance to be
	// heard from by now.
	if time.Since(h.started) > silence {
		h.sweep(agent)
	}
	h.changed()
}

// Ask records that a session wants permission for a tool call. Slorp may
// answer it when the bridge says it can and the user has that turned on;
// the session's terminal is asking either way.
func (h *Hub) Ask(agent, sessionID string, p Pending, canAnswer bool) Pending {
	p.ID, p.At = newID(), time.Now()
	h.Apply(agent, sessionID, func(s *Session) {
		if p.Question {
			p.questions = questionsIn(p.Input, p.Ordered)
		}
		p.Answerable = canAnswer && h.settings.AnswerPermissions && (!p.Question || p.questions != nil)
		s.Pending, s.Status, s.WaitingFor, s.ActiveAt = &p, Waiting, "permission prompt", p.At
		if p.Answerable {
			h.waiters[p.ID] = make(chan Decision, 1)
		}
		if h.permissionRoots == nil {
			h.permissionRoots = map[string]*permissionReference{}
		}
		h.permissionRoots[p.ID] = &permissionReference{root: sessionID, run: s.run}
	})
	return p
}

// Await waits up to d for the user's decision in Slorp. ok is false when the
// wait ran out with the request still undecided. A request Slorp is not
// answering, or that the session has moved past, resolves at once to Terminal.
func (h *Hub) Await(ctx context.Context, requestID string, d time.Duration) (dec Decision, ok bool) {
	h.mu.Lock()
	ch, held := h.waiters[requestID]
	if held && len(ch) == 0 && !h.asking(requestID) {
		// The terminal answered, or the turn ended; nothing is waiting any more.
		delete(h.waiters, requestID)
		h.settle(requestID)
		held = false
	}
	ref := h.permissionRoots[requestID]
	if held && ref != nil {
		ref.awaiting++
	}
	h.mu.Unlock()
	if !held {
		return Decision{Behavior: Terminal}, true
	}
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if ref != nil {
			ref.awaiting--
		}
		h.finishPermission(requestID)
	}()
	select {
	case dec = <-ch:
		h.mu.Lock()
		delete(h.waiters, requestID)
		if ref != nil {
			ref.complete, ref.decided = true, false
			if ref.awaiting > 1 {
				select {
				case ch <- Decision{Behavior: Terminal}:
				default:
				}
			}
		}
		h.mu.Unlock()
		return dec, true
	case <-time.After(d):
	case <-ctx.Done():
	}
	return Decision{}, false
}

// settle tells whoever is waiting on a request that Slorp will not answer it.
func (h *Hub) settle(requestID string) {
	ref := h.permissionRoots[requestID]
	if ref != nil {
		ref.complete = true
	}
	if ch, ok := h.waiters[requestID]; ok && (ref == nil || !ref.decided) {
		select {
		case ch <- Decision{Behavior: Terminal}:
		default:
		}
	}
	h.finishPermission(requestID)
}

func (h *Hub) asking(requestID string) bool {
	for _, s := range h.sessions {
		if s.Pending != nil && s.Pending.ID == requestID {
			return true
		}
	}
	return false
}

// Decide allows or denies a request from Slorp, and reports whether it took.
// It does not when the request is no longer waiting, or is a question being
// allowed: Answer answers those.
func (h *Hub) Decide(requestID string, dec Decision) bool {
	dec.Answers = nil
	dec.Selections = nil
	return h.decide(requestID, func(p *Pending) (Decision, bool) { return dec, !(p.Question && dec.Behavior == Allow) })
}

// Answer answers a question from Slorp with what the user picked, by label,
// for each of its questions by text, and reports whether it took. It does
// not when the request is no longer waiting or the picks do not answer it:
// every question needs a pick from its own options, and only one unless it
// takes several.
func (h *Hub) Answer(requestID string, picks map[string][]string) bool {
	return h.decide(requestID, func(p *Pending) (Decision, bool) {
		if p.Ordered || len(p.questions) == 0 || len(picks) != len(p.questions) {
			return Decision{}, false
		}
		answers := map[string]string{}
		for _, q := range p.questions {
			got := picks[q.text]
			if len(got) == 0 || (len(got) > 1 && !q.many) {
				return Decision{}, false
			}
			var chosen []string
			for _, label := range q.labels {
				if slices.Contains(got, label) {
					chosen = append(chosen, label)
				}
			}
			if len(chosen) != len(got) {
				return Decision{}, false
			}
			answers[q.text] = strings.Join(chosen, ", ")
		}
		return Decision{Behavior: Allow, Answers: answers}, true
	})
}

// AnswerSelections answers an ordered question prompt, retaining each label
// and allowing custom text only for questions that explicitly accept it.
func (h *Hub) AnswerSelections(requestID string, picks [][]string) bool {
	return h.decide(requestID, func(p *Pending) (Decision, bool) {
		if !p.Ordered || len(p.questions) == 0 || len(picks) != len(p.questions) {
			return Decision{}, false
		}
		answers := make([][]string, len(picks))
		for i, q := range p.questions {
			if len(picks[i]) == 0 || (!q.many && len(picks[i]) != 1) {
				return Decision{}, false
			}
			for _, label := range picks[i] {
				if strings.TrimSpace(label) == "" || len(label) > 8000 || (!q.custom && !slices.Contains(q.labels, label)) || slices.Contains(answers[i], label) {
					return Decision{}, false
				}
				answers[i] = append(answers[i], label)
			}
		}
		return Decision{Behavior: Allow, Selections: answers}, true
	})
}

// decide passes on to the bridge the decision with makes of a request Slorp
// may answer, and reports whether it did.
func (h *Hub) decide(requestID string, with func(*Pending) (Decision, bool)) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.Pending == nil || s.Pending.ID != requestID || !s.Pending.Answerable {
			continue
		}
		dec, ok := with(s.Pending)
		if !ok {
			return false
		}
		select {
		case h.waiters[requestID] <- dec:
		default:
			return false
		}
		if ref := h.permissionRoots[requestID]; ref != nil {
			ref.complete, ref.decided = true, true
			ref.decisionDeadline = time.Now().Add(permissionDecisionGrace)
		}
		s.Pending, s.Status, s.WaitingFor = nil, Working, ""
		h.changed()
		return true
	}
	return false
}

// Done records that a request was settled outside Slorp.
func (h *Hub) Done(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settle(requestID)
	for _, s := range h.sessions {
		if s.Pending != nil && s.Pending.ID == requestID {
			s.Pending, s.Status, s.WaitingFor = nil, Working, ""
			h.changed()
		}
	}
}

// Settings returns the user's preferences, as a copy the caller may change.
func (h *Hub) Settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings.clone()
}

// clone returns s sharing nothing the hub changes in place.
func (s Settings) clone() Settings {
	s.Integrations, s.Prompts.Tasks, s.Rewritten = maps.Clone(s.Integrations), maps.Clone(s.Prompts.Tasks), maps.Clone(s.Rewritten)
	return s
}

// SetSettings replaces the user's preferences, leaving which integrations
// are on to SetIntegration and what is written again to Rewrite. It returns
// the tasks for which what the model wrote is left behind its instructions,
// which is none of them where the user has such writing always written
// again, or an error wrapping ErrNotSaved when nothing is changed.
func (h *Hub) SetSettings(s Settings) ([]string, error) {
	return h.UpdateSettings(func(now *Settings) error {
		*now = s
		return nil
	})
}

// UpdateSettings has change edit a copy of the user's preferences as they
// are, and keeps what comes of it as SetSettings would, all under the hub's
// lock: no other change comes between. Nothing changes when change fails.
func (h *Hub) UpdateSettings(change func(*Settings) error) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	said := h.settings.clone()
	if err := change(&said); err != nil {
		return nil, err
	}
	s, kept := said.clone(), h.settings.clone()
	s.Integrations, s.IntegrationsRevision, s.Rewritten = kept.Integrations, kept.IntegrationsRevision, kept.Rewritten
	s.Prompts.Tasks = maps.Clone(s.Prompts.Tasks)
	if s.Prompts.Tasks == nil {
		s.Prompts.Tasks = map[string]string{}
	}
	s.Prompts.Rewrite = cmp.Or(s.Prompts.Rewrite, RewriteAsk)
	behind := h.settings.Prompts.behind(s.Prompts)
	if s.Prompts.Rewrite == RewriteAlways {
		for _, t := range behind {
			s.Rewritten[t]++
		}
		behind = nil
	}
	if err := h.keepSettings(s); err != nil {
		return nil, err
	}
	return behind, nil
}

// Rewrite has the model write again what it wrote for tasks, or with none
// for every task: its descriptions of open sessions with their filings,
// roles and effort notes, and its briefs of pull requests. Days of the
// journal and earlier history are written only when asked for.
func (h *Hub) Rewrite(tasks ...string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(tasks) == 0 {
		tasks = []string{ActDescribe, ActBrief}
	}
	s := h.settings.clone()
	for _, t := range leading(tasks) {
		s.Rewritten[t]++
	}
	return h.keepSettings(s)
}

// rewritten marks the times the model has been asked to write again for
// task: empty until it is, so that what was written before stands.
func (h *Hub) rewritten(task string) string {
	if n := h.settings.Rewritten[task]; n > 0 {
		return "~" + strconv.Itoa(n)
	}
	return ""
}

// SetIntegration turns one integration on or off. Every session is then
// described again, since what the model can look up has changed.
func (h *Hub) SetIntegration(id string, on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settings.Integrations[id] == on {
		return nil
	}
	s := h.settings.clone()
	s.Integrations[id] = on
	s.IntegrationsRevision++
	return h.keepSettings(s)
}

// ErrNotSaved is wrapped by the error of a change to the settings or a reply
// that could not be saved, which is then not made.
var ErrNotSaved = errors.New("could not be saved")

// keepSettings saves s, and only then makes it the user's preferences. s
// shares nothing with the settings it replaces.
func (h *Hub) keepSettings(s Settings) error {
	if err := h.cache.Put("setting", "main", s); err != nil {
		return fmt.Errorf("the settings %w: %w", ErrNotSaved, err)
	}
	h.settings = s
	h.last = ""
	h.changed()
	return nil
}

// Looked records a lookup for the user to see.
func (h *Hub) Looked(l Lookup) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lookups = append(h.lookups, l)
	if n := len(h.lookups); n > keptLookups {
		h.lookups = h.lookups[n-keptLookups:]
	}
	h.last = ""
	h.changed()
}

// SetFeed records what an integration's feed found: the things that need the
// user and the pull requests it listed. When the collection failed, what it
// found the time before is kept.
func (h *Hub) SetFeed(source string, found []Item, pulls []integrations.PullRequest, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.feeds[source]
	if f == nil {
		f = &feed{}
		h.feeds[source] = f
	}
	f.At, f.Error = time.Now(), ""
	if err != nil {
		f.Error = err.Error()
	} else {
		f.items = make([]Item, len(found))
		for i, it := range found {
			it.Source, it.SessionID, it.Replies = source, "", []string{}
			f.items[i] = it
		}
		f.pulls = pulls
		h.forget(source, pulls)
	}
	f.Count = len(f.items)
	h.last = ""
	h.changed()
}

// DropFeed forgets an integration's feed, as when it is turned off.
func (h *Hub) DropFeed(source string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.feeds[source]; ok {
		delete(h.feeds, source)
		h.last = ""
		h.changed()
	}
}

// SetClones records which repositories are checked out in which of the
// efforts' directories, by directory. Feed items are filed by it.
func (h *Hub) SetClones(byDir map[string][]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clones = byDir
	h.changed()
}

// SetLimits records an agent's reading of the user's usage windows, taken
// at the given time, unless a later reading is already held. A reading of
// unknown age, with a zero time, only fills in where none is held.
func (h *Hub) SetLimits(agent string, at time.Time, windows []Limit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if held := h.limits[agent]; len(windows) == 0 || len(held) > 0 && !at.After(held[0].At) {
		return
	}
	for i := range windows {
		windows[i].Agent, windows[i].At = agent, at
	}
	if h.limits == nil {
		h.limits = map[string][]Limit{}
	}
	h.limits[agent] = windows
	h.last = ""
	h.changed()
}

// limited returns the usage windows that have not started over since they
// were read, by agent.
func (h *Hub) limited() []Limit {
	out := []Limit{}
	for _, agent := range slices.Sorted(maps.Keys(h.limits)) {
		for _, l := range h.limits[agent] {
			if l.ResetsAt.IsZero() || l.ResetsAt.After(time.Now()) {
				out = append(out, l)
			}
		}
	}
	return out
}

// SetCheckouts records which repositories are checked out in which of the
// sessions' directories, by directory. Sessions are matched to their pull
// requests by it.
func (h *Hub) SetCheckouts(byDir map[string][]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkouts = byDir
	h.changed()
}

// Cwds lists the directories the sessions are working in.
func (h *Hub) Cwds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, s := range h.sessions {
		if s.Cwd != "" && !slices.Contains(out, s.Cwd) {
			out = append(out, s.Cwd)
		}
	}
	slices.Sort(out)
	return out
}

// pullOn names the one of pulls a session is working on, or "".
func (h *Hub) pullOn(s *Session, pulls []PullRequest) string {
	if p, ok := h.pullAt(s.Cwd, s.Branch, pulls); ok {
		return p.Repo + "#" + strconv.Itoa(p.Number)
	}
	return ""
}

// pullAt finds the one of pulls made from branch in a repository checked
// out in dir. A pull request whose branch has its base's name can only come
// from a fork, and is found for none.
func (h *Hub) pullAt(dir, branch string, pulls []PullRequest) (PullRequest, bool) {
	for _, p := range pulls {
		held := func(repo string) bool { return strings.EqualFold(repo, p.Repo) }
		if branch != "" && p.Branch == branch && p.Branch != p.Base && slices.ContainsFunc(h.checkouts[dir], held) {
			return p, true
		}
	}
	return PullRequest{}, false
}

// fed returns every feed's items, and one for each question or pushback
// the model found in the discussions of its pull requests, each filed under
// the effort its repository belongs to.
func (h *Hub) fed() []Item {
	sources := make([]string, 0, len(h.feeds))
	for id := range h.feeds {
		sources = append(sources, id)
	}
	sort.Strings(sources)
	out := []Item{}
	for _, id := range sources {
		for _, it := range h.feeds[id].items {
			it.Effort, it.EffortVia = h.filePull(it.Repo, it.Pull)
			out = append(out, it)
		}
		for _, p := range h.feeds[id].pulls {
			pr := PullRequest{PullRequest: p, Source: id}
			pr.Effort, pr.EffortVia = h.filePull(p.Repo, pullName(p))
			h.discuss(&pr)
			out = append(out, raised(pr)...)
		}
	}
	return out
}

// pulls returns every feed's pull requests, the most pressing of the user's
// moves first and those waiting on someone else last, the most recently
// changed first within each.
func (h *Hub) pulls() []PullRequest {
	out := []PullRequest{}
	for id, f := range h.feeds {
		for _, p := range f.pulls {
			pr := PullRequest{PullRequest: p, Source: id}
			pr.Effort, pr.EffortVia = h.filePull(p.Repo, pullName(p))
			h.discuss(&pr)
			out = append(out, pr)
		}
	}
	rank := map[string]int{"blocking": 3, "awaiting": 2, "whenever": 1}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := rank[out[i].Urgency], rank[out[j].Urgency]; a != b {
			return a > b
		}
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Repo+"#"+strconv.Itoa(out[i].Number) < out[j].Repo+"#"+strconv.Itoa(out[j].Number)
	})
	return out
}

// pullName names a pull request as "owner/name#number".
func pullName(p integrations.PullRequest) string {
	return p.Repo + "#" + strconv.Itoa(p.Number)
}

// filePull reports the effort something from a repository belongs under:
// where the user filed the pull request it is about, if pull names one, or
// else where its repository's items go. Both are empty when neither the user
// nor a directory rule says.
func (h *Hub) filePull(repo, pull string) (effort, via string) {
	var held []string
	for dir, repos := range h.clones {
		for _, r := range repos {
			if strings.EqualFold(r, repo) {
				held = append(held, dir)
				break
			}
		}
	}
	resolve := h.kb.ResolveRepo
	if pull != "" {
		repo, resolve = pull, h.kb.ResolvePull
	}
	if effort, via, ok := resolve(repo, held); ok {
		return effort, via
	}
	return "", ""
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Sessions returns a snapshot ordered by how much each needs the user.
func (h *Hub) Sessions() []Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot()
}

// State is everything the web UI draws.
type State struct {
	Sessions   []Session    `json:"sessions"`
	Items      []Item       `json:"items"`
	Efforts    []Effort     `json:"efforts"`
	Summarizer Summarizer   `json:"summarizer"`
	Settings   Settings     `json:"settings"`
	Knowledge  kb.Knowledge `json:"knowledge"`
	// Lookups are the most recent ones, newest last.
	Lookups []Lookup `json:"lookups"`
	// Actions are the last few things the local model did, the latest first.
	Actions []Action `json:"actions"`
	// Feeds says how each integration's last collection went, by integration.
	Feeds map[string]FeedStatus `json:"feeds"`
	// PullRequests are the open ones the integrations listed.
	PullRequests []PullRequest `json:"pullRequests"`
	// Limits are the user's usage windows as the agents last read them.
	Limits         []Limit          `json:"limits"`
	ProviderLimits []ProviderLimits `json:"providerLimits"`
	// Journal lists the days that have been wrapped up or are waiting to
	// be, the latest first.
	Journal []JournalDay `json:"journal"`
	// Archive lists the sessions that have closed, the latest first.
	Archive []Archived `json:"archive"`
	// Plugins says, for each agent that can be asked, whether it has the
	// Slorp plugin.
	Plugins map[string]Plugin `json:"plugins"`
	// Release is the release of Slorp running, and the newer one when
	// there is one.
	Release Release `json:"release"`
}

func (h *Hub) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	feeds := map[string]FeedStatus{}
	for id, f := range h.feeds {
		feeds[id] = f.FeedStatus
	}
	return State{Sessions: sessions, Items: append(items(sessions), h.fed()...), Efforts: h.efforts(sessions), Summarizer: h.summ,
		Settings: h.settings.clone(), Knowledge: h.kb.Knowledge(), Lookups: append([]Lookup{}, h.lookups...), Actions: h.acted(recentActions), Feeds: feeds, PullRequests: h.pulls(), Limits: h.limited(), ProviderLimits: h.providerLimited(time.Now()), Journal: h.journal(), Archive: h.archives(), Plugins: maps.Clone(h.plugins), Release: h.release}
}

func (h *Hub) efforts(sessions []Session) []Effort {
	active := map[string]time.Time{}
	latest := func(effort string, at time.Time) {
		if at.After(active[effort]) {
			active[effort] = at
		}
	}
	for id, d := range h.archive {
		latest(h.closedEffort(id, d), d.Ended)
	}
	for _, s := range sessions {
		latest(s.Effort, s.ActiveAt)
	}
	out := []Effort{}
	for _, e := range h.kb.Efforts() {
		v := Effort{Effort: e, ActiveAt: active[e.ID]}
		for _, s := range sessions {
			if s.Effort == e.ID {
				v.Line, v.Stands = h.notes[e.ID].Line, h.notes[e.ID].Stands
				break
			}
		}
		out = append(out, v)
	}
	return out
}

// MergeEfforts moves everything filed under one effort to another and
// removes the first: its filings, directories and questions, its closed
// sessions, and its part of each day of the journal.
// What the model wrote of where it stood is dropped.
func (h *Hub) MergeEfforts(from, into string) error {
	if err := h.kb.Merge(from, into); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.guess == from {
			s.guess = into
		}
	}
	for id, d := range h.archive {
		if d.Effort == from {
			d.Effort = into
			h.archive[id] = d
			h.warn(h.cache.Put("session", id, d))
		}
	}
	for day, w := range h.wrapups {
		if !slices.ContainsFunc(w.Efforts, func(e WrapupEffort) bool { return e.ID == from }) {
			continue
		}
		w.Efforts = slices.Clone(w.Efforts)
		for i := range w.Efforts {
			if w.Efforts[i].ID == from {
				w.Efforts[i].ID = into
			}
		}
		h.wrapups[day] = w
		h.warn(h.cache.Put("wrapup", day, w))
	}
	delete(h.notes, from)
	h.warn(h.cache.Delete("note", from))
	h.changed()
	return nil
}

// readReplies reads the replies kept, and says why when what it read is not
// all of them.
func (h *Hub) readReplies() ([]*Reply, error) {
	var out []*Reply
	var bad error
	err := h.cache.Each("reply", func(id string, body []byte) {
		var r Reply
		if err := json.Unmarshal(body, &r); err != nil {
			bad = fmt.Errorf("decoding reply %s: %w", id, err)
			return
		}
		if r.ID != id || r.SessionID == "" || !slices.Contains([]ReplyState{Queued, Claimed, Delivered, Dropped}, r.State) {
			bad = fmt.Errorf("invalid stored reply %s", id)
			return
		}
		out = append(out, &r)
	})
	if err != nil {
		bad = fmt.Errorf("loading replies: %w", err)
	}
	return out, bad
}

// File makes a change to what the user said of a session, by edit, once the
// session is kept: an open or closed one is saved as it stands first, and
// one whose kept record could not be read is not kept; when the session is
// not kept, the error wraps ErrNotSaved and edit is not made. A session
// Slorp does not know, such as one another has taken up, is refused with
// ErrNoSuchSession. Nothing takes the session up meanwhile, so the change
// goes with it when something does. edit must not call the Hub.
func (h *Hub) File(sessionID string, edit func() error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var err error
	if s := h.sessions[sessionID]; s != nil {
		err = h.checkpoint(s)
	} else if _, held := h.unread[sessionID]; held {
		err = errors.New("what is kept of it could not be read")
	} else if d, archived := h.archive[sessionID]; archived {
		err = h.cache.Put("session", sessionID, d)
	} else if _, unsettled := h.unsettled[sessionID]; !unsettled {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, sessionID)
	}
	if err != nil {
		return fmt.Errorf("session %s %w: %w", sessionID, ErrNotSaved, err)
	}
	return edit()
}

// Touch republishes state after the knowledge base changed, saving each
// open session the user filed or gave a role as it now stands.
func (h *Hub) Touch() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.managed || h.kb.Said(s.ID) {
			h.remember(s)
		}
	}
	h.changed()
}

// routeKey identifies what a model's filing guess was based on.
func (h *Hub) routeKey(s *Session) string {
	return s.insightKey + ":" + strconv.Itoa(h.kb.Revision()) + h.rewritten(ActFile)
}

// Unrouted lists described sessions that still need the model to guess an effort.
func (h *Hub) Unrouted() []Unrouted {
	efforts := slices.DeleteFunc(h.kb.Efforts(), func(e kb.Effort) bool { return e.Shelved })
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unrouted
	if len(efforts) == 0 {
		return out
	}
	for _, s := range h.sessions {
		if s.Insight == nil || s.insightKey != s.key || s.guessKey == h.routeKey(s) {
			continue
		}
		if _, _, ok := h.kb.Resolve(s.ID, s.Cwd); !ok {
			know := h.kb.Knowledge()
			c := *s
			c.Subagents = cloneSubagents(s.Subagents)
			out = append(out, Unrouted{Session: c, Key: h.routeKey(s), Efforts: efforts, Hints: append(texts(know.Context), texts(know.Routing)...)})
		}
	}
	return out
}

// SetGuess records the model's filing for a session; an empty effortID means
// it found no effort the session clearly belongs to.
func (h *Hub) SetGuess(sessionID, key, effortID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.guess, s.guessKey = effortID, key
		h.remember(s)
		h.changed()
	}
}

// Unroled lists described sessions that still need the model to guess a role.
func (h *Hub) Unroled() []Unroled {
	roles := h.kb.Knowledge().Roles
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unroled
	if len(roles) == 0 {
		return out
	}
	for _, s := range h.sessions {
		key := s.insightKey + ":" + strconv.Itoa(h.kb.RoleRevision()) + h.rewritten(ActRole)
		if s.insightKey != s.key || s.roleKey == key {
			continue
		}
		if _, ok := h.kb.RoleOf(s.ID); !ok {
			c := *s
			c.Subagents = cloneSubagents(s.Subagents)
			out = append(out, Unroled{Session: c, Key: key, Roles: roles})
		}
	}
	return out
}

// SetRoleGuess records the model's choice of role for a session; an empty
// roleID means it found none the session clearly has.
func (h *Hub) SetRoleGuess(sessionID, key, roleID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.roleGuess, s.roleKey = roleID, key
		h.remember(s)
		h.changed()
	}
}

// Unnoted lists efforts with described sessions whose note is out of date.
func (h *Hub) Unnoted() []Unnoted {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	know := h.kb.Knowledge()
	var out []Unnoted
	for _, e := range h.kb.Efforts() {
		var members []Session
		var questions []string
		key := e.Name + "\x00" + e.About + h.rewritten(ActNote)
		for _, q := range know.Questions {
			if q.Effort == e.ID {
				questions = append(questions, q.Text)
				key += "\x00?" + q.Text
			}
		}
		for _, s := range sessions {
			if s.Effort == e.ID && s.Insight != nil {
				members = append(members, s)
				key += "\x00" + s.ID + ":" + s.insightKey
			}
		}
		sum := sha1.Sum([]byte(key))
		if k := hex.EncodeToString(sum[:]); len(members) > 0 && h.notes[e.ID].Key != k {
			out = append(out, Unnoted{Effort: e, Members: members, Key: k, Context: texts(know.Context), Questions: questions})
		}
	}
	return out
}

func (h *Hub) SetNote(effortID, key string, n Note) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes[effortID] = note{Key: key, Note: n}
	h.warn(h.cache.Put("note", effortID, h.notes[effortID]))
	h.last = ""
	h.changed()
}

// Undescribed lists sessions that have settled on content no insight covers.
// Working sessions are left out: their content is still moving.
func (h *Hub) Undescribed() []Undescribed {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Undescribed
	for _, s := range h.sessions {
		hasContent := s.LastReply != "" || s.Pending != nil
		outdated := s.key != s.insightKey ||
			(s.Insight != nil && len(s.Insight.Lookups) > 0 && time.Since(s.insightAt) > lookupTTL)
		if hasContent && outdated && time.Since(s.keySince) >= quiet && (s.Status != Working || s.Pending != nil) {
			c := *s
			c.Subagents = cloneSubagents(s.Subagents)
			out = append(out, Undescribed{Session: c, Key: s.key, Context: texts(h.kb.Knowledge().Context)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.ActiveAt.After(out[j].Session.ActiveAt) })
	return out
}

// SetInsight attaches an insight written for the content identified by key.
func (h *Hub) SetInsight(sessionID, key string, in Insight) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.Insight, s.insightKey, s.insightAt = &in, key, time.Now()
		if s.pinned == "" {
			s.pinned = in.Title
		}
		// A description of what the session has since moved on from says
		// nothing about its latest moment.
		if i := lastSaid(s.Timeline); i >= 0 && key == s.key {
			s.Timeline = slices.Clone(s.Timeline)
			s.Timeline[i].Line, s.Timeline[i].Summary = in.Title, in.Summary
		}
		h.remember(s)
		h.changed()
	}
}

// Rename gives a session the name it goes by, or with an empty name leaves
// that to its agent and the model again. It reports whether the session is
// known.
func (h *Hub) Rename(sessionID, name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[sessionID]
	if ok {
		s.given = name
		h.remember(s)
		h.changed()
	}
	return ok
}

func (s *Session) label() (name, via string) {
	switch {
	case s.given != "":
		return s.given, kb.ViaUser
	case s.Named != "":
		return s.Named, kb.ViaUser
	case s.Title != "":
		return s.Title, ""
	case s.pinned != "":
		return s.pinned, ViaGuess
	case s.Name != "":
		return s.Name, ""
	}
	return s.ID[:min(8, len(s.ID))], ""
}

// Summarizer returns which local model is in use and where.
func (h *Hub) Summarizer() Summarizer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.summ
}

func (h *Hub) SetSummarizer(s Summarizer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.summ != s {
		h.summ = s
		h.last = ""
		h.changed()
	}
}

// described reports whether the session's insight was written about what it
// is showing now. An insight written under older standing context still
// counts, so that editing the context does not blank everything it replaces.
func (s *Session) described() bool {
	return s.Insight != nil && contentOf(s.insightKey) == contentOf(s.key)
}

// contentOf strips from a content key the revisions of what the model reads
// besides the session: the standing context and the integrations turned on.
func contentOf(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

func texts(facts []kb.Fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Text
	}
	return out
}

// contentKey identifies what a session is currently showing the user.
// track opens a moment in a session's timeline when the user has said
// something new, and when its agent has compacted it. What happened before
// the latest moment is not new, however late its agent reports it; nor is
// an untimed repeat of a recent moment.
func (h *Hub) track(s *Session, now time.Time) {
	var fresh []Moment
	if c := s.Compacted; c != nil && !slices.ContainsFunc(s.Timeline, func(m Moment) bool { return sameCompaction(m.Compacted, c) }) {
		fresh = append(fresh, Moment{At: c.At, Compacted: c})
	}
	n := len(s.Timeline)
	if prompt := momentPrompt(s.LastPrompt); prompt != "" {
		at := s.PromptAt
		if i := slices.IndexFunc(s.Timeline, func(m Moment) bool { return aliasesPrompt(m.Aliases, at, prompt) }); i >= 0 {
			at, prompt = s.Timeline[i].At, s.Timeline[i].Prompt
		}
		same := func(m Moment) bool { return m.Compacted == nil && samePrompt(m.Prompt, prompt) }
		if at.IsZero() && !slices.ContainsFunc(s.Timeline[max(0, n-untimedLookback):], same) {
			at = now
		}
		if !at.IsZero() && !(n > 0 && same(s.Timeline[n-1])) {
			fresh = append(fresh, Moment{At: at, Prompt: prompt})
		}
	}
	slices.SortStableFunc(fresh, func(a, b Moment) int { return a.At.Compare(b.At) })
	for _, m := range fresh {
		if last := len(s.Timeline) - 1; last < 0 || m.At.After(s.Timeline[last].At) {
			s.Timeline = append(s.Timeline, m)
		}
	}
	if len(s.Timeline) == n {
		return
	}
	s.Timeline = s.Timeline[max(0, len(s.Timeline)-timelineLimit):]
	h.remember(s)
}

// lastSaid is the place in a timeline of the latest thing the user said, -1
// when they have said nothing.
func lastSaid(timeline []Moment) int {
	for i := len(timeline) - 1; i >= 0; i-- {
		if timeline[i].Compacted == nil {
			return i
		}
	}
	return -1
}

// samePrompt reports whether a and b are one prompt, either of them
// perhaps cut short with a mark where it was cut.
func samePrompt(a, b string) bool {
	cut := func(short, whole string) bool {
		return strings.HasSuffix(short, "…") && strings.HasPrefix(whole, strings.TrimSpace(strings.TrimSuffix(short, "…")))
	}
	return a == b || cut(a, b) || cut(b, a)
}

// tidy folds each moment into the one before it when both are one prompt,
// keeping the fuller wording and the later line, and drops a compaction
// told twice over.
func tidy(timeline []Moment) []Moment {
	var out []Moment
	for _, m := range timeline {
		n := len(out)
		if n > 0 && sameCompaction(m.Compacted, out[n-1].Compacted) {
			continue
		}
		if n == 0 || m.Compacted != nil || out[n-1].Compacted != nil || (m.Recorded || out[n-1].Recorded) && !m.At.Equal(out[n-1].At) || !samePrompt(out[n-1].Prompt, m.Prompt) {
			out = append(out, m)
			continue
		}
		if strings.HasSuffix(out[n-1].Prompt, "…") {
			out[n-1].Prompt = m.Prompt
		}
		out[n-1].Recorded = out[n-1].Recorded || m.Recorded
		for _, alias := range m.Aliases {
			if !aliasesPrompt(out[n-1].Aliases, alias.At, alias.Prompt) {
				out[n-1].Aliases = append(out[n-1].Aliases, alias)
			}
		}
		if m.Line != "" {
			out[n-1].Line, out[n-1].Summary = m.Line, m.Summary
		}
	}
	return out
}

func contentKey(s *Session) string {
	state, pending := "open", ""
	switch {
	case s.Pending != nil:
		state, pending = "permission", s.Pending.Tool+"\x00"+s.Pending.Summary
	case s.Status == Waiting, s.Status == Failed:
		state = string(s.Status)
	}
	sum := sha1.Sum([]byte(state + "\x00" + pending + "\x00" + s.LastPrompt + "\x00" + s.LastReply))
	return hex.EncodeToString(sum[:])
}

// items derives what needs the user from the sessions. Permission prompts,
// blocked and failed sessions are items on their state alone; a question or
// finished work is an item only once an insight written for the current
// content says so. An insight that calls for permission when no prompt is
// pending is taken as a question. A role the user gave a session can keep it
// from asking: altogether, or unless it is stopped.
func items(sessions []Session) []Item {
	out := []Item{}
	for _, s := range sessions {
		it := Item{ID: s.ID[:min(8, len(s.ID))] + "-" + s.key[:min(10, len(s.key))], SessionID: s.ID, At: s.ActiveAt, Replies: []string{},
			Effort: s.Effort, EffortVia: s.EffortVia}
		if s.attention == kb.AttentionNone {
			continue
		}
		it.Title = s.Label
		fresh := s.described()
		if fresh {
			it.Subtitle, it.Summary, it.Ask = s.Insight.Title, s.Insight.Summary, s.Insight.Ask
		}
		switch {
		case s.Pending != nil && s.Pending.Question:
			it.Kind, it.Urgency = KindQuestion, "awaiting"
			if it.Ask == "" {
				it.Ask = cmp.Or(s.Pending.Summary, "This session asked you something. Answer it in its terminal.")
			}
		case s.Pending != nil:
			it.Kind, it.Urgency = KindPermission, "awaiting"
			if it.Ask == "" {
				it.Ask = "Allow or deny " + s.Pending.Tool + ": " + s.Pending.Summary
			}
		case s.Status == Failed:
			it.Kind, it.Urgency = KindFailed, "awaiting"
			if it.Ask == "" {
				it.Ask = "This session failed. Look at what went wrong."
			}
		case s.Status == Waiting:
			it.Kind, it.Urgency = KindQuestion, "awaiting"
			if it.Ask == "" {
				it.Ask = "This session is stopped until you respond."
			}
			if fresh {
				it.Replies = s.Insight.Replies
			}
		case s.attention == kb.AttentionStopped:
			continue
		case fresh && s.Status != Working && (s.Insight.Kind == KindQuestion || s.Insight.Kind == KindPermission):
			it.Kind, it.Urgency, it.Replies = KindQuestion, "awaiting", s.Insight.Replies
		case fresh && s.Status != Working && s.Insight.Kind == KindReview:
			it.Kind, it.Urgency, it.Replies = KindReview, "whenever", s.Insight.Replies
		case s.Status != Working && s.ContextWindow > 0 && s.ContextTokens*100 >= s.ContextWindow*contextWarn:
			// One item for as long as the session lasts, so that dismissing it
			// is not undone by the session's next exchange.
			it.ID, it.Kind, it.Urgency = s.ID[:min(8, len(s.ID))]+"-context", KindContext, "whenever"
			it.Ask = "Its context is " + strconv.Itoa(s.ContextTokens*100/s.ContextWindow) + "% full. Compact it or wrap up before it runs out of room."
		default:
			continue
		}
		if it.Replies == nil {
			it.Replies = []string{}
		}
		out = append(out, it)
	}
	return out
}

func (h *Hub) snapshot() []Session {
	rank := map[Status]int{Waiting: 0, Failed: 1, Working: 2, Idle: 3, Done: 4}
	out := make([]Session, 0, len(h.sessions))
	pulls := h.pulls()
	for _, s := range h.sessions {
		c := *s
		c.CanStop = s.CanStop || s.Agent == "claude" && s.Job != ""
		c.CanReply = time.Since(s.bridgeSeen) < bridgeTTL
		c.Subagents = h.sessionSubagents(s, c.CanReply)
		c.Label, c.LabelVia = s.label()
		c.Pull = h.pullOn(s, pulls)
		c.Backfilling = len(h.backfill[s.ID])
		c.Effort, c.EffortVia = h.filed(s)
		if role, ok := h.kb.RoleOf(s.ID); ok {
			c.Role, c.RoleVia, c.attention = role.ID, kb.ViaUser, role.Attention
		} else if s.roleGuess != "" && h.kb.HasRole(s.roleGuess) {
			c.Role, c.RoleVia = s.roleGuess, ViaGuess
		}
		for _, r := range h.replies {
			if r.SessionID == s.ID {
				c.Replies = append(c.Replies, *r)
			}
		}
		if n := len(c.Replies); n > 5 {
			c.Replies = c.Replies[n-5:]
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		if !out[i].ActiveAt.Equal(out[j].ActiveAt) {
			return out[i].ActiveAt.After(out[j].ActiveAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ErrNoSuchSession is Enqueue's error for a session the hub does not know.
var ErrNoSuchSession = errors.New("no such session")

// Enqueue queues text for a session. The reply is saved before it is queued:
// a reply that could not be saved is not queued, and the error wraps
// ErrNotSaved.
func (h *Hub) Enqueue(sessionID, text string) (Reply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[sessionID]
	if !ok {
		return Reply{}, ErrNoSuchSession
	}
	r := &Reply{ID: newID(), SessionID: sessionID, Text: text, State: Queued, CreatedAt: time.Now(), Owner: OwnerBridge}
	if s.run != "" {
		r.Owner = OwnerRun
	}
	doc := h.doc(s)
	kept, err := fingerprint(doc)
	if err != nil {
		return Reply{}, fmt.Errorf("the reply %w: %w", ErrNotSaved, err)
	}
	if kept == s.persisted {
		err = h.saveReply(*r)
	} else if err = h.cache.PutAll(store.Doc{Kind: "session", ID: s.ID, Value: doc}, store.Doc{Kind: "reply", ID: r.ID, Value: *r}); err != nil {
		err = fmt.Errorf("the reply %w: %w", ErrNotSaved, err)
	} else {
		s.persisted = kept
	}
	if err != nil {
		return Reply{}, err
	}
	h.replies = append(h.replies, r)
	h.changed()
	return *r, nil
}

// Claim hands a session's queued replies to its bridge, in order, and claims
// each at most once: a reply is saved as claimed before it is handed out,
// and is not handed out again, whether or not the bridge received it. Claim
// stops at the first that could not be saved, which stays queued with those
// after it, and returns the replies claimed before it with an error wrapping
// ErrNotSaved.
func (h *Hub) Claim(sessionID string) ([]Reply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Reply
	var err error
	for _, r := range h.replies {
		if r.SessionID != sessionID || r.State != Queued {
			continue
		}
		claimed := *r
		claimed.State = Claimed
		if err = h.saveReply(claimed); err != nil {
			break
		}
		*r = claimed
		if s := h.sessions[sessionID]; s != nil && s.run != "" {
			if h.replyRuns == nil {
				h.replyRuns = map[string]string{}
			}
			h.replyRuns[r.ID] = s.run
		}
		out = append(out, claimed)
	}
	if len(out) > 0 {
		h.changed()
	}
	return out, err
}

// Ack records what became of a claimed reply. A reply that is not claimed,
// such as one already acknowledged, is left as it is without error. The new
// state is saved before it is recorded: on an error, which wraps ErrNotSaved,
// the reply stays claimed, by the run that claimed it.
func (h *Hub) Ack(replyID string, state ReplyState) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.replies {
		if r.ID != replyID || r.State != Claimed {
			continue
		}
		acked := *r
		acked.State = state
		if err := h.saveReply(acked); err != nil {
			return err
		}
		*r = acked
		h.rememberCompletedReference(h.replyRuns[replyID], replyID, true)
		delete(h.replyRuns, replyID)
		h.changed()
	}
	return nil
}

func (h *Hub) saveReply(r Reply) error {
	if err := h.cache.Put("reply", r.ID, r); err != nil {
		return fmt.Errorf("the reply %w: %w", ErrNotSaved, err)
	}
	return nil
}

// Subscribe returns a channel that receives a tick whenever the snapshot
// changes, and a function that ends the subscription.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) changed() {
	now := time.Now()
	context := ":" + strconv.Itoa(h.kb.ContextRevision()) + "." + strconv.Itoa(h.settings.IntegrationsRevision) + h.rewritten(ActDescribe)
	for _, s := range h.sessions {
		if k := contentKey(s) + context; k != s.key {
			s.key, s.keySince = k, now
		}
		h.track(s, now)
		if s.run != "" || s.indexed != childKey(childIndex(s.Subagents)) {
			h.remember(s)
		}
	}
	snap := h.snapshot()
	b, _ := json.Marshal(map[string]any{"s": snap, "i": items(snap), "f": h.fed(), "e": h.efforts(snap), "m": h.summ, "providerLimits": h.providerLimited(now)})
	if string(b) == h.last {
		return
	}
	h.last = string(b)
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
