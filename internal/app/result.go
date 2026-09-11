package app

import (
	"encoding/json"
	"time"
)

const ResultSchemaVersion = 1

type ResultMeta struct {
	Source       string    `json:"source"`
	FetchedAt    time.Time `json:"fetched_at,omitzero"`
	Completeness string    `json:"completeness,omitempty"`
	Pending      int       `json:"pending"`
}

type ResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type CommandResult struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	Status        string          `json:"status"`
	Data          any             `json:"data"`
	Meta          ResultMeta      `json:"meta"`
	Warnings      []string        `json:"warnings"`
	Error         *ResultError    `json:"error"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

func Result(command string, data any, meta ResultMeta) CommandResult {
	return CommandResult{
		SchemaVersion: ResultSchemaVersion,
		Command:       command, Status: "ok", Data: data, Meta: meta,
		Warnings: []string{},
	}
}

type Capability struct {
	Name         string `json:"name"`
	Read         bool   `json:"read"`
	Write        bool   `json:"write"`
	Delete       bool   `json:"delete"`
	Scope        string `json:"requested_scopes"`
	AccountGrant string `json:"account_grant"`
	Evidence     string `json:"evidence"`
	Reason       string `json:"reason,omitempty"`
}

func Capabilities() []Capability {
	capabilities := []Capability{
		{Name: "project", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Container deletion is gated until consequences and inventory are verified"},
		{Name: "folder", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Container deletion is gated until consequences and inventory are verified"},
		{Name: "column", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Column deletion and ordering have no verified write contract"},
		{Name: "tag", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Only tag creation is documented"},
		{Name: "habit", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Habit deletion has no verified contract"},
		{Name: "checkin", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "documented"},
		{Name: "comment", Read: true, Write: true, Delete: true, Scope: "tasks:read tasks:write", Evidence: "documented", Reason: "Comment editing has no verified contract"},
		{Name: "focus", Read: true, Write: true, Delete: true, Scope: "tasks:read tasks:write", Evidence: "documented"},
		{Name: "countdown", Read: true, Scope: "tasks:read", Evidence: "documented"},
		{Name: "task.column", Read: true, Write: true, Scope: "tasks:read tasks:write", Evidence: "live_verified", Reason: "Official Open API; undocumented column write verified on disposable objects; independent open tasks, same-project columns, no atomic compare-and-set"},
		{Name: "project.folder", Read: true, Scope: "tasks:read", Evidence: "unverified", Reason: "Folder membership writes require provider contract verification"},
		{Name: "container.cascade", Evidence: "unverified", Reason: "Cascade consequences and complete inventory require provider verification"},
		{Name: "auth.pkce", Evidence: "unverified", Reason: "PKCE support for the tt public client registration is not verified"},
	}
	for i := range capabilities {
		capabilities[i].AccountGrant = "unverified"
	}
	return capabilities
}
