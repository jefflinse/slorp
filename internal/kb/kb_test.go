package kb_test

import (
	"testing"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

func open(t *testing.T) *kb.Store {
	t.Helper()
	return testdb.KB(t, testdb.Open(t))
}

func create(t *testing.T, k *kb.Store, name string) string {
	t.Helper()
	e, err := k.Create(name, "")
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return e.ID
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateMakesUniqueSlugs(t *testing.T) {
	k := open(t)
	if got := create(t, k, "Estoria: Snapshot Store!"); got != "estoria-snapshot-store" {
		t.Errorf("slug = %q", got)
	}
	if got := create(t, k, "estoria snapshot store"); got != "estoria-snapshot-store-2" {
		t.Errorf("second slug = %q, want a distinct one", got)
	}
	if _, err := k.Create("  !! ", ""); err == nil {
		t.Error("a name with nothing sluggable was accepted")
	}
	if n := len(k.Efforts()); n != 2 {
		t.Errorf("efforts = %d, want 2", n)
	}
}

func TestResolve(t *testing.T) {
	k := open(t)
	a, b := create(t, k, "A"), create(t, k, "B")
	must(t, k.AddDir(a, "/work/repo"))
	must(t, k.AddDir(b, "/work/repo/sub"))
	must(t, k.Assign("filed", b))
	must(t, k.Assign("unsorted", ""))

	tests := []struct {
		name, session, cwd string
		effort, via        string
		ok                 bool
	}{
		{"no rule applies", "s", "/elsewhere", "", "", false},
		{"directory itself", "s", "/work/repo", a, kb.ViaDir, true},
		{"inside the directory", "s", "/work/repo/pkg/x", a, kb.ViaDir, true},
		{"sibling sharing a prefix is not inside", "s", "/work/repository", "", "", false},
		{"deepest directory wins", "s", "/work/repo/sub/deep", b, kb.ViaDir, true},
		{"the user's filing beats a directory", "filed", "/work/repo", b, kb.ViaUser, true},
		{"the user's filing applies with no directory", "filed", "/elsewhere", b, kb.ViaUser, true},
		{"left unsorted by the user beats a directory", "unsorted", "/work/repo", "", kb.ViaUser, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			effort, via, ok := k.Resolve(tt.session, tt.cwd)
			if ok != tt.ok || (ok && (effort != tt.effort || via != tt.via)) {
				t.Errorf("Resolve(%q, %q) = %q, %q, %v; want %q, %q, %v", tt.session, tt.cwd, effort, via, ok, tt.effort, tt.via, tt.ok)
			}
		})
	}
}

func TestAssignRejectsUnknownEffort(t *testing.T) {
	k := open(t)
	if err := k.Assign("s", "nope"); err == nil {
		t.Fatal("filing under an effort that does not exist was accepted")
	}
	if _, _, ok := k.Resolve("s", "/x"); ok {
		t.Fatal("a rejected filing still resolved")
	}
}

func TestAddDirTakesTheDirectoryFromOtherEfforts(t *testing.T) {
	k := open(t)
	a, b := create(t, k, "A"), create(t, k, "B")
	must(t, k.AddDir(a, "/work/repo/"))
	must(t, k.AddDir(b, "/work/repo"))

	if effort, _, _ := k.Resolve("s", "/work/repo"); effort != b {
		t.Errorf("resolved to %q, want %q", effort, b)
	}
	for _, e := range k.Efforts() {
		if want := map[string]int{a: 0, b: 1}[e.ID]; len(e.Dirs) != want {
			t.Errorf("effort %s has dirs %v, want %d", e.ID, e.Dirs, want)
		}
	}
	if err := k.AddDir(a, "relative/path"); err == nil {
		t.Error("a relative directory was accepted")
	}
	if err := k.AddDir("nope", "/x"); err == nil {
		t.Error("a directory for an unknown effort was accepted")
	}
}

func TestDeleteForgetsTheEffortEverywhere(t *testing.T) {
	k := open(t)
	a, b := create(t, k, "A"), create(t, k, "B")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.Assign("s1", a))
	must(t, k.Assign("s2", b))
	must(t, k.Add(kb.SectionQuestions, kb.Entry{Text: "about a?", Effort: a}))
	must(t, k.Add(kb.SectionWatch, kb.Entry{Type: "channel", Name: "#a", Effort: a}))

	must(t, k.Delete(a))

	if _, _, ok := k.Resolve("s1", "/work/a"); ok {
		t.Error("a session filed under the deleted effort still resolves")
	}
	if effort, _, _ := k.Resolve("s2", "/x"); effort != b {
		t.Error("deleting one effort disturbed another's filing")
	}
	know := k.Knowledge()
	if know.Questions[0].Effort != "" || know.Questions[0].Text != "about a?" {
		t.Errorf("question should outlive its effort, unfiled: %+v", know.Questions[0])
	}
	if len(know.Watch[0].Efforts) != 0 {
		t.Errorf("watch entry still names the deleted effort: %+v", know.Watch[0])
	}
	if err := k.Delete(a); err == nil {
		t.Error("deleting an unknown effort reported success")
	}

	if again := create(t, k, "A"); again != a {
		t.Fatalf("recreated effort got id %q, want the freed %q", again, a)
	}
	if _, _, ok := k.Resolve("s1", "/elsewhere"); ok {
		t.Error("a new effort with a deleted one's name inherited its filings")
	}
}

