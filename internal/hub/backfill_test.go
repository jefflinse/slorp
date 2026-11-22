package hub

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestBackfill(t *testing.T) {
	h, k, st := newHub(t)
	t0 := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	say := func(prompt string, when time.Time) {
		h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, Title: "Flange support", LastPrompt: prompt, PromptAt: when}})
	}
	timeline := func(h *Hub) []Moment { return session(t, h, "s").Timeline }

	if _, ok := h.Backfill("nobody", nil); ok {
		t.Fatal("Backfill took a session that is not open")
	}

	// Slorp saw the second and third exchanges as they happened, a little
	// late, and the model described the second.
	say("second thing", at(2).Add(3*time.Second))
	h.SetInsight("s", h.sessions["s"].key, Insight{Title: "Second done", Summary: "It did the second."})
	say("an aside the record lacks", at(2).Add(30*time.Minute))
	say("third thing", at(3).Add(3*time.Second))
	say("said after the record was read", at(5))

	record := []Exchange{
		{At: at(1), Prompt: "first\n  thing", Reply: "Did the first."},
		{At: at(2), Prompt: "second thing", Reply: "Did the second."},
		{At: at(3), Prompt: "third thing", Reply: "Did the third."},
		{At: at(4), Prompt: "fourth thing, still going"},
		{At: at(4), Prompt: "   "},
	}
	if queued, _ := h.Backfill("s", append([]Exchange{{At: at(0), Compacted: &Compaction{At: at(0), Before: 7}}}, record...)); queued != 2 {
		t.Fatalf("with a compaction in the record, Backfill queued %d; want only the two exchanges", queued)
	}
	if got := timeline(h); len(got) != 7 || got[0].Compacted == nil || got[0].Compacted.Before != 7 || !got[0].At.Equal(at(0)) {
		t.Fatalf("timeline = %+v; want the record's compaction first of seven", got)
	}
	h.Backfill("s", []Exchange{{At: at(0), Compacted: &Compaction{At: at(0), Before: 7}}})
	if got := timeline(h); len(got) != 7 || got[1].Compacted != nil {
		t.Fatalf("timeline = %+v; want a compaction it already had kept once", got)
	}
	h.sessions["s"].Timeline = h.sessions["s"].Timeline[1:]
	queued, ok := h.Backfill("s", record)
	if !ok || queued != 2 {
		t.Fatalf("Backfill = %d, %v; want the two exchanges that were answered and never described", queued, ok)
	}
	want := []Moment{
		{At: at(1), Prompt: "first thing"},
		{At: at(2), Prompt: "second thing", Line: "Second done", Summary: "It did the second."},
		{At: at(2).Add(30 * time.Minute), Prompt: "an aside the record lacks"},
		{At: at(3), Prompt: "third thing"},
		{At: at(4), Prompt: "fourth thing, still going"},
		{At: at(5), Prompt: "said after the record was read"},
	}
	check := func(h *Hub, when string) {
		t.Helper()
		got := timeline(h)
		if len(got) != len(want) {
			t.Fatalf("%s: timeline = %+v, want %d moments", when, got, len(want))
		}
		for i := range want {
			if got[i].Prompt != want[i].Prompt || got[i].Line != want[i].Line || got[i].Summary != want[i].Summary || !got[i].At.Equal(want[i].At) {
				t.Errorf("%s: moment %d = %+v, want %+v", when, i, got[i], want[i])
			}
		}
	}
	check(h, "once backfilled")
	if got := session(t, h, "s").Backfilling; got != 2 {
		t.Errorf("backfilling = %d, want the two waiting", got)
	}

	waiting := h.Unfilled()
	if len(waiting) != 1 || waiting[0].SessionID != "s" || waiting[0].Label != "Flange support" || len(waiting[0].Exchanges) != 2 ||
		waiting[0].Exchanges[0].Reply != "Did the first." || waiting[0].Exchanges[1].Prompt != "third thing" {
		t.Fatalf("waiting = %+v; want the first and third exchanges, with what the agent answered", waiting)
	}
	// The third gets its line some other way before the model gets to it.
	h.sessions["s"].Timeline[3].Line = "Third done"
	before := h.State()
	h.SetFilled(waiting[0], []Filled{{N: 0, Line: " First done ", Summary: " It did the first. "}, {N: 1, Line: "Too late"}, {N: 7, Line: "No such exchange"}, {N: -1, Line: "Nor this"}})
	want[0].Line, want[0].Summary = "First done", "It did the first."
	want[3].Line = "Third done"
	check(h, "once the model answered")
	if got := before.Sessions[0].Timeline[0].Line; got != "" {
		t.Errorf("a state handed out earlier was changed under its reader: line %q", got)
	}
	if got := session(t, h, "s").Backfilling; got != 0 || len(h.Unfilled()) != 0 {
		t.Errorf("backfilling = %d with %d waiting, want none once every exchange was tried", got, len(h.Unfilled()))
	}
	h2 := New(k, st)
	h2.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, Title: "Flange support"}})
	check(h2, "after a restart")

	// Asking again keeps what is written and queues only what is not.
	record[3].Reply = "Did the fourth."
	if queued, _ := h.Backfill("s", record); queued != 1 || h.Unfilled()[0].Exchanges[0].Prompt != "fourth thing, still going" {
		t.Errorf("a second backfill queued %d, want only the exchange answered since", queued)
	}
	check(h, "after a second backfill")
	h.SetFilled(h.Unfilled()[0], []Filled{{N: 0, Line: "   ", Summary: "A summary with no line."}})
	if got := timeline(h)[4]; got.Line != "" || got.Summary != "" || len(h.Unfilled()) != 0 {
		t.Errorf("a blank line left the moment as %+v, with %d still waiting; want it left bare and not asked about again", got, len(h.Unfilled()))
	}
}

