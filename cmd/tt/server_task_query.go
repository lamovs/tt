package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func hasServerTaskQuery(inv *invocation) bool {
	for _, flag := range inv.refinements {
		switch flag {
		case "--remote", "--server", "--completed", "--filter":
			return true
		}
	}
	return false
}

func parseServerTaskQuery(inv *invocation) (app.ServerTaskQuery, bool, error) {
	var q app.ServerTaskQuery
	var remote bool
	if inv.verb != "ls" && inv.verb != "s" {
		return q, false, errors.New("server queries use tt ls or tt s; today remains local")
	}
	if inv.verb == "s" {
		q.Mode = "search"
		q.Text = strings.Join(inv.data, " ")
	} else if len(inv.data) > 0 {
		return q, false, errors.New("server listing takes no positional arguments")
	}
	seen := map[string]bool{}
	for i := 0; i < len(inv.refinements); i++ {
		flag := inv.refinements[i]
		if seen[flag] {
			return q, false, errors.New("repeated server query option")
		}
		seen[flag] = true
		switch flag {
		case "--remote":
			remote = true
			continue
		case "--server":
			continue
		case "--completed", "--filter":
			if q.Mode != "" {
				return q, false, errors.New("choose one server query mode")
			}
			q.Mode = strings.TrimPrefix(flag, "--")
			continue
		}
		if i+1 >= len(inv.refinements) {
			return q, false, errors.New("server query option needs a value")
		}
		i++
		value := inv.refinements[i]
		switch flag {
		case "--from":
			q.From = value
		case "--to":
			q.To = value
		case "--project":
			for _, id := range strings.Split(value, ",") {
				id = strings.TrimPrefix(id, "id:")
				if strings.HasPrefix(id, "name:") {
					return q, false, errors.New("server queries require exact project IDs")
				}
				q.ProjectIDs = append(q.ProjectIDs, id)
			}
		case "--tag":
			q.Tags = strings.Split(value, ",")
		case "--kind":
			q.Kind = strings.Split(value, ",")
		case "--priority", "--status":
			var values []int
			for _, word := range strings.Split(value, ",") {
				n, err := strconv.Atoi(word)
				if err != nil {
					return q, false, errors.New("priority and status need comma-separated wire integers")
				}
				values = append(values, n)
			}
			if flag == "--priority" {
				q.Priority = values
			} else {
				q.Status = values
			}
		default:
			return q, false, errors.New("unknown server query option")
		}
	}
	if q.Mode == "" {
		return q, false, errors.New("server listing needs --completed or --filter")
	}
	return q, remote, nil
}

func (inv *invocation) serverTaskQuery() int {
	q, remote, err := parseServerTaskQuery(inv)
	if err != nil {
		return inv.misuse("%s", err.Error())
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	service := app.NewServerTaskQueries(st, func() (*api.Client, error) {
		token, err := syncToken()
		if err != nil {
			return nil, err
		}
		return managedAPIClient(token), nil
	})
	listing, err := service.List(inv.ctx, q, remote)
	if err != nil {
		return inv.fail(err)
	}
	return inv.printServerTaskQuery(listing)
}

func (inv *invocation) printServerTaskQuery(listing app.ServerTaskListing) int {
	if inv.jsonOutput {
		result := app.Result(inv.verb, struct {
			Query    app.ServerTaskQuery `json:"query"`
			Tasks    any                 `json:"tasks"`
			Warnings []api.Warning       `json:"conversion_warnings"`
			ReadOnly bool                `json:"read_only"`
		}{listing.Query, listing.Tasks, listing.Warnings, true}, listing.Meta)
		result.Warnings = append(result.Warnings, "Server query coverage is not authoritative; these rows do not update local tasks or numbered references.")
		if len(listing.Warnings) > 0 {
			result.Warnings = append(result.Warnings, "Some provider fields need interpretation; inspect conversion_warnings and --raw.")
		}
		if inv.rawOutput {
			result.Raw, _ = json.Marshal(listing.Raw)
		}
		return inv.writeResult(result)
	}
	entities := make([]store.ResourceEntity, 0, len(listing.Tasks))
	for i, task := range listing.Tasks {
		entities = append(entities, store.ResourceEntity{Ref: store.EntityRef{Kind: "server-task", Key: task.Id}, ServerID: task.Id, Data: listing.Raw[i], Base: listing.Raw[i]})
	}
	if len(entities) == 0 {
		cli.WriteLines(inv.stdout, []string{"No rows in this server query; this does not prove an empty account."})
		cli.WriteLines(inv.stdout, cli.Wrap("Source: "+listing.Meta.Source+"; completeness: "+listing.Meta.Completeness, cli.Width))
	} else {
		printResourceEntities(inv, entities, listing.Meta)
	}
	cli.WriteLines(inv.stdout, cli.Wrap("Read-only server query. Local tasks and numbered references are unchanged. Coverage is not authoritative.", cli.Width))
	if len(listing.Warnings) > 0 {
		cli.WriteLines(inv.stdout, cli.Wrap("Some provider fields need interpretation. Use --json --raw to inspect conversion warnings and source data.", cli.Width))
	}
	return exitOK
}
