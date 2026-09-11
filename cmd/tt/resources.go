package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/movsar/tt/internal/api"
	"github.com/movsar/tt/internal/app"
	"github.com/movsar/tt/internal/cli"
	"github.com/movsar/tt/internal/store"
)

func init() {
	for _, verb := range []string{"project", "folder", "tag", "habit", "comment", "countdown"} {
		register(command{help: resourceHelp(verb), run: cmdResources})
	}
}

func resourceHelp(verb string) cli.Help {
	commonRead := cli.HelpSection{Title: "Reading", Items: []string{
		"ls reads the local resource cache. Add --remote to fetch the collection explicitly from the server.",
		"--json includes versioned cache metadata. --raw requires --json and includes provider snapshots for read operations.",
		"Remote resource reads update their own cache; pending local state remains separate.",
	}}
	commonWrite := cli.HelpSection{Title: "Saving changes", Items: []string{
		"Writes are local-first. --preview returns an exact preview ID; repeat the same action with --accept ID to enqueue it.",
		"Mutation actions use cached state. Refresh with a separate --remote read before preparing a change.",
		"tt sync sends supported queued operations. Uncertain allocating requests are not blindly repeated.",
	}}
	seeAlso := []string{"sync", "ui", "show", "auth"}

	switch verb {
	case "project":
		return cli.Help{
			Verb: "project", Summary: "inspect projects and manage supported project fields",
			Examples: []cli.Example{
				{Cmd: "tt project ls --remote", What: "fetch projects from the server"},
				{Cmd: "tt project show PROJECT_ID --tasks", What: "show one project with cached tasks and columns"},
				{Cmd: "tt project add Study --preview", What: "preview a new project"},
				{Cmd: "tt project edit PROJECT_ID --name University --preview", What: "preview a project rename"},
				{Cmd: "tt project column ls --project PROJECT_ID --remote", What: "fetch Kanban columns for one project"},
				{Cmd: "tt project column add Doing --project PROJECT_ID --preview", What: "preview a new Kanban column"},
			},
			Sections: []cli.HelpSection{
				commonRead,
				{Title: "Supported project changes", Items: []string{
					"Projects support create and update for name, color, sortOrder, viewMode and kind. Use --resource-color for resource color; global --color only controls terminal output.",
					"Nonempty project deletion requires a separate verified cascade decision. groupId and closed are read-only here.",
				}},
				{Title: "Kanban columns", Items: []string{
					"project column ls, add and edit require --project ID. Column names can be created and renamed; deletion and reordering are unavailable.",
					"Move a task with tt edit TASK --column COLUMN_ID --preview, or use b inside the selected project in tt ui.",
				}},
				commonWrite,
			}, SeeAlso: seeAlso,
		}
	case "folder":
		return cli.Help{
			Verb: "folder", Summary: "inspect project folders and manage supported folder fields",
			Examples: []cli.Example{
				{Cmd: "tt folder ls --remote", What: "fetch project folders from the server"},
				{Cmd: "tt folder show FOLDER_ID", What: "show one cached folder"},
				{Cmd: "tt folder add Study --preview", What: "preview a new folder"},
				{Cmd: "tt folder edit FOLDER_ID --name University --preview", What: "preview a folder rename"},
			},
			Sections: []cli.HelpSection{
				commonRead,
				{Title: "Supported folder changes", Items: []string{
					"Folders support create and rename. Names are limited to 64 characters.",
					"Project membership, folder ordering and cascade effects are not verified. tt does not perform an unverified container cascade.",
				}},
				commonWrite,
			}, SeeAlso: seeAlso,
		}
	case "tag":
		return cli.Help{
			Verb: "tag", Summary: "inspect tags and create supported tags",
			Examples: []cli.Example{
				{Cmd: "tt tag ls --remote", What: "fetch tags from the server"},
				{Cmd: "tt tag show study", What: "show one cached tag"},
				{Cmd: "tt tag add Study --preview", What: "preview a tag named study with label Study"},
			},
			Sections: []cli.HelpSection{
				commonRead,
				{Title: "Supported tag changes", Items: []string{
					"Tags support creation only. Names are trimmed and lowercased; the display label preserves the supplied spelling.",
					"Tag names and labels are limited to 64 characters. Rename and delete are unavailable.",
					"Assigning tags to tasks is a task update, not a tag-resource mutation.",
				}},
				commonWrite,
			}, SeeAlso: seeAlso,
		}
	case "habit":
		return cli.Help{
			Verb: "habit", Summary: "inspect habits, manage supported fields and record check-ins",
			Examples: []cli.Example{
				{Cmd: "tt habit ls --remote", What: "fetch habits from the server"},
				{Cmd: "tt habit show HABIT_ID", What: "show one cached habit"},
				{Cmd: "tt habit add Reading --preview", What: "preview a new habit"},
				{Cmd: "tt habit history HABIT_ID --from 2026-09-01 --to 2026-09-11 --remote", What: "fetch a dated check-in range"},
				{Cmd: "tt habit checkin HABIT_ID --date 2026-09-11 --value 1 --preview", What: "preview an absolute value for one date"},
			},
			Sections: []cli.HelpSection{
				commonRead,
				{Title: "Supported habit changes", Items: []string{
					"Habits support create and update for documented fields. Names are limited to 1000 characters; unknown provider values remain readable.",
					"Habit deletion is unavailable.",
				}},
				{Title: "Check-ins", Items: []string{
					"history requires a habit ID and --from/--to calendar dates. checkin requires --date YYYY-MM-DD and --value.",
					"A check-in sets an absolute value for one date; it never increments the existing value. Refresh that date before the first preview.",
				}},
				commonWrite,
			}, SeeAlso: seeAlso,
		}
	case "comment":
		return cli.Help{
			Verb: "comment", Summary: "inspect, add and remove task comments",
			Examples: []cli.Example{
				{Cmd: "tt comment ls TASK --remote", What: "fetch comments for one task"},
				{Cmd: "tt comment show TASK COMMENT_ID", What: "show one cached comment"},
				{Cmd: `tt comment add TASK "Review notes" --preview`, What: "preview a new comment"},
				{Cmd: "tt comment rm TASK COMMENT_ID --preview", What: "preview deletion of one exact comment"},
			},
			Sections: []cli.HelpSection{
				{Title: "Task scope", Items: []string{
					"Every comment action starts with a task reference. Remove also requires the exact comment ID.",
					"Comments are cached separately for each task. Add --remote to ls or show to refresh from the server.",
				}},
				{Title: "Supported comment changes", Items: []string{
					"Comments support create and delete. Editing an existing comment and creating replies are unavailable.",
					"A lost create response can be ambiguous, so uncertain comment creation is not blindly repeated.",
				}},
				commonRead,
				commonWrite,
			}, SeeAlso: seeAlso,
		}
	case "countdown":
		return cli.Help{
			Verb: "countdown", Summary: "inspect read-only calendar countdowns",
			Examples: []cli.Example{
				{Cmd: "tt countdown ls --remote", What: "fetch countdowns from the server"},
				{Cmd: "tt countdown show COUNTDOWN_ID", What: "show one cached countdown"},
				{Cmd: "tt countdown ls --json --raw", What: "include versioned metadata and provider snapshots"},
			},
			Sections: []cli.HelpSection{
				commonRead,
				{Title: "Read-only resource", Items: []string{
					"Countdown creation, update and deletion are unavailable. Unknown calendar, style and status values are preserved when read.",
					"Calendar countdowns are unrelated to the local tt pomodoro focus timer.",
				}},
			}, SeeAlso: []string{"pomodoro", "ui", "auth"},
		}
	default:
		panic("unknown resource help: " + verb)
	}
}

