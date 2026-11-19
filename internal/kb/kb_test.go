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

	must(t, k.Edit(e.ID, &name, nil))
	if got := k.Efforts()[0]; got.ID != e.ID || got.Name != "Gadgets" || got.About != "Making widgets" {
		t.Errorf("after a rename: %+v, want the same effort by another name", got)
	}
	must(t, k.Edit(e.ID, nil, &about))
	if got := k.Efforts()[0]; got.Name != "Gadgets" || got.About != "Making gadgets" {
		t.Errorf("after a new description: %+v", got)
	}
	must(t, k.Edit(e.ID, nil, &blank))
	if got := k.Efforts()[0]; got.About != "" {
		t.Errorf("a description could not be cleared: %q", got.About)
	}
	if k.Edit(e.ID, &blank, nil) == nil || k.Efforts()[0].Name != "Gadgets" {
		t.Error("an effort was left without a name")
	}
	if k.Edit("nope", &name, nil) == nil {
		t.Error("an unknown effort was edited")
	}
	if got := testdb.KB(t, st).Efforts()[0]; got.Name != "Gadgets" {
		t.Errorf("after reopen the effort is named %q", got.Name)
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
		{"rename effort", func() { name := "B"; must(t, k.Edit(a, &name, nil)) }, true, false},
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
		if got, via, ok := k.ResolveRepo(name, []string{"/work/b"}); got != a || via != kb.ViaUser || !ok {
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
