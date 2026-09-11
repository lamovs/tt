package model

import (
	"encoding/json"
	"testing"
)

func TestEditListSurvivesJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    TaskEdit
		want    string
		cleared bool
	}{
		{"untouched", TaskEdit{}, `{}`, false},
		{
			"cleared, gathered as a nil list",
			TaskEdit{
				Reminders: NewEditList[string](nil),
				Tags:      NewEditList[string](nil),
				Items:     NewEditList[Item](nil),
			},
			`{"reminders":[],"tags":[],"items":[]}`,
			true,
		},
		{
			"cleared, gathered as an empty list",
			TaskEdit{
				Reminders: NewEditList([]string{}),
				Tags:      NewEditList([]string{}),
				Items:     NewEditList([]Item{}),
			},
			`{"reminders":[],"tags":[],"items":[]}`,
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.edit)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Fatalf("payload %s, want %s", b, tc.want)
			}
			var back TaskEdit
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("%v (payload %s)", err, b)
			}
			if !tc.cleared {
				if back.Reminders != nil || back.Tags != nil || back.Items != nil {
					t.Fatalf("untouched lists came back as an assignment: %+v", back)
				}
				return
			}
			if back.Reminders == nil || back.Tags == nil || back.Items == nil {
				t.Fatalf("a cleared list came back as \"leave it alone\": %+v (payload %s)", back, b)
			}
			if len(*back.Reminders) != 0 || len(*back.Tags) != 0 || len(*back.Items) != 0 {
				t.Fatalf("a cleared list came back with something in it: %+v", back)
			}
		})
	}
}

func TestEditListCarriesWhatItSets(t *testing.T) {
	e := TaskEdit{
		Reminders: NewEditList([]string{"TRIGGER:PT0S"}),
		Tags:      NewEditList([]string{"дом", "почта"}),
		Items:     NewEditList([]Item{{Id: "i1", Title: "паспорт", Status: ItemDone}}),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back TaskEdit
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("%v (payload %s)", err, b)
	}
	if back.Reminders == nil || len(*back.Reminders) != 1 || (*back.Reminders)[0] != "TRIGGER:PT0S" {
		t.Errorf("reminders came back as %v", back.Reminders)
	}
	if back.Tags == nil || len(*back.Tags) != 2 ||
		(*back.Tags)[0] != "дом" || (*back.Tags)[1] != "почта" {
		t.Errorf("tags came back as %v", back.Tags)
	}
	if back.Items == nil || len(*back.Items) != 1 {
		t.Fatalf("items came back as %v", back.Items)
	}
	if it := (*back.Items)[0]; it.Id != "i1" || it.Title != "паспорт" || !it.Status.Done() {
		t.Errorf("item came back as %+v", it)
	}
}

func TestOldEntryWithANullListIsLeftAlone(t *testing.T) {
	var e TaskEdit
	raw := `{"title":"Забрать посылку","reminders":null,"tags":null,"items":null}`
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.Title == nil || *e.Title != "Забрать посылку" {
		t.Fatalf("title came back as %v", e.Title)
	}
	if e.Reminders != nil || e.Tags != nil || e.Items != nil {
		t.Errorf("a null list in an older entry was read as a clear: %+v", e)
	}

	var cleared TaskEdit
	if err := json.Unmarshal([]byte(`{"reminders":[],"tags":[],"items":[]}`), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Reminders == nil || cleared.Tags == nil || cleared.Items == nil {
		t.Errorf("an empty list was read as \"leave it alone\": %+v", cleared)
	}
	var absent TaskEdit
	if err := json.Unmarshal([]byte(`{"title":"Забрать посылку"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Reminders != nil || absent.Tags != nil || absent.Items != nil {
		t.Errorf("a list nobody mentioned was read as a clear: %+v", absent)
	}
}

func TestOldBuildReadsANewEntryAsAClear(t *testing.T) {
	b, err := json.Marshal(TaskEdit{Tags: NewEditList[string](nil)})
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Tags *[]string `json:"tags,omitempty"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("%v (payload %s)", err, b)
	}
	if old.Tags == nil {
		t.Fatalf("an older build reads %s as leaving the tags alone", b)
	}
	if len(*old.Tags) != 0 {
		t.Errorf("tags came back as %v", *old.Tags)
	}

	b, err = json.Marshal(TaskEdit{Tags: NewEditList([]string{"дом", "почта"})})
	if err != nil {
		t.Fatal(err)
	}
	old.Tags = nil
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatalf("%v (payload %s)", err, b)
	}
	if old.Tags == nil {
		t.Fatalf("an older build reads %s as leaving the tags alone", b)
	}
	if len(*old.Tags) != 2 || (*old.Tags)[0] != "дом" || (*old.Tags)[1] != "почта" {
		t.Errorf("tags came back as %v", *old.Tags)
	}
}