func TestBackfillTakesTheLatestAndAFewAtATime(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: "said since the record was read", PromptAt: time.Now()}, {ID: "other", Agent: agent, Status: Idle}})
	t0 := time.Now().Add(-100 * time.Hour)
	var record []Exchange
	for i := range BackfillLimit + 5 {
		record = append(record, Exchange{At: t0.Add(time.Duration(i) * time.Minute), Prompt: "step " + strconv.Itoa(i), Reply: "done"})
	}
	if queued, _ := h.Backfill("s", record); queued != BackfillLimit {
		t.Fatalf("queued %d, want the latest %d", queued, BackfillLimit)
	}
	if got := session(t, h, "s").Timeline; len(got) != timelineLimit || got[0].Prompt != "step 6" || got[timelineLimit-1].Prompt != "said since the record was read" {
		t.Fatalf("timeline has %d moments from %q to %q, want the latest %d", len(got), got[0].Prompt, got[len(got)-1].Prompt, timelineLimit)
	}
	for left := BackfillLimit; left > 0; left -= backfillBatch {
		waiting := h.Unfilled()
		if len(waiting) != 1 || len(waiting[0].Exchanges) != min(left, backfillBatch) || waiting[0].Exchanges[0].Prompt != "step "+strconv.Itoa(5+BackfillLimit-left) {
			t.Fatalf("with %d left, waiting = %+v; want the next %d, oldest first", left, waiting, backfillBatch)
		}
		h.SetFilled(waiting[0], nil)
	}
	if len(h.Unfilled()) != 0 {
		t.Error("exchanges are still waiting after each was tried")
	}

	h.Backfill("s", record)
	h.SetRoster(agent, []Session{{ID: "other", Agent: agent, Status: Idle}})
	if len(h.Unfilled()) != 0 {
		t.Error("a session that has gone is still being backfilled")
	}
}

func TestBackfillKeepsRepeatedPromptsAndCompactionIdentity(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	c := &Compaction{ID: "prt_compact", At: at.Add(time.Minute), Auto: true}
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: "again", PromptAt: at.Add(time.Second), Compacted: c}})
	h.sessions["s"].Timeline = []Moment{
		{At: at.Add(time.Second), Prompt: "again", Line: "Second answer", Summary: "Only the second exchange was observed."},
		{At: c.At, Compacted: c},
	}
	recorded := *c
	recorded.At = c.At.Add(-time.Millisecond)
	xs := []Exchange{
		{At: at, Prompt: "again", Reply: "First answer"},
		{At: at.Add(time.Second), Prompt: "again", Reply: "Second answer"},
		{At: recorded.At, Compacted: &recorded},
	}
	for range 2 {
		h.Backfill("s", xs)
		got := session(t, h, "s").Timeline
		if len(got) != 3 || got[0].Line != "" || got[1].Line != "Second answer" || got[2].Compacted.ID != c.ID {
			t.Fatalf("backfill duplicated or misattributed history: %+v", got)
		}
	}
	h = New(k, st)
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle}})
	h.Backfill("s", xs)
	got := session(t, h, "s").Timeline
	if len(got) != 3 || got[0].Line != "" || got[1].Line != "Second answer" {
		t.Fatalf("restart collapsed repeated prompts: %+v", got)
	}
}

