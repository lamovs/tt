package main

import (
	"github.com/movsar/tt/internal/model"
	"github.com/movsar/tt/internal/taskdoc"
)

type editorFields = taskdoc.Fields

func taskEditorFields(task model.Task) editorFields { return taskdoc.FieldsOf(task) }
func editorDocument(task model.Task, hints model.EditorHints) ([]byte, error) {
	return taskdoc.Encode(task, hints)
}
func parseEditorDocument(data []byte) (editorFields, string, error) { return taskdoc.Decode(data) }
func editedTaskDocument(original model.Task, data []byte, creating bool) (model.Task, model.TaskEdit, error) {
	return taskdoc.Apply(original, data, creating)
}
