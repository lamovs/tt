package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestCreateDefaultListNeverFallsBack(t *testing.T) {
	for _, tc := range []struct{ value, explicit, want string }{
		{"id:home", "", "home"}, {"ome", "", "home"},
		{"missing", "", ""}, {"id:gone", "", ""}, {"", "", ""},
		{"id:home", "work", "work"}, {"missing", "work", "work"},
		{"id:home", "gone", ""},
	} {
		m, _ := writableModel(t)
		m.defaultProject, m.query.ProjectID = tc.value, tc.explicit
		m.openCreate()
		if got := m.form.draft().ProjectID; got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
		if tc.want == "" && m.form.err == "" {
			t.Errorf("%+v: missing explanation", tc)
		}
	}
	m, st := writableModel(t)
	m.defaultProject = "missing"
	m.openCreate()
	m.form.title.set("Must choose")
	before := rowsIn(t, st, "tasks")
	m, cmd := m.saveForm()
	if cmd != nil || rowsIn(t, st, "tasks") != before {
		t.Fatal("saved without an explicit list")
	}
	m.projects = []model.Project{{Id: "a", Name: "Same"}, {Id: "b", Name: "Same"}, {Id: "closed", Name: "Closed", Closed: true}}
	for _, value := range []string{"Same", "Closed", "id:closed"} {
		m.defaultProject = value
		m.openCreate()
		if m.form.project != -1 || len(m.form.projects) != 2 {
			t.Fatalf("%q silently selected or offered a closed list", value)
		}
	}
}

func TestCreateRechecksClosedDefaultAtSave(t *testing.T) {
	m, st := writableModel(t)
	m.openCreate()
	m.form.title.set("Closed while editing")
	before := rowsIn(t, st, "tasks")
	if err := st.ReplaceProjects(context.Background(), []model.Project{{Id: "work", Name: "Work"}, {Id: "home", Name: "Home", Closed: true}}); err != nil {
		t.Fatal(err)
	}
	m, cmd := m.saveForm()
	m = finishLocal(t, m, cmd)
	if rowsIn(t, st, "tasks") != before || m.form == nil || !strings.Contains(m.form.err, "closed") {
		t.Fatalf("closed target saved or draft lost: %+v", m.form)
	}
}