type resourceArguments struct {
	remote, preview, tasks, cascade bool
	accept, project, from, to, date string
	dateSet, rangeSet               bool
	fields                          map[string]any
}

func parseResourceArguments(args []string) (resourceArguments, error) {
	out := resourceArguments{fields: map[string]any{}}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return out, fmt.Errorf("repeated option %s", flag)
		}
		seen[flag] = true
		switch flag {
		case "--remote":
			out.remote = true
			continue
		case "--preview":
			out.preview = true
			continue
		case "--tasks":
			out.tasks = true
			continue
		case "--cascade":
			out.cascade = true
			continue
		}
		if i+1 >= len(args) {
			return out, fmt.Errorf("missing value for %s", flag)
		}
		i++
		value := args[i]
		switch flag {
		case "--accept":
			out.accept = value
		case "--project":
			out.project = value
		case "--from":
			out.from = value
			out.rangeSet = true
		case "--to":
			out.to = value
			out.rangeSet = true
		case "--date":
			out.date = value
			out.dateSet = true
		case "--name", "--label", "--title", "--type", "--unit":
			out.fields[strings.TrimPrefix(flag, "--")] = value
		case "--resource-color":
			out.fields["color"] = value
		case "--view-mode":
			out.fields["viewMode"] = value
		case "--kind":
			out.fields["kind"] = value
		case "--repeat":
			out.fields["repeatRule"] = value
		case "--section":
			out.fields["sectionId"] = value
		case "--sort-order":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return out, errors.New("sort order must be an int64")
			}
			out.fields["sortOrder"] = n
		case "--value", "--goal", "--step":
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return out, errors.New("value, goal and step must be finite numbers")
			}
			out.fields[strings.TrimPrefix(flag, "--")] = n
		default:
			return out, fmt.Errorf("unknown resource option %s", flag)
		}
	}
	if out.preview && out.accept != "" {
		return out, errors.New("choose --preview or --accept")
	}
	if out.accept != "" && (len(out.fields) > 0 || out.remote || out.cascade || out.date != "") {
		return out, errors.New("--accept applies the saved preview; do not supply new fields")
	}
	return out, nil
}

