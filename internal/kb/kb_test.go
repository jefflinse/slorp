package kb_test

import (
	"reflect"
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

func TestMoveRearrangesEfforts(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	a, b, c := create(t, k, "A"), create(t, k, "B"), create(t, k, "C")
	order := func(k *kb.Store) string {
		out := ""
		for _, e := range k.Efforts() {
			out += e.ID
		}
		return out
	}
	revision := k.Revision()

	for _, step := range []struct{ id, before, want string }{
		{c, a, "cab"},
		{c, "", "abc"},
		{a, c, "bac"},
		{b, b, "bac"},
		{b, a, "bac"},
		{b, "", "acb"},
	} {
		must(t, k.Move(step.id, step.before))
		if got := order(k); got != step.want {
			t.Errorf("Move(%q, %q): order %s, want %s", step.id, step.before, got, step.want)
		}
	}
	if k.Move("nope", a) == nil || k.Move(a, "nope") == nil {
		t.Error("a move naming an unknown effort was accepted")
	}
	if got := order(k); got != "acb" {
		t.Errorf("a refused move left the order %s", got)
	}
	if k.Revision() != revision {
		t.Error("rearranging changed the filing revision, which would make the model file every session again")
	}
	if got := order(testdb.KB(t, st)); got != "acb" {
		t.Errorf("order after reopen %s, want acb", got)
	}
}

func TestEditRenamesAndRedescribes(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	e, err := k.Create("Widgets", "Making widgets")
	must(t, err)
	name, about, blank := "  Gadgets ", " Making gadgets ", "  "

	must(t, k.Edit(e.ID, kb.Changes{Name: &name}))
	if got := k.Efforts()[0]; got.ID != e.ID || got.Name != "Gadgets" || got.About != "Making widgets" {
		t.Errorf("after a rename: %+v, want the same effort by another name", got)
	}
	must(t, k.Edit(e.ID, kb.Changes{About: &about}))
	if got := k.Efforts()[0]; got.Name != "Gadgets" || got.About != "Making gadgets" {
		t.Errorf("after a new description: %+v", got)
	}
	must(t, k.Edit(e.ID, kb.Changes{About: &blank}))
	if got := k.Efforts()[0]; got.About != "" {
		t.Errorf("a description could not be cleared: %q", got.About)
	}
	if k.Edit(e.ID, kb.Changes{Name: &blank}) == nil || k.Efforts()[0].Name != "Gadgets" {
		t.Error("an effort was left without a name")
	}
	if k.Edit("nope", kb.Changes{Name: &name}) == nil {
		t.Error("an unknown effort was edited")
	}
	revision, color, glyph, garish, long := k.Revision(), "teal", " ⚙ ", "puce", "far too long"
	must(t, k.Edit(e.ID, kb.Changes{Color: &color, Glyph: &glyph}))
	if got := k.Efforts()[0]; got.Color != "teal" || got.Glyph != "⚙" || got.Name != "Gadgets" {
		t.Errorf("after a colour and a glyph: %+v", got)
	}
	if k.Revision() != revision {
		t.Error("marking an effort changed the filing revision, which the model does not read it for")
	}
	if k.Edit(e.ID, kb.Changes{Color: &garish}) == nil || k.Edit(e.ID, kb.Changes{Glyph: &long}) == nil || k.Efforts()[0].Color != "teal" || k.Efforts()[0].Glyph != "⚙" {
		t.Errorf("a colour that is not offered or a glyph that is a word was taken: %+v", k.Efforts()[0])
	}
	must(t, k.Edit(e.ID, kb.Changes{Color: &blank, Glyph: &blank}))
	if got := k.Efforts()[0]; got.Color != "" || got.Glyph != "" {
		t.Errorf("a colour and glyph could not be cleared: %+v", got)
	}
	if got := testdb.KB(t, st).Efforts()[0]; got.Name != "Gadgets" {
		t.Errorf("after reopen the effort is named %q", got.Name)
	}
}

func TestEffortsAreGivenColoursThatDiffer(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	seen := map[string]bool{}
	for i := range kb.Colors {
		create(t, k, "Effort "+string(rune('A'+i)))
	}
	for _, e := range k.Efforts() {
		if e.Color == "" || seen[e.Color] {
			t.Errorf("%s was given %q, which is no colour or one another effort has while some are spare", e.Name, e.Color)
		}
		seen[e.Color] = true
	}
	first := k.Efforts()[0]
	must(t, k.Delete(first.ID))
	if again := create(t, k, "One more"); k.Efforts()[len(kb.Colors)-1].ID != again || k.Efforts()[len(kb.Colors)-1].Color != first.Color {
		t.Errorf("a new effort was not given the one colour left spare, %q", first.Color)
	}
}

func TestEffortsFromBeforeColoursAreGivenThem(t *testing.T) {
	st := testdb.Open(t)
	must(t, st.Put("kb", "main", map[string]any{"efforts": []map[string]any{
		{"id": "a", "name": "A", "dirs": []string{}}, {"id": "b", "name": "B", "dirs": []string{}, "color": "red"}, {"id": "c", "name": "C", "dirs": []string{}}}}))
	colours := func() string {
		out := ""
		for _, e := range testdb.KB(t, st).Efforts() {
			out += e.Color + " "
		}
		return out
	}
	first := colours()
	if first != "orange red olive " {
		t.Errorf("colours = %q, want the efforts without one given spare ones, and the one with a colour left alone", first)
	}
	if again := colours(); again != first {
		t.Errorf("colours changed between one start and the next: %q then %q", first, again)
	}
}

func TestShelve(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	a := create(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.Assign("s1", a))

	must(t, k.Shelve(a, true))
	if !k.Has(a) || k.Active(a) || !k.Efforts()[0].Shelved {
		t.Error("a shelved effort should still be there, and not be one the model may file under")
	}
	if effort, _, _ := k.Resolve("s1", "/x"); effort != a {
		t.Error("shelving lost what the user filed under the effort")
	}
	if effort, _, _ := k.Resolve("s2", "/work/a/x"); effort != a {
		t.Error("shelving lost the effort's directory rule")
	}
	if !testdb.KB(t, st).Efforts()[0].Shelved {
		t.Error("an effort came back off the shelf on reopen")
	}
	must(t, k.Shelve(a, false))
	if !k.Active(a) || k.Efforts()[0].Shelved {
		t.Error("an effort brought back is still shelved")
	}
	if k.Shelve("nope", true) == nil || k.Active("nope") || k.Active("") {
		t.Error("an unknown effort was shelved or counted as active")
	}
}

func TestRemoveDir(t *testing.T) {
	k := open(t)
	a, b := create(t, k, "A"), create(t, k, "B")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.AddDir(a, "/work/shared"))
	must(t, k.AddDir(b, "/work/b"))

	must(t, k.RemoveDir(a, "/work/a/"))
	if _, _, ok := k.Resolve("s", "/work/a/x"); ok {
		t.Error("a removed directory still files sessions")
	}
	if effort, _, _ := k.Resolve("s", "/work/shared"); effort != a {
		t.Error("removing one directory took another with it")
	}
	if k.RemoveDir(a, "/work/b") == nil || k.RemoveDir("nope", "/work/shared") == nil {
		t.Error("a directory was removed from an effort that does not have it")
	}
	if effort, _, _ := k.Resolve("s", "/work/b"); effort != b {
		t.Error("a refused removal took the directory from its own effort")
	}
}

