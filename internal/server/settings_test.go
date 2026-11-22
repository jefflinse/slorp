package server

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestPartialSettingsSavedTogetherBothStand(t *testing.T) {
	f := newFixture(t)
	body, more := io.Pipe()
	first := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/api/settings", body)
		if err != nil {
			first <- 0
			return
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := f.srv.Client().Do(req)
		if err != nil {
			first <- 0
			return
		}
		res.Body.Close()
		first <- res.StatusCode
	}()

	// The first save is still being read when the second is made.
	if _, err := more.Write([]byte(`{"model":{"name":"other-model"`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if code := f.post("/api/settings", `{"answerPermissions":false}`); code != http.StatusNoContent {
		t.Fatalf("second save = %d, want 204", code)
	}
	if _, err := more.Write([]byte(`}}`)); err != nil {
		t.Fatal(err)
	}
	more.Close()
	select {
	case code := <-first:
		if code != http.StatusNoContent {
			t.Fatalf("first save = %d, want 204", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first save never finished")
	}

	for name, s := range map[string]hub.Settings{"kept": f.hub.Settings(), "reopened": hub.New(f.kb, f.store).Settings()} {
		if s.AnswerPermissions || s.Model.Name != "other-model" {
			t.Errorf("%s settings answer permissions %v with model %q; want both saves to stand", name, s.AnswerPermissions, s.Model.Name)
		}
	}
}

func TestARejectedSettingsSaveChangesNothing(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/settings", `{"model":{"name":"kept"},"prompts":{"global":"Be brief."}}`); code/100 != 2 {
		t.Fatalf("save = %d, want it kept", code)
	}
	if code := f.post("/api/settings", `{"model":{"name":"lost","url":"ftp://nowhere"}}`); code != http.StatusBadRequest {
		t.Fatalf("save with a bad model address = %d, want 400", code)
	}
	if s := f.hub.Settings(); s.Model.Name != "kept" || s.Prompts.Global != "Be brief." {
		t.Errorf("after a rejected save, model %q and global prompt %q; want what was saved before", s.Model.Name, s.Prompts.Global)
	}
}