func TestBackfillMigratesALegacyCompactionAndKeepsItsMeasurements(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	old := &Compaction{At: at.Add(time.Millisecond), Auto: true, Before: 1234, After: 100}
	report := func(h *Hub) { h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, Compacted: old}}) }
	report(h)
	record := []Exchange{{At: at, Compacted: &Compaction{ID: "prt_compact", At: at, Auto: true}}}
	check := func(h *Hub) {
		t.Helper()
		got := session(t, h, "s").Timeline
		if len(got) != 1 || got[0].Compacted.ID != "prt_compact" || got[0].Compacted.Before != 1234 || got[0].Compacted.After != 100 || !got[0].At.Equal(at) {
			t.Fatalf("legacy marker was duplicated or lost readings: %+v", got)
		}
	}
	h.Backfill("s", record)
	report(h)
	check(h)
	h.Backfill("s", record)
	check(h)
	h = New(k, st)
	report(h)
	check(h)
}

func TestBackfillReconcilesALiveReplayAndItsLaterSnapshots(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	replay := PromptAlias{At: at.Add(time.Minute), Prompt: "Original prompt\n\n[Attached image/png: image.png]"}
	oldBridge := func(h *Hub) {
		h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: replay.Prompt, PromptAt: replay.At}})
	}
	oldBridge(h)
	xs := []Exchange{{At: at, Prompt: "Original prompt", Reply: "Done", Aliases: []PromptAlias{replay}}}
	check := func(h *Hub) {
		t.Helper()
		got := session(t, h, "s").Timeline
		if len(got) != 1 || !got[0].At.Equal(at) || got[0].Prompt != "Original prompt" || len(got[0].Aliases) != 1 {
			t.Fatalf("replay duplicated original: %+v", got)
		}
	}
	for range 2 {
		h.Backfill("s", xs)
		oldBridge(h)
		check(h)
	}
	h = New(k, st)
	oldBridge(h)
	check(h)
	h.Backfill("s", xs)
	check(h)
}

func TestRecallAddsAndQueuesOnlyWhatIsAskedFor(t *testing.T) {
	h, _, _ := newHub(t)
	t0 := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: "third thing", PromptAt: at(3).Add(3 * time.Second)}})
	record := []Exchange{
		{At: at(0), Compacted: &Compaction{At: at(0)}},
		{At: at(1), Prompt: "first thing", Reply: "Did the first."},
		{At: at(2), Prompt: "second thing", Reply: "Did the second."},
		{At: at(3), Prompt: "third thing", Reply: "Did the third."},
	}
	prompts := func() (out []string) {
		for _, m := range session(t, h, "s").Timeline {
			out = append(out, m.Prompt)
		}
		return out
	}
	waiting := func() (out []string) {
		for _, u := range h.Unfilled() {
			for _, x := range u.Exchanges {
				out = append(out, x.Prompt)
			}
		}
		return out
	}

	if _, ok := h.Recall("nobody", record, nil); ok {
		t.Fatal("Recall took a session that is not open")
	}
	if queued, ok := h.Recall("s", record, []time.Time{at(2)}); !ok || queued != 1 {
		t.Fatalf("Recall = %d, %v; want the one exchange asked for", queued, ok)
	}
	if got := prompts(); !slices.Equal(got, []string{"second thing", "third thing"}) || !session(t, h, "s").Timeline[1].At.Equal(at(3)) {
		t.Fatalf("timeline = %q; want the exchange asked for beside the one it had, at the record's time", got)
	}
	if got := waiting(); !slices.Equal(got, []string{"second thing"}) {
		t.Fatalf("waiting = %q; want only the exchange asked for", got)
	}

	if queued, _ := h.Recall("s", record, []time.Time{at(1), at(2)}); queued != 2 {
		t.Fatalf("a second Recall left %d waiting; want the one already waiting and the one added", queued)
	}
	if got := waiting(); !slices.Equal(got, []string{"second thing", "first thing"}) {
		t.Fatalf("waiting = %q; want what was asked for first still first, and once", got)
	}
	h.SetFilled(h.Unfilled()[0], []Filled{{N: 0, Line: "Second done"}, {N: 1, Line: "First done"}})
	if got := session(t, h, "s").Timeline; len(got) != 3 || got[0].Line != "First done" || got[1].Line != "Second done" || got[2].Line != "" {
		t.Fatalf("timeline = %+v; want the model's line on each exchange recalled", got)
	}
	if queued, _ := h.Recall("s", record, []time.Time{at(1), at(2)}); queued != 0 {
		t.Errorf("recalling what the model has described queued %d", queued)
	}
}