func (inv *invocation) resourceService(st *store.Store) *app.Resources {
	return app.NewResources(st, func() (*api.Client, error) {
		token, err := syncToken()
		if err != nil {
			return nil, err
		}
		return managedAPIClient(token), nil
	})
}

func cmdResources(inv *invocation) int {
	args, err := parseResourceArguments(inv.refinements)
	if err != nil {
		return inv.misuse("%s", err.Error())
	}
	kind := inv.verb
	data := append([]string(nil), inv.data...)
	if kind == "project" && len(data) > 0 && data[0] == "column" {
		kind = "column"
		data = data[1:]
	}
	action := "ls"
	if len(data) > 0 {
		action = data[0]
		data = data[1:]
	}
	if kind == "habit" && (action == "history" || action == "checkin") {
		kind = "checkin"
		if action == "history" {
			action = "ls"
		} else {
			action = "add"
		}
	}
	if action != "ls" && action != "show" && action != "add" && action != "edit" && action != "rm" {
		return inv.misuse("unknown resource action")
	}
	if args.accept != "" && (len(data) != 0 || args.project != "" || args.from != "" || args.to != "" || args.tasks) {
		return inv.misuse("--accept applies only the saved preview; omit targets and query options")
	}
	if kind != "checkin" && (args.dateSet || args.rangeSet) {
		return inv.misuse("date options require habit history or checkin")
	}
	if kind == "checkin" && ((action == "ls" || action == "show") && args.dateSet || (action != "ls" && action != "show") && args.rangeSet) {
		return inv.misuse("use --from/--to only with habit history and --date only with habit checkin")
	}
	if kind != "column" && args.project != "" {
		return inv.misuse("--project is only used for columns")
	}
	if args.tasks && (kind != "project" || action != "show") {
		return inv.misuse("--tasks requires project show")
	}
	if args.tasks && inv.rawOutput {
		return inv.misuse("--raw is unavailable for the combined project/task view; read its resources separately")
	}
	if args.cascade {
		return inv.misuse("container cascade is unavailable until provider consequences are verified")
	}
	st, err := inv.openStore()
	if err != nil {
		return inv.fail(err)
	}
	defer st.Close()
	service := inv.resourceService(st)
	query := app.ResourceQuery{Kind: kind}
	if kind == "column" && args.accept == "" {
		if args.project == "" {
			return inv.misuse("columns need --project ID")
		}
		project, err := st.Project(inv.ctx, strings.TrimPrefix(args.project, "id:"))
		if errors.Is(err, store.ErrNotFound) && !strings.HasPrefix(args.project, "id:") {
			project, err = inv.resolver(st).Project(inv.ctx, args.project)
		}
		if err != nil {
			return inv.fail(err)
		}
		query.ProjectID = project.Id
	}
	if kind == "comment" {
		if len(data) == 0 && args.accept == "" {
			return inv.misuse("comments need a task reference")
		}
		if len(data) > 0 {
			id, code, ok := inv.oneFeatureTask(st, data[0])
			if !ok {
				return code
			}
			task, err := st.Task(inv.ctx, id)
			if err != nil {
				return inv.fail(err)
			}
			query.ProjectID, query.TaskID = task.ProjectId, task.Id
			data = data[1:]
		}
	}
	if kind == "checkin" {
		if len(data) == 0 && args.accept == "" {
			return inv.misuse("check-ins need a habit ID")
		}
		if len(data) > 0 {
			query.HabitID = data[0]
			data = data[1:]
		}
		query.From, query.To = resourceDateStamp(args.from), resourceDateStamp(args.to)
	}
	reading := action == "ls" || action == "show"
	if reading {
		if args.preview || args.accept != "" || len(args.fields) > 0 || args.cascade {
			return inv.misuse("read actions do not accept mutation options")
		}
		listing, err := service.List(inv.ctx, query, args.remote)
		if err != nil {
			return inv.fail(err)
		}
		if action == "show" {
			if len(data) != 1 {
				return inv.misuse("show needs one exact ID or cached name")
			}
			entity, err := resolveResource(listing.Entities, data[0])
			if err != nil {
				return inv.fail(err)
			}
			if args.tasks {
				if kind != "project" {
					return inv.misuse("--tasks requires project show")
				}
				tasks, err := st.Tasks(inv.ctx, store.TaskFilter{ProjectID: entity.ServerID})
				if err != nil {
					return inv.fail(err)
				}
				columns, err := service.List(inv.ctx, app.ResourceQuery{Kind: "column", ProjectID: entity.ServerID}, args.remote)
				if err != nil {
					return inv.fail(err)
				}
				if inv.jsonOutput {
					return inv.output(map[string]any{"project": resourceView(entity), "tasks": tasks, "task_source": "local", "columns": resourceViews(columns.Entities), "column_meta": columns.Meta}, listing.Meta)
				}
				if code := printResourceEntities(inv, []store.ResourceEntity{entity}, listing.Meta); code != exitOK {
					return code
				}
				fmt.Fprintln(inv.stdout, "Tasks from the local task cache:")
				for _, task := range tasks {
					fmt.Fprintln(inv.stdout, cli.ReportLine("  ", task.Title, "", nil))
				}
				return printResourceEntities(inv, columns.Entities, columns.Meta)
			}
			listing.Entities = []store.ResourceEntity{entity}
		} else if len(data) != 0 {
			return inv.misuse("ls takes no extra arguments")
		}
		return printResourceEntities(inv, listing.Entities, listing.Meta)
	}
	if inv.rawOutput {
		return inv.misuse("--raw is read-only")
	}
	operation := map[string]string{"add": "create", "edit": "update", "rm": "delete"}[action]
	if args.accept != "" {
		outcome, err := service.Apply(inv.ctx, args.accept, kind, operation)
		if err != nil {
			return inv.fail(err)
		}
		if inv.jsonOutput {
			result := app.Result(inv.verb, outcome, app.ResultMeta{Source: "local", Pending: 1})
			result.Status = "queued"
			return inv.writeResult(result)
		}
		cli.WriteLines(inv.stdout, cli.Wrap(fmt.Sprintf("queued operation %d; run tt sync", outcome.OperationSeq), cli.Width))
		return exitOK
	}
	if args.remote || args.tasks {
		return inv.misuse("mutation actions use the local cache; refresh separately")
	}
	mutation := store.EntityMutation{Ref: store.EntityRef{Kind: kind}, Action: operation}
	switch kind {
	case "column":
		mutation.ProjectKey = query.ProjectID
	case "comment":
		mutation.ProjectKey = query.TaskID
	case "checkin":
		mutation.ProjectKey = query.HabitID
	}
	if operation != "create" {
		if len(data) != 1 {
			return inv.misuse("edit and rm need one exact ID or cached name")
		}
		entities, err := st.Entities(inv.ctx, kind, mutation.ProjectKey, false)
		if err != nil {
			return inv.fail(err)
		}
		entity, err := resolveResource(entities, data[0])
		if err != nil {
			return inv.fail(err)
		}
		mutation.Ref = entity.Ref
	} else {
		if len(data) > 0 {
			field := "name"
			if kind == "comment" {
				field = "title"
			}
			if _, exists := args.fields[field]; exists {
				return inv.misuse("provide the name or title once")
			}
			args.fields[field] = strings.Join(data, " ")
		}
		if kind == "tag" {
			if name, ok := args.fields["name"].(string); ok {
				args.fields["name"] = strings.ToLower(strings.TrimSpace(name))
				if _, exists := args.fields["label"]; !exists {
					args.fields["label"] = strings.TrimSpace(name)
				}
			}
		}
		if kind == "checkin" {
			stamp := resourceDateStamp(args.date)
			if _, err := time.Parse("20060102", stamp); err != nil {
				return inv.misuse("checkin needs --date YYYY-MM-DD")
			}
			n, _ := strconv.Atoi(stamp)
			args.fields["stamp"] = n
		}
	}
	mutation.Patch, err = json.Marshal(args.fields)
	if err != nil {
		return inv.misuse("invalid resource field value")
	}
	preview, err := service.Prepare(inv.ctx, mutation)
	if err != nil {
		return inv.fail(err)
	}
	if inv.jsonOutput {
		result := app.Result(inv.verb, preview, app.ResultMeta{Source: "local"})
		result.Status = "preview"
		return inv.writeResult(result)
	}
	fmt.Fprintln(inv.stdout, cli.ReportLine("Preview ", preview.ID, "", nil))
	cli.WriteLines(inv.stdout, cli.Wrap(operation+" "+kind+"; "+preview.Undo, cli.Width))
	fmt.Fprintln(inv.stdout, "Accept with the same action and --accept followed by the preview ID.")
	return exitOK
}

