package opencode

import (
	"context"
	"encoding/json"
	"errors"
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

func TestHistoryPairsParentsAndPreservesCompactions(t *testing.T) {
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
	if c := xs[2].Compacted; c == nil || c.ID != "prt_compact" || !c.Auto || c.At.UnixMilli() != 1700000008000 {
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
	if err != nil || len(xs) != 1 || xs[0].Reply != "I will inspect it." {
		t.Fatalf("reverted = %+v, %v", xs, err)
	}
	delete(doc["info"].(map[string]any), "revert")
	for _, m := range doc["messages"].([]any) {
		info := m.(map[string]any)["info"].(map[string]any)
		if info["id"] == "msg_summary" {
			info["error"] = map[string]string{"name": "MessageAbortedError"}
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
	{"info":{"id":"msg_file","sessionID":"ses_history","role":"user","time":{"created":1}},"parts":[{"type":"file","filename":"screenshot.png"}]},
	{"info":{"id":"msg_answer","sessionID":"ses_history","parentID":"msg_file","role":"assistant","time":{"created":2,"completed":3}},"parts":[{"type":"text","text":"An image."}]}]}`
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
	a := historyCommand(t, `test "$1" = export && test "$2" = --pure && test "$3" = ses_history || exit 2
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

func TestHistoryOverflowReplayIsNotANewPrompt(t *testing.T) {
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
	for _, field := range []string{`"overflow":true`, `"auto":true`} {
		raw := strings.Replace(string(b), field, strings.Replace(field, "true", "false", 1), 1)
		xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
		if err != nil || len(xs) != 5 {
			t.Fatalf("collapsed a genuine post-compaction prompt: %+v %v", xs, err)
		}
	}
}

func TestHistoryPartialRevertKeepsEarlierParts(t *testing.T) {
	raw := `{"info":{"id":"ses_history","revert":{"messageID":"msg_answer","partID":"prt_reverted"}},"messages":[
	{"info":{"id":"msg_user","sessionID":"ses_history","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"Question"}]},
	{"info":{"id":"msg_answer","sessionID":"ses_history","parentID":"msg_user","role":"assistant","time":{"created":2,"completed":3}},"parts":[{"id":"prt_kept","type":"text","text":"Kept answer"},{"id":"prt_reverted","type":"text","text":"Discarded answer"}]}]}`
	xs, err := readExchanges(strings.NewReader(raw), "ses_history", 40)
	if err != nil || len(xs) != 1 || xs[0].Reply != "Kept answer" {
		t.Fatalf("partial revert: %+v %v", xs, err)
	}
	raw = strings.Replace(raw, `"partID":"prt_reverted"`, `"partID":"missing"`, 1)
	if _, err := readExchanges(strings.NewReader(raw), "ses_history", 40); err == nil {
		t.Fatal("accepted a missing revert part")
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
