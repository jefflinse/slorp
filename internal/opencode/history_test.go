package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/history.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHistoryPairsTurnsAndPreservesCompactions(t *testing.T) {
	b := historyFixture(t)
	xs, err := readExchanges(strings.NewReader(string(b)), "ses_history", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 4 {
		t.Fatalf("exchanges = %+v", xs)
	}
	if xs[0].Prompt != "Inspect the cache" || xs[0].Reply != "The cache is stale." || xs[0].At.UnixMilli() != 1700000001000 {
		t.Fatalf("first = %+v", xs[0])
	}
	if xs[1].Prompt != "Apply the fix" || xs[1].Reply != "Tests pass.\n\nThe fix is ready." {
		t.Fatalf("second = %+v", xs[1])
	}
	if c := xs[2].Compacted; c == nil || c.ID != "msg_compact" || !c.Auto || c.At.UnixMilli() != 1700000008000 {
		t.Fatalf("compaction = %+v", c)
	}
	if xs[3].Prompt != "Apply the fix" || xs[3].Reply != "" {
		t.Fatalf("unanswered = %+v", xs[3])
	}
	limited, err := readExchanges(strings.NewReader(string(b)), "ses_history", 2)
	if err != nil || len(limited) != 2 || limited[0].Compacted == nil || limited[1].Prompt != xs[3].Prompt {
		t.Fatalf("latest two = %+v, %v", limited, err)
	}
}

func TestHistoryRevertAndFailedCompactions(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(historyFixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc["info"].(map[string]any)["revert"] = map[string]string{"messageID": "msg_question2"}
	b, _ := json.Marshal(doc)
	xs, err := readExchanges(strings.NewReader(string(b)), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Reply != "The cache is stale." {
		t.Fatalf("reverted = %+v, %v", xs, err)
	}
	delete(doc["info"].(map[string]any), "revert")
	for _, m := range doc["messages"].([]any) {
		message := m.(map[string]any)
		if message["id"] == "msg_compact" {
			message["status"] = "failed"
		}
	}
	b, _ = json.Marshal(doc)
	xs, err = readExchanges(strings.NewReader(string(b)), "ses_history", 40)
	if err != nil || len(xs) != 3 {
		t.Fatalf("failed compaction = %+v, %v", xs, err)
	}
	for _, x := range xs {
		if x.Compacted != nil {
			t.Fatal("failed compaction marked complete")
		}
	}
}

func TestHistoryIgnoresInvalidExports(t *testing.T) {
	for _, raw := range []string{``, `{`, `{}`, `{"info":{"id":"ses_other"},"messages":[]}`, `{"info":{"id":"ses_history"},"messages":[]} garbage`, `{"info":{"id":"ses_history","revert":{"messageID":"missing"}},"messages":[]}`} {
		if _, err := readExchanges(strings.NewReader(raw), "ses_history", 40); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestHistoryAttachmentsAndUnicodeLimits(t *testing.T) {
	raw := `{"info":{"id":"ses_history"},"messages":[
	{"id":"msg_file","type":"user","time":{"created":1},"text":"","files":[{"name":"screenshot.png"}]},
	{"id":"msg_answer","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"text","text":"An image."}]}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Prompt != "[Attached: screenshot.png]" || xs[0].Reply != "An image." {
		t.Fatalf("attachment = %+v, %v", xs, err)
	}
	if got := clipped("一二三四", 2, false); got != "一二…" {
		t.Fatal(got)
	}
	if got := clipped("一二三四", 2, true); got != "…三四" {
		t.Fatal(got)
	}
	raw = `{"info":{"id":"ses_history"},"messages":[
	{"id":"msg_user","type":"user","time":{"created":1},"text":"` + strings.Repeat("一", historyPrompt+1) + `"},
	{"id":"msg_answer","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"text","text":"` + strings.Repeat("二", historyReply+1) + `"}]}]}`
	xs, err = readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Prompt != strings.Repeat("一", historyPrompt)+"…" || xs[0].Reply != "…"+strings.Repeat("二", historyReply) {
		t.Fatalf("Unicode clipping failed: %v", err)
	}
}

func historyCommand(t *testing.T, script string) *Adapter {
	t.Helper()
	a := fixture(t)
	put(t, a.Bin, "#!/bin/sh\n"+script)
	if err := os.Chmod(a.Bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestHistoryExportUsesRegularFileAndCleansUp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	a := historyCommand(t, `test "$1" = session && test "$2" = export && test "$3" = ses_history && test "$4" = --standalone || exit 2
test "$OPENCODE_PURE" = true || exit 4
test -f /dev/stdout || exit 3
exec /bin/cat "$HOME/history.json"
`)
	put(t, filepath.Join(a.Home, "history.json"), string(historyFixture(t)))
	xs, err := a.Exchanges(context.Background(), "ses_history", 40)
	if err != nil || len(xs) != 4 {
		t.Fatalf("export = %+v, %v", xs, err)
	}
	left, _ := os.ReadDir(os.TempDir())
	if len(left) != 0 {
		t.Fatalf("temporary exports were kept: %+v", left)
	}
	for _, id := range []string{"", "--help", "ses_../../other", "ses_name\nextra"} {
		if _, err := a.Exchanges(context.Background(), id, 40); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}

func TestHistoryExportFailureAndCancellation(t *testing.T) {
	a := historyCommand(t, "echo missing >&2\nexit 1\n")
	if _, err := a.Exchanges(context.Background(), "ses_history", 40); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatal(err)
	}
	a = historyCommand(t, "exec sleep 60\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := a.Exchanges(ctx, "ses_history", 40); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("export cancellation did not stop the subprocess")
	}
}

func TestHistoryExportSizeLimit(t *testing.T) {
	a := historyCommand(t, "exec dd if=/dev/zero bs=1048576 count=65 2>/dev/null\n")
	if _, err := a.Exchanges(context.Background(), "ses_history", 40); err == nil || !strings.Contains(err.Error(), "64 MiB") {
		t.Fatalf("oversized export = %v", err)
	}
}

func TestHistoryCheckpointContinuationIsNotANewPrompt(t *testing.T) {
	b, err := os.ReadFile("testdata/overflow.json")
	if err != nil {
		t.Fatal(err)
	}
	xs, err := readExchanges(strings.NewReader(string(b)), "ses_history", 40)
	if err != nil || len(xs) != 4 {
		t.Fatalf("overflow history: %+v %v", xs, err)
	}
	if xs[1].Prompt != "Apply the fix" || xs[1].At.UnixMilli() != 1700000003000 || xs[1].Reply != "Fixed after compaction." {
		t.Fatalf("original prompt: %+v", xs[1])
	}
	if xs[2].Compacted == nil || xs[3].Reply != "Handled the genuine repeated request." {
		t.Fatalf("lost compaction or genuine repeat: %+v", xs)
	}
	raw := strings.Replace(string(b), `"reason":"auto"`, `"reason":"manual"`, 1)
	xs, err = readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 4 || xs[2].Compacted.Auto {
		t.Fatalf("manual checkpoint: %+v %v", xs, err)
	}
}

func TestHistoryRevertAtAssistant(t *testing.T) {
	raw := `{"info":{"id":"ses_history","revert":{"messageID":"msg_answer"}},"messages":[
	{"id":"msg_user","type":"user","time":{"created":1},"text":"Question"},
	{"id":"msg_answer","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"text","text":"Discarded answer"}]}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Reply != "" {
		t.Fatalf("assistant revert: %+v %v", xs, err)
	}
}

func TestHistoryLatestFortyAndDistinctRepeats(t *testing.T) {
	var messages []map[string]any
	for i := range 45 {
		messages = append(messages, map[string]any{"id": fmt.Sprintf("msg_%d", i), "type": "user", "text": "Repeated prompt", "time": map[string]int{"created": i + 1}})
	}
	raw, err := json.Marshal(map[string]any{"info": map[string]string{"id": "ses_history"}, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	xs, err := readExchanges(strings.NewReader(string(raw)), "ses_history", 100)
	if err != nil || len(xs) != 40 || xs[0].At.UnixMilli() != 6 || xs[39].At.UnixMilli() != 45 {
		t.Fatalf("latest 40 = %+v, %v", xs, err)
	}
}

func TestHistoryKeepsTranscriptOrderForPairing(t *testing.T) {
	raw := `{"info":{"id":"ses_history"},"messages":[
	{"id":"msg_user","type":"user","time":{"created":30},"text":"Question"},
	{"id":"msg_answer","type":"assistant","time":{"created":20,"completed":40},"content":[{"type":"text","text":"Answer"},{"type":"tool","text":"Private output"},{"type":"reasoning","text":"Private reasoning"}]},
	{"id":"msg_user","type":"user","time":{"created":30},"text":"Duplicate"},
	{"id":"msg_empty","type":"user","time":{"created":50},"text":""},
	{"id":"msg_other","type":"assistant","time":{"created":60,"completed":70},"content":[{"type":"text","text":"Unrelated"}]}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Prompt != "Question" || xs[0].Reply != "Answer" {
		t.Fatalf("transcript order = %+v, %v", xs, err)
	}
}

func TestHistoryConsecutiveUsersPairOnlyNewest(t *testing.T) {
	raw := `{"info":{"id":"ses_history"},"messages":[
	{"id":"msg_first","type":"user","time":{"created":1},"text":"First question"},
	{"id":"msg_second","type":"user","time":{"created":2},"text":"Second question"},
	{"id":"msg_answer","type":"assistant","time":{"created":3,"completed":4},"content":[{"type":"text","text":"Second answer"}]}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 2 || xs[0].Prompt != "First question" || xs[0].Reply != "" ||
		xs[1].Prompt != "Second question" || xs[1].Reply != "Second answer" {
		t.Fatalf("consecutive users = %+v, %v", xs, err)
	}
}

func TestHistoryExcludesSyntheticChildReports(t *testing.T) {
	raw := `{"info":{"id":"ses_history"},"messages":[
	{"id":"msg_user","type":"user","time":{"created":1},"text":"Review the change"},
	{"id":"msg_child","type":"synthetic","time":{"created":2},"text":"<subagent>\nPrivate child report\n</subagent>","metadata":{"source":"subagent","childID":"ses_child","state":"completed"}},
	{"id":"msg_answer","type":"assistant","time":{"created":3,"completed":4},"content":[{"type":"text","text":"The review is complete."}]},
	{"id":"msg_latechild","type":"synthetic","time":{"created":5},"text":"Private late child report","metadata":{"source":"subagent","childID":"ses_other","state":"error"}}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Prompt != "Review the change" || xs[0].Reply != "The review is complete." {
		t.Fatalf("synthetic child reports = %+v, %v", xs, err)
	}
}

func TestHistoryFailedAssistantKeepsCompletedText(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			end := ""
			if completed {
				end = `,"completed":3`
			}
			raw := `{"info":{"id":"ses_history"},"messages":[
			{"id":"msg_user","type":"user","time":{"created":1},"text":"Question"},
			{"id":"msg_answer","type":"assistant","time":{"created":2` + end + `},"finish":"error","error":{"type":"TestError","message":"Private diagnostic"},"content":[{"type":"text","text":"Visible answer before failure."}]}]}`
			xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
			want := ""
			if completed {
				want = "Visible answer before failure."
			}
			if err != nil || len(xs) != 1 || xs[0].Reply != want {
				t.Fatalf("failed assistant = %+v, %v", xs, err)
			}
		})
	}
}

func TestHistoryReadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readExchanges(&historyReader{ctx: ctx, Reader: strings.NewReader(string(historyFixture(t)))}, "ses_history", 40)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v", err)
	}
}

func TestReplyIsReadWhole(t *testing.T) {
	long := strings.Repeat("二", historyReply+5)
	raw := `{"info":{"id":"ses_history","revert":{"messageID":"msg_reverted"}},"messages":[
	{"id":"msg_user","type":"user","time":{"created":1},"text":"Fix it"},
	{"id":"msg_first","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"text","text":"Earlier. ` + long + `"}]},
	{"id":"msg_answer","type":"assistant","time":{"created":4,"completed":5},"content":[{"type":"text","text":" Done. "},{"type":"tool","text":"ls"},{"type":"text","text":"` + long + `"}]},
	{"id":"msg_running","type":"assistant","time":{"created":6},"content":[{"type":"text","text":"Still writing. ` + long + `"}]},
	{"id":"msg_reverted","type":"assistant","time":{"created":7,"completed":8},"content":[{"type":"text","text":"Undone. ` + long + `"}]}]}`
	kept := string([]rune(long)[5:])
	if got, err := readReply(strings.NewReader(raw), "ses_history", kept); err != nil || got != "Done.\n\n"+long {
		t.Errorf("reply = %d runes, %v; want all of the latest completed message, its text parts a blank line apart", len([]rune(got)), err)
	}
	if got, err := readReply(strings.NewReader(raw), "ses_history", "never said"); err != nil || got != "" {
		t.Errorf("reply = %q, %v; want nothing for what was never said", got, err)
	}
	if _, err := readReply(strings.NewReader(raw), "ses_other", kept); err == nil {
		t.Error("read a reply from another session's export")
	}

	t.Setenv("TMPDIR", t.TempDir())
	a := historyCommand(t, `test "$1" = session && test "$2" = export && test "$3" = ses_history && test "$4" = --standalone || exit 2
exec /bin/cat "$HOME/history.json"
`)
	put(t, filepath.Join(a.Home, "history.json"), raw)
	if got, err := a.Reply(context.Background(), "ses_history", kept); err != nil || got != "Done.\n\n"+long {
		t.Errorf("Reply = %d runes, %v; want the whole message from an export", len([]rune(got)), err)
	}
	if left, _ := os.ReadDir(os.TempDir()); len(left) != 0 {
		t.Errorf("temporary exports were kept: %+v", left)
	}
	if _, err := a.Reply(context.Background(), "ses_../../other", kept); err == nil {
		t.Error("accepted a session id that is not one")
	}
}