func TestMergeMovesEverythingAcross(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	a, b, c := create(t, k, "A"), create(t, k, "B"), create(t, k, "C")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.AddDir(b, "/work/b"))
	must(t, k.Assign("s1", a))
	must(t, k.Assign("s2", c))
	must(t, k.AssignRepo("Org/Repo", a))
	must(t, k.Add(kb.SectionQuestions, kb.Entry{Text: "about a?", Effort: a}))
	must(t, k.Add(kb.SectionWatch, kb.Entry{Type: "channel", Name: "#a", Effort: a}))
	revision := k.Revision()

	must(t, k.Merge(a, b))

	check := func(k *kb.Store) {
		t.Helper()
		if ids := k.Efforts(); len(ids) != 2 || ids[0].ID != b || ids[1].ID != c {
			t.Fatalf("efforts = %+v, want the merged one gone", ids)
		}
		if effort, via, _ := k.Resolve("s1", "/x"); effort != b || via != kb.ViaUser {
			t.Errorf("a session the user filed went to %q via %q", effort, via)
		}
		for _, dir := range []string{"/work/a/x", "/work/b/x"} {
			if effort, _, _ := k.Resolve("other", dir); effort != b {
				t.Errorf("%s files under %q, want the effort merged into", dir, effort)
			}
		}
		if effort, _, _ := k.ResolveRepo("org/repo", nil); effort != b {
			t.Errorf("a repository the user filed went to %q", effort)
		}
		if effort, _, _ := k.Resolve("s2", "/x"); effort != c {
			t.Error("merging two efforts moved what was filed under a third")
		}
		know := k.Knowledge()
		if know.Questions[0].Effort != b || len(know.Watch[0].Efforts) != 1 || know.Watch[0].Efforts[0] != b {
			t.Errorf("questions and watch entries were not moved: %+v, %+v", know.Questions, know.Watch)
		}
	}
	check(k)
	check(testdb.KB(t, st))
	if k.Revision() == revision {
		t.Error("the model was not told the efforts it files under have changed")
	}
	if k.Merge(b, b) == nil || k.Merge("nope", b) == nil || k.Merge(b, "nope") == nil || len(k.Efforts()) != 2 {
		t.Error("a merge with itself or with an unknown effort was accepted")
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
		{"drop directory rule", func() { must(t, k.RemoveDir(a, "/w")) }, false, false},
		{"rename effort", func() { name := "B"; must(t, k.Edit(a, kb.Changes{Name: &name})) }, true, false},
		{"rename it the same", func() { name := "B"; must(t, k.Edit(a, kb.Changes{Name: &name})) }, false, false},
		{"shelve effort", func() { must(t, k.Shelve(a, true)) }, true, false},
		{"shelve it again", func() { must(t, k.Shelve(a, true)) }, false, false},
		{"bring effort back", func() { must(t, k.Shelve(a, false)) }, true, false},
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
	for _, name := range []string{"acme/widgets", "ACME/Widgets"} {
		if got, via, ok := k.ResolveRepo(name, []string{"/work/b"}); got != a || via != kb.ViaRepo || !ok {
			t.Errorf("%s after the user filed it: %q, %q, %v; want their choice over the directory rule, whatever the case", name, got, via, ok)
		}
	}
	if got, _, ok := k.ResolveRepo("acme/other", nil); ok {
		t.Errorf("another repository resolved to %q", got)
	}
	if _, _, ok := k.Resolve("acme/widgets", "/nowhere"); ok {
		t.Error("a repository's filing was read as a session's")
	}

	must(t, k.AssignRepo("acme/widgets", ""))
	if got, via, ok := k.ResolveRepo("acme/widgets", []string{"/work/b"}); got != "" || via != kb.ViaRepo || !ok {
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
	if got, via, ok := testdb.KB(t, st).ResolveRepo("acme/widgets", nil); got != id || via != kb.ViaRepo || !ok {
		t.Errorf("after reopening: %q, %q, %v", got, via, ok)
	}
}

func TestResolvePull(t *testing.T) {
	k := open(t)
	a, b, c := create(t, k, "A"), create(t, k, "B"), create(t, k, "C")
	must(t, k.AddDir(c, "/work/c"))
	held := []string{"/work/c"}
	resolve := func(pull string) [2]string {
		id, via, ok := k.ResolvePull(pull, held)
		if !ok {
			return [2]string{}
		}
		return [2]string{id, via}
	}

	if got := resolve("acme/widgets#7"); got != [2]string{c, kb.ViaDir} {
		t.Errorf("before anything is filed: %v, want the directory rule for its repository", got)
	}
	must(t, k.AssignRepo("acme/widgets", a))
	if got := resolve("acme/widgets#7"); got != [2]string{a, kb.ViaRepo} {
		t.Errorf("after its repository was filed: %v, want it to go with the repository", got)
	}

	must(t, k.AssignPull("Acme/Widgets#7", b))
	for _, name := range []string{"acme/widgets#7", "ACME/Widgets#7"} {
		if got := resolve(name); got != [2]string{b, kb.ViaUser} {
			t.Errorf("%s after the user filed it: %v, want their choice over its repository's, whatever the case", name, got)
		}
	}
	if got := resolve("acme/widgets#70"); got != [2]string{a, kb.ViaRepo} {
		t.Errorf("another pull request of the repository: %v, want it left with the repository", got)
	}
	if got, via, _ := k.ResolveRepo("acme/widgets", held); got != a || via != kb.ViaRepo {
		t.Errorf("the repository after one of its pull requests was filed: %q, %q; want it where it was", got, via)
	}

	must(t, k.AssignRepo("acme/widgets", c))
	if got := resolve("acme/widgets#7"); got != [2]string{b, kb.ViaUser} {
		t.Errorf("after its repository was filed elsewhere: %v, want its own filing to hold", got)
	}
	must(t, k.AssignPull("acme/widgets#7", ""))
	if got := resolve("acme/widgets#7"); got != [2]string{"", kb.ViaUser} {
		t.Errorf("after the user left it unsorted: %v, want unsorted to hold against its repository", got)
	}

	must(t, k.AssignPull("acme/widgets#7", b))
	must(t, k.Delete(b))
	if got := resolve("acme/widgets#7"); got != [2]string{c, kb.ViaRepo} {
		t.Errorf("after its effort was deleted: %v, want it with its repository again", got)
	}
	must(t, k.AssignPull("acme/widgets#7", a))
	must(t, k.UnassignPull("ACME/widgets#7"))
	if got := resolve("acme/widgets#7"); got != [2]string{c, kb.ViaRepo} {
		t.Errorf("after its filing was forgotten: %v, want it with its repository again", got)
	}

	for _, bad := range []string{"acme/widgets", "acme/widgets#", "acme/widgets#x", "acme/widgets#0", "#7", " #7"} {
		if err := k.AssignPull(bad, a); err == nil {
			t.Errorf("filing %q as a pull request was accepted", bad)
		}
	}
	if err := k.AssignPull("acme/widgets#7", "nope"); err == nil {
		t.Error("filing under an effort that does not exist was accepted")
	}
}

func TestPullFilingSurvivesReopen(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	id := create(t, k, "A")
	must(t, k.AssignPull("acme/widgets#7", id))
	if got, via, ok := testdb.KB(t, st).ResolvePull("acme/widgets#7", nil); got != id || via != kb.ViaUser || !ok {
		t.Errorf("after reopening: %q, %q, %v", got, via, ok)
	}
}

// role adds a role and returns its ID.
func role(t *testing.T, k *kb.Store, name, attention string) string {
	t.Helper()
	must(t, k.Add(kb.SectionRoles, kb.Entry{Name: name, Text: " what " + name + " does ", Type: attention}))
	roles := k.Knowledge().Roles
	return roles[len(roles)-1].ID
}

func TestRoles(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	if got := k.Knowledge().Roles; got == nil || len(got) != 0 {
		t.Fatalf("Roles = %#v before any is added, want an empty list", got)
	}
	start := k.RoleRevision()
	reviewer, triage := role(t, k, " Reviewer ", ""), role(t, k, "Triage", kb.AttentionStopped)
	want := []kb.Role{{ID: reviewer, Name: "Reviewer", About: "what  Reviewer  does", Attention: kb.AttentionAll}, {ID: triage, Name: "Triage", About: "what Triage does", Attention: kb.AttentionStopped}}
	if got := k.Knowledge().Roles; len(got) != 2 || got[0] != want[0] || got[1] != want[1] || reviewer == triage {
		t.Fatalf("Roles = %+v, want %+v", got, want)
	}
	if k.RoleRevision() != start+2 {
		t.Error("adding roles did not change the role revision, so the model would not look again")
	}
	for name, bad := range map[string]kb.Entry{"no name": {Text: "x"}, "an attention that does not exist": {Name: "x", Type: "loud"}} {
		if k.Add(kb.SectionRoles, bad) == nil {
			t.Errorf("a role with %s was accepted", name)
		}
	}

	if _, ok := k.RoleOf("s"); ok {
		t.Error("a session nobody gave a role has one")
	}
	if k.SetRole("s", "nope") == nil {
		t.Error("a role that does not exist was given to a session")
	}
	must(t, k.SetRole("s", triage))
	must(t, k.SetRole("none", ""))
	if got, ok := k.RoleOf("s"); !ok || got != want[1] {
		t.Errorf("RoleOf = %+v, %v", got, ok)
	}
	if got, ok := k.RoleOf("none"); !ok || got != (kb.Role{}) {
		t.Errorf("RoleOf a session told to have none = %+v, %v; want none, as the user said", got, ok)
	}
	settled := k.RoleRevision()

	if k.SetRoleAttention("nope", kb.AttentionNone) == nil || k.SetRoleAttention(triage, "loud") == nil {
		t.Error("a bad change of attention was accepted")
	}
	must(t, k.SetRoleAttention(triage, kb.AttentionNone))
	if got, _ := k.RoleOf("s"); got.Attention != kb.AttentionNone {
		t.Errorf("attention = %q after it was changed", got.Attention)
	}
	if k.RoleRevision() != settled {
		t.Error("giving a session a role, or changing a role's attention, changed what the model chooses among")
	}

	k2 := testdb.KB(t, st)
	if got, ok := k2.RoleOf("s"); !ok || got.ID != triage || got.Attention != kb.AttentionNone || len(k2.Knowledge().Roles) != 2 || k2.RoleRevision() != settled {
		t.Errorf("after reopening: role %+v, %v; %d roles; revision %d", got, ok, len(k2.Knowledge().Roles), k2.RoleRevision())
	}

	must(t, k.Remove(kb.SectionRoles, triage))
	if _, ok := k.RoleOf("s"); ok || k.HasRole(triage) || !k.HasRole(reviewer) || len(k.Knowledge().Roles) != 1 {
		t.Error("a removed role is still around, or took another with it")
	}
	if _, ok := k.RoleOf("none"); !ok {
		t.Error("removing a role forgot a session the user said has none")
	}
	if k.RoleRevision() == settled {
		t.Error("removing a role did not change the role revision")
	}
}

func TestCarry(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	widgets, gears := create(t, k, "Widgets"), create(t, k, "Gears")
	reviewer, triage := role(t, k, "Reviewer", ""), role(t, k, "Triage", "")
	filed := func(k *kb.Store, id string) string {
		effort, via, _ := k.Resolve(id, "")
		r, _ := k.RoleOf(id)
		return effort + "/" + via + "/" + r.ID
	}
	must(t, k.Assign("old", widgets))
	must(t, k.SetRole("old", reviewer))

	must(t, k.Carry("old", "new"))
	if got := filed(k, "new"); got != widgets+"/"+kb.ViaUser+"/"+reviewer {
		t.Errorf("the new session is filed %q, want the effort and role the user gave the old", got)
	}
	if _, _, ok := k.Resolve("old", ""); ok {
		t.Error("the old session is still filed where the user put it")
	}
	if _, ok := k.RoleOf("old"); ok {
		t.Error("the old session still has the role the user gave it")
	}
	if got := filed(testdb.KB(t, st), "new"); got != widgets+"/"+kb.ViaUser+"/"+reviewer {
		t.Errorf("after reopening, the new session is filed %q", got)
	}

	// What the user said of the session carried to stands.
	must(t, k.Assign("older", gears))
	must(t, k.SetRole("older", triage))
	must(t, k.Carry("older", "new"))
	if got := filed(k, "new"); got != widgets+"/"+kb.ViaUser+"/"+reviewer {
		t.Errorf("the new session is filed %q, want what it already had kept", got)
	}
	if _, _, ok := k.Resolve("older", ""); ok {
		t.Error("a session whose filing was not needed is still filed")
	}
	must(t, k.Carry("nothing", "new"))
}

func TestWorkspaces(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	if got := k.Knowledge().Workspaces; got == nil || len(got) != 0 {
		t.Errorf("Workspaces = %v before any is added, want an empty list", got)
	}
	must(t, k.Add(kb.SectionWorkspaces, kb.Entry{Name: " Slorp ", Dir: "/work/slorp/"}))
	must(t, k.Add(kb.SectionWorkspaces, kb.Entry{Name: "Docs", Dir: "/work/docs"}))
	got := k.Knowledge().Workspaces
	if len(got) != 2 || got[0].Name != "Slorp" || got[0].Dir != "/work/slorp" || got[1].Name != "Docs" || got[0].ID == "" || got[0].ID == got[1].ID {
		t.Fatalf("Workspaces = %+v, want the two added, in that order, each with an id of its own, a trimmed name and a clean directory", got)
	}

	bad := map[string]kb.Entry{
		"no name":                 {Dir: "/work/x"},
		"a blank name":            {Name: "  ", Dir: "/work/x"},
		"no directory":            {Name: "X"},
		"a relative directory":    {Name: "X", Dir: "work/x"},
		"a directory another has": {Name: "Again", Dir: "/work/docs/"},
	}
	for name, entry := range bad {
		if err := k.Add(kb.SectionWorkspaces, entry); err == nil {
			t.Errorf("a workspace with %s was accepted", name)
		}
	}
	if n := len(k.Knowledge().Workspaces); n != 2 {
		t.Errorf("%d workspaces after the rejected ones, want 2", n)
	}

	if after := testdb.KB(t, st).Knowledge().Workspaces; !reflect.DeepEqual(after, got) {
		t.Errorf("after reopening: %+v, want %+v", after, got)
	}
	must(t, k.Remove(kb.SectionWorkspaces, got[0].ID))
	if left := k.Knowledge().Workspaces; len(left) != 1 || left[0].Name != "Docs" {
		t.Errorf("after removing the first: %+v, want only Docs", left)
	}
	if err := k.Remove(kb.SectionWorkspaces, got[0].ID); err == nil {
		t.Error("removing a workspace that is gone was accepted")
	}
	must(t, k.Add(kb.SectionWorkspaces, kb.Entry{Name: "Slorp again", Dir: "/work/slorp"}))
}

func TestProfiles(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	if got := k.Knowledge().Profiles; got == nil || len(got) != 0 {
		t.Errorf("Profiles = %v before any is added, want an empty list", got)
	}
	effort, err := k.Create("Docs", "")
	must(t, err)
	other, err := k.Create("Site", "")
	must(t, err)
	must(t, k.Add(kb.SectionRoles, kb.Entry{Name: "Reviewer"}))
	role := k.Knowledge().Roles[0].ID
	warm := 0.7

	first, err := k.AddProfile(kb.Profile{ID: "mine", Name: " Review ", Default: true, Dir: "/work/docs/", Effort: effort.ID, Role: role, Terminal: true, Model: " opus ", EffortLevel: "high"})
	must(t, err)
	second, err := k.AddProfile(kb.Profile{Name: "Local", Engine: kb.EngineLocal, Thinking: true, Temperature: &warm})
	must(t, err)
	got := k.Knowledge().Profiles
	want := []kb.Profile{
		{ID: first, Name: "Review", Default: true, Engine: kb.EngineClaude, Dir: "/work/docs", Effort: effort.ID, Role: role, Terminal: true, Model: "opus", EffortLevel: "high"},
		{ID: second, Name: "Local", Engine: kb.EngineLocal, Thinking: true, Temperature: &warm},
	}
	if first == "" || first == "mine" || first == second || !reflect.DeepEqual(got, want) {
		t.Fatalf("Profiles = %+v, want %+v", got, want)
	}

	hot, cold := 2.5, -0.1
	bad := map[string]kb.Profile{
		"no name":                       {},
		"a blank name":                  {Name: "  "},
		"a name another has":            {Name: "review"},
		"an engine there is none of":    {Name: "X", Engine: "gemini"},
		"a relative directory":          {Name: "X", Dir: "work/x"},
		"an effort that does not exist": {Name: "X", Effort: "nope"},
		"a role that does not exist":    {Name: "X", Role: "nope"},
		"a temperature above 2":         {Name: "X", Temperature: &hot},
		"a temperature below 0":         {Name: "X", Temperature: &cold},
	}
	for name, p := range bad {
		if _, err := k.AddProfile(p); err == nil {
			t.Errorf("a profile with %s was accepted", name)
		}
		p.ID = second
		if name != "a name another has" {
			if err := k.SetProfile(p); err == nil {
				t.Errorf("a profile changed to have %s was accepted", name)
			}
		}
	}
	if err := k.SetProfile(kb.Profile{ID: second, Name: "REVIEW"}); err == nil {
		t.Error("a profile renamed to another's name was accepted")
	}
	if err := k.SetProfile(kb.Profile{ID: "nope", Name: "X"}); err == nil {
		t.Error("changing a profile that does not exist was accepted")
	}
	if after := k.Knowledge().Profiles; !reflect.DeepEqual(after, want) {
		t.Errorf("after the rejected ones: %+v, want %+v", after, want)
	}

	must(t, k.SetProfile(kb.Profile{ID: second, Name: "local", Default: true, Engine: kb.EngineOpenCode, Effort: other.ID}))
	must(t, k.SetProfile(kb.Profile{ID: first, Name: "Review", Effort: effort.ID, Role: role}))
	must(t, k.SetProfile(kb.Profile{ID: second, Name: "local", Default: true, Engine: kb.EngineOpenCode, Effort: other.ID}))
	want = []kb.Profile{
		{ID: first, Name: "Review", Engine: kb.EngineClaude, Effort: effort.ID, Role: role},
		{ID: second, Name: "local", Default: true, Engine: kb.EngineOpenCode, Effort: other.ID},
	}
	if got := k.Knowledge().Profiles; !reflect.DeepEqual(got, want) {
		t.Fatalf("after changing both: %+v, want %+v, with the one default", got, want)
	}
	third, err := k.AddProfile(kb.Profile{Name: "Third", Default: true})
	must(t, err)
	for _, p := range k.Knowledge().Profiles {
		if p.Default != (p.ID == third) {
			t.Errorf("profile %s default = %v after a new one was added as the default", p.Name, p.Default)
		}
	}
	if after := testdb.KB(t, st).Knowledge().Profiles; !reflect.DeepEqual(after, k.Knowledge().Profiles) {
		t.Errorf("after reopening: %+v, want %+v", after, k.Knowledge().Profiles)
	}

	must(t, k.Merge(other.ID, effort.ID))
	if got := k.Knowledge().Profiles[1].Effort; got != effort.ID {
		t.Errorf("a profile filed under a merged effort is filed under %q, want the effort it was merged into", got)
	}
	must(t, k.Delete(effort.ID))
	must(t, k.Remove(kb.SectionRoles, role))
	for _, p := range k.Knowledge().Profiles {
		if p.Effort != "" || p.Role != "" {
			t.Errorf("profile %s keeps effort %q and role %q after both were deleted", p.Name, p.Effort, p.Role)
		}
	}
	must(t, k.RemoveProfile(first))
	if left := k.Knowledge().Profiles; len(left) != 2 || left[0].ID != second {
		t.Errorf("after removing the first: %+v", left)
	}
	if err := k.RemoveProfile(first); err == nil {
		t.Error("removing a profile that is gone was accepted")
	}
}
