package cli

import (
	"context"
	"testing"

	"github.com/movsar/tt/internal/model"
)

func TestDefaultProjectStableIdentityAndLegacyNames(t *testing.T) {
	projects := []model.Project{
		{Id: "p1", Name: "Work", Kind: "TASK"},
		{Id: "p2", Name: "Work", Kind: "TASK"},
		{Id: "p3", Name: "Personal", Kind: "NOTE"},
		{Id: "p4", Name: "Closed", Closed: true},
		{Id: "p5", Name: "Unknown", Kind: "OTHER"},
		{Id: "p6", Name: "id:p1"},
	}
	st := testStore(t)
	if err := st.ReplaceProjects(context.Background(), projects); err != nil {
		t.Fatal(err)
	}
	r, _, _ := newResolver(st, "")
	for _, tc := range []struct{ value, want string }{
		{"id:p1", "p1"}, {"id:p2", "p2"}, {"personal", "p3"},
		{"erson", "p3"}, {"name:id:p1", "p6"}, {"Work", ""},
		{"Closed", ""}, {"id:p4", ""}, {"Unknown", ""},
		{"missing", ""}, {"id:missing", ""}, {"id:P1", ""},
	} {
		p, err := r.DefaultProject(context.Background(), tc.value)
		if p.Id != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("%q: %+v %v; want %q", tc.value, p, err, tc.want)
		}
	}
	projects[0].Name = "Renamed"
	if err := st.ReplaceProjects(context.Background(), projects); err != nil {
		t.Fatal(err)
	}
	if p, err := r.DefaultProject(context.Background(), "id:p1"); err != nil || p.Name != "Renamed" {
		t.Fatalf("rename: %+v %v", p, err)
	}
	if _, err := ResolveDefaultProject(nil, "Work"); err == nil {
		t.Fatal("empty cache resolved")
	}
}
