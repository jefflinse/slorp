package hub

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestOrderedQuestionValidation(t *testing.T) {
	h, _, _ := newHub(t)
	p := h.Ask("opencode", "ses_1", Pending{Question: true, Ordered: true, Input: `{"questions":[
		{"question":"Choose","multiSelect":true,"options":[{"label":"A, B"},{"label":"C"}]},
		{"question":"Choose","custom":true,"options":[]}]}`}, true)
	if !p.Answerable {
		t.Fatal("ordered questions with repeated text should be answerable")
	}
	for _, picks := range [][][]string{
		nil, {{"C"}}, {{}, {"text"}}, {{"unknown"}, {"text"}}, {{"C", "C"}, {"text"}}, {{"C"}, {" "}}, {{"C"}, {"one", "two"}},
	} {
		if h.AnswerSelections(p.ID, picks) {
			t.Fatalf("accepted invalid answers: %#v", picks)
		}
	}
	want := [][]string{{"C", "A, B"}, {"custom, with comma"}}
	if !h.AnswerSelections(p.ID, want) {
		t.Fatal("valid answers refused")
	}
	dec, ok := h.Await(context.Background(), p.ID, time.Second)
	if !ok || dec.Behavior != Allow || !reflect.DeepEqual(dec.Selections, want) || dec.Answers != nil {
		t.Fatalf("answer = %+v, %v", dec, ok)
	}
	if h.AnswerSelections(p.ID, want) {
		t.Fatal("answered twice")
	}
	legacy := h.Ask(agent, "s2", Pending{Question: true, Input: sandwich}, true)
	if h.AnswerSelections(legacy.ID, [][]string{{"Tomato"}, {"Pickle"}}) {
		t.Fatal("ordered answers accepted for a legacy prompt")
	}
}