func TestRevisionsTrackWhatTheModelReads(t *testing.T) {
	k := open(t)
	type revs struct{ filing, context int }
	now := func() revs { return revs{k.Revision(), k.ContextRevision()} }

	var a string
	steps := []struct {
		name            string
		do              func()
		filing, context bool
	}{
		{"create effort", func() { a = create(t, k, "A") }, true, false},
		{"routing hint", func() { must(t, k.Add(kb.SectionRouting, kb.Entry{Text: "hint"})) }, true, false},
		{"standing context", func() { must(t, k.Add(kb.SectionContext, kb.Entry{Text: "fact"})) }, false, true},
		{"open question", func() { must(t, k.Add(kb.SectionQuestions, kb.Entry{Text: "q", Effort: a})) }, false, false},
		{"watch entry", func() { must(t, k.Add(kb.SectionWatch, kb.Entry{Type: "dm", Name: "x"})) }, false, false},
		{"file a session", func() { must(t, k.Assign("s", a)) }, false, false},
		{"directory rule", func() { must(t, k.AddDir(a, "/w")) }, false, false},
		{"remove context", func() { must(t, k.Remove(kb.SectionContext, k.Knowledge().Context[0].ID)) }, false, true},
		{"remove hint", func() { must(t, k.Remove(kb.SectionRouting, k.Knowledge().Routing[0].ID)) }, true, false},
		{"delete effort", func() { must(t, k.Delete(a)) }, true, false},
	}
	for _, s := range steps {
		before := now()
		s.do()
		after := now()
		if got := after.filing != before.filing; got != s.filing {
			t.Errorf("%s: filing revision changed = %v, want %v", s.name, got, s.filing)
		}
		if got := after.context != before.context; got != s.context {
			t.Errorf("%s: context revision changed = %v, want %v", s.name, got, s.context)
		}
	}
}

func TestAddValidates(t *testing.T) {
	k := open(t)
	bad := []struct {
		name, section string
		entry         kb.Entry
	}{
		{"unknown section", "nonsense", kb.Entry{Text: "x"}},
		{"blank fact", kb.SectionContext, kb.Entry{Text: "  "}},
		{"blank question", kb.SectionQuestions, kb.Entry{}},
		{"watch without a name", kb.SectionWatch, kb.Entry{Type: "person"}},
		{"watch of an unknown type", kb.SectionWatch, kb.Entry{Type: "robot", Name: "x"}},
		{"question for an unknown effort", kb.SectionQuestions, kb.Entry{Text: "x", Effort: "nope"}},
	}
	for _, tt := range bad {
		if err := k.Add(tt.section, tt.entry); err == nil {
			t.Errorf("%s: accepted", tt.name)
		}
	}
	know := k.Knowledge()
	if n := len(know.Watch) + len(know.Questions) + len(know.Context) + len(know.Routing); n != 0 {
		t.Errorf("%d rejected entries were stored", n)
	}
}

func TestRemove(t *testing.T) {
	k := open(t)
	must(t, k.Add(kb.SectionContext, kb.Entry{Text: "one"}))
	must(t, k.Add(kb.SectionContext, kb.Entry{Text: "two"}))
	first := k.Knowledge().Context[0]

	if err := k.Remove(kb.SectionRouting, first.ID); err == nil {
		t.Error("removing an id from the wrong section reported success")
	}
	must(t, k.Remove(kb.SectionContext, first.ID))
	if left := k.Knowledge().Context; len(left) != 1 || left[0].Text != "two" {
		t.Errorf("after Remove: %+v", left)
	}
}

