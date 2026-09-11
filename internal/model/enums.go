package model

type Priority int

const (
	PriorityNone   Priority = 0
	PriorityLow    Priority = 1
	PriorityMedium Priority = 3
	PriorityHigh   Priority = 5
)

var priorities = newEnum("priority",
	def(PriorityNone, "none"),
	def(PriorityLow, "low"),
	def(PriorityMedium, "med", "medium"),
	def(PriorityHigh, "high"),
)

func ParsePriority(s string) (Priority, error) { return priorities.parse(s) }

func PriorityFromWire(n int) (Priority, error) { return fromWire(priorities, n) }

func PriorityNames() []string { return priorities.list() }

func (p Priority) String() string                { return priorities.name(p) }
func (p Priority) Wire() int                     { return int(p) }
func (p Priority) MarshalText() ([]byte, error)  { return marshalEnum(priorities, p) }
func (p *Priority) UnmarshalText(b []byte) error { return unmarshalEnum(priorities, p, b) }

type TaskStatus int

const (
	TaskOpen TaskStatus = 0
	TaskDone TaskStatus = 2
)

var taskStatuses = newEnum("task status",
	def(TaskOpen, "open"),
	def(TaskDone, "done"),
)

func ParseTaskStatus(s string) (TaskStatus, error) { return taskStatuses.parse(s) }
func TaskStatusFromWire(n int) (TaskStatus, error) { return fromWire(taskStatuses, n) }
func TaskStatusNames() []string                    { return taskStatuses.list() }
func (s TaskStatus) String() string                { return taskStatuses.name(s) }
func (s TaskStatus) Wire() int                     { return int(s) }
func (s TaskStatus) Done() bool                    { return s == TaskDone }
func (s TaskStatus) MarshalText() ([]byte, error)  { return marshalEnum(taskStatuses, s) }
func (s *TaskStatus) UnmarshalText(b []byte) error { return unmarshalEnum(taskStatuses, s, b) }

type ItemStatus int

const (
	ItemOpen ItemStatus = 0
	ItemDone ItemStatus = 1
)

var itemStatuses = newEnum("item status",
	def(ItemOpen, "open"),
	def(ItemDone, "done"),
)

func ParseItemStatus(s string) (ItemStatus, error) { return itemStatuses.parse(s) }
func ItemStatusFromWire(n int) (ItemStatus, error) { return fromWire(itemStatuses, n) }
func ItemStatusNames() []string                    { return itemStatuses.list() }
func (s ItemStatus) String() string                { return itemStatuses.name(s) }
func (s ItemStatus) Wire() int                     { return int(s) }
func (s ItemStatus) Done() bool                    { return s == ItemDone }
func (s ItemStatus) MarshalText() ([]byte, error)  { return marshalEnum(itemStatuses, s) }
func (s *ItemStatus) UnmarshalText(b []byte) error { return unmarshalEnum(itemStatuses, s, b) }

type EditorHints int

const (
	HintsFull EditorHints = iota
	HintsShort
	HintsNone
)

var editorHints = newEnum("editor hints",
	def(HintsFull, "full"),
	def(HintsShort, "short"),
	def(HintsNone, "none"),
)

func ParseEditorHints(s string) (EditorHints, error) { return editorHints.parse(s) }
func EditorHintsNames() []string                     { return editorHints.list() }
func (h EditorHints) String() string                 { return editorHints.name(h) }
func (h EditorHints) MarshalText() ([]byte, error)   { return marshalEnum(editorHints, h) }
func (h *EditorHints) UnmarshalText(b []byte) error  { return unmarshalEnum(editorHints, h, b) }

type SessionKind int

const (
	SessionFocus SessionKind = iota
	SessionShortBreak
	SessionLongBreak
)

var sessionKinds = newEnum("session kind",
	def(SessionFocus, "focus"),
	def(SessionShortBreak, "short_break"),
	def(SessionLongBreak, "long_break"),
)

func ParseSessionKind(s string) (SessionKind, error) { return sessionKinds.parse(s) }
func SessionKindNames() []string                     { return sessionKinds.list() }
func (k SessionKind) String() string                 { return sessionKinds.name(k) }
func (k SessionKind) IsBreak() bool                  { return k != SessionFocus }
func (k SessionKind) MarshalText() ([]byte, error)   { return marshalEnum(sessionKinds, k) }
func (k *SessionKind) UnmarshalText(b []byte) error  { return unmarshalEnum(sessionKinds, k, b) }

type SessionOutcome int

const (
	OutcomeUnset SessionOutcome = iota
	OutcomeDone
	OutcomeAborted
)

var sessionOutcomes = newEnum("session outcome",
	def(OutcomeDone, "done"),
	def(OutcomeAborted, "aborted"),
)

func ParseSessionOutcome(s string) (SessionOutcome, error) { return sessionOutcomes.parse(s) }
func SessionOutcomeNames() []string                        { return sessionOutcomes.list() }
func (o SessionOutcome) IsSet() bool                       { return o != OutcomeUnset }

func (o SessionOutcome) String() string {
	if !o.IsSet() {
		return ""
	}
	return sessionOutcomes.name(o)
}

func (o SessionOutcome) MarshalText() ([]byte, error) {
	if !o.IsSet() {
		return nil, nil
	}
	return marshalEnum(sessionOutcomes, o)
}

func (o *SessionOutcome) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*o = OutcomeUnset
		return nil
	}
	return unmarshalEnum(sessionOutcomes, o, b)
}
