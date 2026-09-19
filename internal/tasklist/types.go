package tasklist

// Limits of one batch document.
const (
	MaxTasks        = 200
	MaxItemsPerTask = 200
)

// Item is one checklist entry of a task.
type Item struct {
	Title string
	Done  bool
	Line  int
}

// Task is one task parsed from a batch document.
type Task struct {
	Title    string
	Done     bool
	Items    []Item
	Children []Task
	Line     int
}

// Group collects the tasks written under one list heading.
type Group struct {
	List  string
	Tasks []Task
	Line  int
}

// Document is a parsed batch document.
type Document struct {
	Groups []Group
}

// ParseError reports a problem on one line of the source.
type ParseError struct {
	Line int
	Msg  string
}