func TestKnowledgeBaseSurvivesReopen(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	a := create(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.Assign("s", a))
	must(t, k.Add(kb.SectionContext, kb.Entry{Text: "fact"}))
	wantFiling, wantContext := k.Revision(), k.ContextRevision()

	k2 := testdb.KB(t, st)
	if effort, via, _ := k2.Resolve("s", "/x"); effort != a || via != kb.ViaUser {
		t.Errorf("filing lost: %q via %q", effort, via)
	}
	if effort, _, _ := k2.Resolve("other", "/work/a/x"); effort != a {
		t.Error("directory rule lost")
	}
	if c := k2.Knowledge().Context; len(c) != 1 || c[0].Text != "fact" {
		t.Errorf("context lost: %+v", c)
	}
	if k2.Revision() != wantFiling || k2.ContextRevision() != wantContext {
		t.Error("revisions reset, which would make every cached guess look stale or fresh wrongly")
	}
	must(t, k2.Add(kb.SectionContext, kb.Entry{Text: "second"}))
	if ids := k2.Knowledge().Context; ids[0].ID == ids[1].ID {
		t.Error("entry ids collide after reopen")
	}
}

func TestResolveRepo(t *testing.T) {
	k := open(t)
	a, b := create(t, k, "A"), create(t, k, "B")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.AddDir(a, "/work/a2"))
	must(t, k.AddDir(b, "/work/b"))

	tests := []struct {
		name string
		held []string
		want string
		via  string
		ok   bool
	}{
		{"no checkout anywhere", nil, "", kb.ViaDir, false},
		{"a checkout outside every effort", []string{"/elsewhere"}, "", kb.ViaDir, false},
		{"a checkout in one effort", []string{"/work/b"}, b, kb.ViaDir, true},
		{"checkouts in two directories of one effort", []string{"/work/a", "/work/a2"}, a, kb.ViaDir, true},
		{"checkouts in two efforts", []string{"/work/a", "/work/b"}, "", "", false},
		{"checkouts in two efforts, the later listed first", []string{"/work/b", "/work/a2"}, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, via, ok := k.ResolveRepo("acme/widgets", tt.held)
			if got != tt.want || ok != tt.ok || (ok && via != tt.via) {
				t.Errorf("ResolveRepo = %q, %q, %v; want %q, %q, %v", got, via, ok, tt.want, tt.via, tt.ok)
			}
		})
	}

	must(t, k.AssignRepo("Acme/Widgets", a))
	if got, via, ok := k.ResolveRepo("acme/widgets", []string{"/work/b"}); got != a || via != kb.ViaUser || !ok {
		t.Errorf("after the user filed it: %q, %q, %v; want their choice over the directory rule, whatever the case", got, via, ok)
	}
	if got, _, ok := k.ResolveRepo("acme/other", nil); ok {
		t.Errorf("another repository resolved to %q", got)
	}
	if _, _, ok := k.Resolve("acme/widgets", "/nowhere"); ok {
		t.Error("a repository's filing was read as a session's")
	}

	must(t, k.AssignRepo("acme/widgets", ""))
	if got, via, ok := k.ResolveRepo("acme/widgets", []string{"/work/b"}); got != "" || via != kb.ViaUser || !ok {
		t.Errorf("after the user left it unsorted: %q, %q, %v; want unsorted to hold against the rule", got, via, ok)
	}

	must(t, k.AssignRepo("acme/widgets", a))
	must(t, k.Delete(a))
	if got, via, ok := k.ResolveRepo("acme/widgets", []string{"/work/b"}); got != b || via != kb.ViaDir || !ok {
		t.Errorf("after its effort was deleted: %q, %q, %v; want the rule to apply again", got, via, ok)
	}

	if err := k.AssignRepo("acme/widgets", "nope"); err == nil {
		t.Error("filing under an effort that does not exist was accepted")
	}
	if err := k.AssignRepo("  ", b); err == nil {
		t.Error("filing a repository with no name was accepted")
	}
}

func TestRepoFilingSurvivesReopen(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	id := create(t, k, "A")
	must(t, k.AssignRepo("acme/widgets", id))
	if got, via, ok := testdb.KB(t, st).ResolveRepo("acme/widgets", nil); got != id || via != kb.ViaUser || !ok {
		t.Errorf("after reopening: %q, %q, %v", got, via, ok)
	}
}
