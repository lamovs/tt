package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/model"
)

func TestServerTaskQueryParserPreservesLocalDefaults(t *testing.T) {
	for _, verb := range []string{"ls", "s"} {
		if hasServerTaskQuery(&invocation{verb: verb}) {
			t.Fatal("local command routed remotely")
		}
	}
	inv := &invocation{verb: "ls", refinements: []string{"--filter", "--project", "id:p,id:q", "--priority", "0,5", "--status", "0", "--tag", "one,two"}}
	q, remote, err := parseServerTaskQuery(inv)
	if err != nil || remote || q.Mode != "filter" || q.ProjectIDs[0] != "p" || len(q.Priority) != 2 || q.Priority[0] != 0 {
		t.Fatalf("%+v %v %v", q, remote, err)
	}
	inv = &invocation{verb: "s", data: []string{"exact", "phrase"}, refinements: []string{"--server"}}
	q, remote, err = parseServerTaskQuery(inv)
	if err != nil || remote || q.Text != "exact phrase" || q.Mode != "search" {
		t.Fatalf("%+v %v %v", q, remote, err)
	}
	for _, inv := range []*invocation{
		{verb: "today", refinements: []string{"--server"}},
		{verb: "ls", refinements: []string{"--remote"}},
		{verb: "ls", refinements: []string{"--completed", "--filter"}},
		{verb: "s", data: []string{"x"}, refinements: []string{"--completed"}},
		{verb: "ls", refinements: []string{"--filter", "--priority", "high"}},
		{verb: "ls", refinements: []string{"--filter", "--project", "name:Work"}},
		{verb: "ls", refinements: []string{"--filter", "--remote", "--remote"}},
	} {
		if _, _, err := parseServerTaskQuery(inv); err == nil {
			t.Fatalf("accepted %+v", inv)
		}
	}
}

func TestServerTaskQueryJSONAndTextUseReadOnlyExactIDs(t *testing.T) {
	listing := app.ServerTaskListing{Query: app.ServerTaskQuery{Mode: "filter"}, Tasks: []model.Task{{Id: "id\x1b[2J", ProjectId: "p", Title: "bad\x1b]52;c;x\x07"}}, Raw: []json.RawMessage{json.RawMessage(`{"id":"id\u001b[2J","projectId":"p","title":"bad\u001b]52;c;x\u0007","future":9007199254740993}`)}, Warnings: []api.Warning{}, Meta: app.ResultMeta{Source: "remote", FetchedAt: time.Now().UTC(), Completeness: "unknown"}}
	var output bytes.Buffer
	inv := &invocation{ctx: context.Background(), verb: "ls", jsonOutput: true, rawOutput: true, resultWriter: &output, stdout: io.Discard}
	if inv.printServerTaskQuery(listing) != exitOK {
		t.Fatal("JSON failed")
	}
	var envelope struct {
		Raw  []json.RawMessage `json:"raw"`
		Data struct {
			ReadOnly bool `json:"read_only"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || !envelope.Data.ReadOnly || !strings.Contains(string(envelope.Raw[0]), "9007199254740993") {
		t.Fatalf("%s %v", output.String(), err)
	}
	output.Reset()
	inv.jsonOutput, inv.stdout = false, &output
	if inv.printServerTaskQuery(listing) != exitOK {
		t.Fatal("text failed")
	}
	if strings.Contains(output.String(), "\x1b") || !strings.Contains(output.String(), "Read-only server query") || !strings.Contains(output.String(), "id:") {
		t.Fatal("unsafe or ambiguous text output", output.String())
	}
}

func TestServerTaskQueryOfflineMachineDispatch(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	inv := &invocation{ctx: ctx}
	st, err := inv.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"id":"remote-only","projectId":"p","title":"cached"}]`)
	}))
	defer server.Close()
	service := app.NewServerTaskQueries(st, func() (*api.Client, error) {
		return api.NewClient("synthetic", "test", api.WithBaseURL(server.URL), api.WithHTTPClient(server.Client())), nil
	})
	for _, q := range []app.ServerTaskQuery{{Mode: "completed"}, {Mode: "search", Text: "cached"}} {
		if _, err := service.List(ctx, q, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"ls", "--completed"}, {"s", "cached", "--server"}} {
		result, code := runMachine(t, ctx, args...)
		if code != exitOK || result.Meta.Source != "local" || !bytes.Contains(result.Data, []byte("remote-only")) {
			t.Fatalf("%v %d %+v", args, code, result)
		}
	}
	if _, err := st.Task(ctx, "remote-only"); err == nil {
		t.Fatal("machine query cached authoritative task")
	}
}