func resourceDateStamp(value string) string { return strings.ReplaceAll(value, "-", "") }

type resourceOutput struct {
	Ref      store.EntityRef `json:"ref"`
	ServerID string          `json:"server_id,omitempty"`
	Data     json.RawMessage `json:"data"`
	Revision int64           `json:"revision"`
	Pending  bool            `json:"pending"`
}

func resourceView(entity store.ResourceEntity) resourceOutput {
	return resourceOutput{Ref: entity.Ref, ServerID: entity.ServerID, Data: entity.Data, Revision: entity.Revision, Pending: entity.Dirty}
}

func resourceViews(entities []store.ResourceEntity) []resourceOutput {
	out := make([]resourceOutput, 0, len(entities))
	for _, entity := range entities {
		out = append(out, resourceView(entity))
	}
	return out
}

func resourceLabel(entity store.ResourceEntity) string {
	var fields struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Label string `json:"label"`
	}
	if json.Unmarshal(entity.Data, &fields) != nil {
		return entity.Ref.Key
	}
	if fields.Label != "" {
		return fields.Label
	}
	if fields.Name != "" {
		return fields.Name
	}
	if fields.Title != "" {
		return fields.Title
	}
	return entity.Ref.Key
}

func resolveResource(entities []store.ResourceEntity, query string) (store.ResourceEntity, error) {
	key := strings.TrimPrefix(query, "id:")
	for _, entity := range entities {
		if entity.Ref.Key == key || entity.ServerID == key {
			return entity, nil
		}
	}
	if strings.HasPrefix(query, "id:") {
		return store.ResourceEntity{}, store.ErrNotFound
	}
	name := strings.TrimPrefix(query, "name:")
	var matches []store.ResourceEntity
	for _, entity := range entities {
		if strings.EqualFold(resourceLabel(entity), name) {
			matches = append(matches, entity)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return store.ResourceEntity{}, errors.New("ambiguous resource name; use an exact ID")
	}
	return store.ResourceEntity{}, errors.New("resource is not cached; list with --remote and use an exact ID")
}

func printResourceEntities(inv *invocation, entities []store.ResourceEntity, meta app.ResultMeta) int {
	if inv.jsonOutput {
		result := app.Result(inv.verb, resourceViews(entities), meta)
		if inv.rawOutput {
			raw := make([]json.RawMessage, 0, len(entities))
			for _, entity := range entities {
				raw = append(raw, entity.Base)
			}
			result.Raw, _ = json.Marshal(raw)
		}
		return inv.writeResult(result)
	}
	if len(entities) == 0 {
		cli.WriteLines(inv.stdout, []string{"No cached resources in this view."})
	}
	for _, entity := range entities {
		pending := ""
		if entity.Dirty {
			pending = " [pending]"
		}
		fmt.Fprintln(inv.stdout, cli.ReportLine("id: ", entity.Ref.Key, "", nil))
		fmt.Fprintln(inv.stdout, cli.ReportLine("  ", resourceLabel(entity), pending, nil))
	}
	cli.WriteLines(inv.stdout, cli.Wrap("Source: "+meta.Source+"; completeness: "+meta.Completeness, cli.Width))
	return exitOK
}
