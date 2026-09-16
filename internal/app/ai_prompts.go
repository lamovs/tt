package app

import (
	"encoding/json"
	"slices"
)

// The schemas are written for both claude --json-schema and codex
// --output-schema, whose strict mode wants every object closed and every
// property required: a field that may be left out is nullable instead.

func aiObject(props map[string]any) map[string]any {
	required := make([]string, 0, len(props))
	for name := range props {
		required = append(required, name)
	}
	slices.Sort(required)
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
}

func aiNullableString(description string) map[string]any {
	return map[string]any{"type": []string{"string", "null"}, "description": description}
}

func aiNullableStrings(description string) map[string]any {
	return map[string]any{"type": []string{"array", "null"}, "items": map[string]any{"type": "string"}, "description": description}
}

func aiSchema(root map[string]any) []byte {
	data, err := json.Marshal(root)
	if err != nil {
		panic("tt: an AI schema does not marshal: " + err.Error())
	}
	return data
}

// AIPlanSchema is the reply tt ai asks for: a list of operations and optional
// notes for the user.
func AIPlanSchema() []byte {
	op := aiObject(map[string]any{
		"op":        map[string]any{"type": "string", "enum": aiOpNames},
		"ref":       aiNullableString("ref of an existing task from the context"),
		"title":     aiNullableString("task title in the user's language"),
		"project":   aiNullableString("list name exactly as in the context"),
		"tags":      aiNullableStrings("tag names exactly as in the context"),
		"content":   aiNullableString("task notes in the user's language"),
		"checklist": aiNullableStrings("checklist item titles"),
		"due":       aiNullableString("due date in tt date words"),
		"schedule":  aiNullableString("START + DURATION in tt date words"),
		"reminders": aiNullableStrings("reminders: at, or -10min style offsets"),
		"priority":  aiNullableString("none, low, medium or high"),
	})
	return aiSchema(aiObject(map[string]any{
		"ops":   map[string]any{"type": "array", "items": op},
		"notes": aiNullableString("short note for the user, or null"),
	}))
}

// AIFindSchema is the filter tt ai find asks for.
func AIFindSchema() []byte {
	return aiSchema(aiObject(map[string]any{
		"keywords":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"project":   aiNullableString("list name exactly as in the context"),
		"status":    aiNullableString("open, done or all"),
		"due_from":  aiNullableString("tt date words, inclusive"),
		"due_to":    aiNullableString("tt date words, inclusive"),
		"done_from": aiNullableString("tt date words, inclusive"),
		"done_to":   aiNullableString("tt date words, inclusive"),
	}))
}

// AIRerankSchema is the ranking tt ai find asks for when it has many
// candidates.
func AIRerankSchema() []byte {
	return aiSchema(aiObject(map[string]any{
		"ranked": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
	}))
}

const aiDateWords = `Date words (case-insensitive):
  today, tmr (tomorrow), yst (yesterday), eow (end of this week, Sunday), eom (end of this month)
  mon tue wed thu fri sat sun   the next such weekday strictly after today (never today itself)
  next fri                      one week after "fri"
  +3d, +2w, +1m                 days, weeks or calendar months from today (m is a month)
  2026-10-21, 21.10.2026        an explicit date; 21.10, 21oct or oct21 mean the next such date
  in 2h, in 30min, in 1h30min   elapsed time from now; +2h and +30min mean the same
A date may be followed by a clock: "tmr 09:00", "fri 18:30", "21.10 14:00", "today 9am", "mon 6:30pm". A date without a clock is all-day. A clock alone is not a date: write "today 14:00".
Durations use h and min only, never m: 45min, 2h, 1h30min.`

const aiPlanSystem = `You turn a request to a to-do list app called tt into a list of operations. You never change anything yourself: tt validates your operations, shows them to the user as one preview, and applies or discards them together.

Reply with one JSON object matching the schema: {"ops": [...], "notes": string or null}. Every op has every field; use null for fields that do not apply.

Operations ("op"):
- "add": create a task. Needs "title". May set "project", "tags", "content", "checklist", "due" or "schedule" (not both), "reminders", "priority".
- "edit": change an existing task named by "ref". May set "title" (replaces it), "tags" (added to its tags), "content" (appended to its notes), "due", "reminders" (added to its reminders), "priority".
- "schedule": give the task "ref" a start and a planned duration with "schedule".
- "done": complete the task "ref".
- "move": move the task "ref" to the list named by "project". A move must be the only operation on its task.
- "checklist_add": append the items in "checklist" to the task "ref".
There is no delete. When part of a request cannot be expressed with these operations, return what you can and explain the rest briefly in "notes". When nothing can be done, return an empty "ops" list and say why in "notes".

Rules:
- The request is often in Russian. Keep titles, notes and checklist items in the user's language; never translate them. Keep titles short and put details in "content".
- "ref" must be a ref from the context's task list. Never invent refs. If no listed task matches what the user means, say so in "notes".
- "project" must be a list name from the context, written exactly as there. Use null for the user's default list. Never invent lists.
- "tags" must be tag names from the context. Never invent tags.
- "priority" is none, low, medium or high.
- Never compute calendar dates or time zones yourself. Write every date in tt's date words below; tt resolves them against the current time from the context.

"due" takes date words, or "none" to remove a task's due date (edit only).
"schedule" is START + DURATION with spaces around the plus: "fri 14:00 + 2h", "tmr 09:30 + 45min", "14:00 + 1h30min" (a clock alone means today here), "in 2h + 30min".
"reminders" entries are "at" (at the due or start time) or an offset before it: "-10min", "-1h", "-1h30min". A reminder needs a due date or a schedule.

` + aiDateWords

const aiFindSystem = `You turn a search request to a to-do list app called tt into a filter over the user's cached tasks. Reply with one JSON object matching the schema.

- "keywords": words tt looks for as case-insensitive substrings of task titles, notes, tags and list names; a task matches when any keyword matches. Include synonyms and, for Russian and other inflected languages, every stem the word and its derived words use (for "молоко": "молок" and "молоч", which also finds "молочные продукты"). Use 1 to 10 short keywords. Use an empty list only when the request is only about dates, a list or status.
- "project": a list name from the context, written exactly as there, or null for all lists.
- "status": "open", "done" or "all"; null means open tasks, or done tasks when a completion range is given.
- "due_from", "due_to", "done_from", "done_to": inclusive bounds in tt's date words below, or null. Never compute calendar dates yourself.

` + aiDateWords

const aiRerankSystem = `You rank search results for a to-do list app called tt. The search request and a numbered list of task titles follow. Reply with one JSON object matching the schema: "ranked" lists the numbers of the titles that match the request, most relevant first. Leave out titles that clearly do not match. Use only numbers from the list, each at most once.`
