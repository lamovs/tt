package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/movsar/tt/internal/config"
)

// claudeEnvelope is the shape of `claude -p --output-format json
// --json-schema ...`: a single JSON object carrying telemetry alongside the
// reply. structured_output holds the schema-validated value; result holds
// either the same value marshaled as a string, or, on failure, a
// human-readable message.
type claudeEnvelope struct {
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	Structured json.RawMessage `json:"structured_output"`
}

// parseClaudeOutput extracts the reply from a claude envelope. On is_error
// the error carries claude's own message instead (see claudeErrorMessage).
func parseClaudeOutput(stdout []byte, sent, private string) ([]byte, error) {
	var env claudeEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &env); err != nil {
		return extractJSON(stdout)
	}
	if env.IsError {
		msg, _ := claudeErrorMessage(stdout, sent, private)
		return nil, errors.New(msg)
	}
	if len(env.Structured) > 0 && !bytes.Equal(bytes.TrimSpace(env.Structured), []byte("null")) {
		return env.Structured, nil
	}
	return extractJSON([]byte(env.Result))
}

// claudeErrorMessage reports whether stdout is a claude envelope with
// is_error set, and returns its result, claude's own message. That message
// can quote the request, so it is redacted (see redactPromptEcho) first.
func claudeErrorMessage(stdout []byte, sent, private string) (string, bool) {
	var env claudeEnvelope
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &env); err != nil || !env.IsError {
		return "", false
	}
	msg := strings.TrimSpace(string(redactPromptEcho([]byte(env.Result), sent, private)))
	if msg == "" {
		msg = "claude reported an error"
	}
	return msg, true
}

// parseOutput extracts the JSON reply from an engine's payload. sent and
// private are the request's texts, for redacting any engine message that
// ends up in the returned error.
func parseOutput(engine config.AIEngine, payload []byte, sent, private string) ([]byte, error) {
	if engine == config.AIEngineClaude {
		return parseClaudeOutput(payload, sent, private)
	}
	return extractJSON(payload)
}

// compactJSON removes insignificant whitespace so a schema fits cleanly
// into one argv value. It returns the input unchanged if it does not parse.
func compactJSON(schema []byte) string {
	if len(schema) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, schema); err != nil {
		return string(schema)
	}
	return buf.String()
}

// extractJSON finds the first complete JSON object or array in raw,
// tolerating surrounding prose and a ```-fenced block.
func extractJSON(raw []byte) ([]byte, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, errors.New("empty output")
	}
	if json.Valid([]byte(s)) {
		return []byte(s), nil
	}

	s = stripCodeFence(s)
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return nil, errors.New("no JSON object or array found in output")
	}
	end, ok := scanJSONValue(s[start:])
	if !ok {
		return nil, errors.New("unterminated JSON value in output")
	}
	candidate := s[start : start+end]
	if !json.Valid([]byte(candidate)) {
		return nil, errors.New("extracted text is not valid JSON")
	}
	return []byte(candidate), nil
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	rest := s[3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	if end := strings.LastIndex(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}

// scanJSONValue returns the length of the balanced JSON object or array
// starting at s[0], respecting string literals and escapes.
func scanJSONValue(s string) (int, bool) {
	if len(s) == 0 {
		return 0, false
	}
	open, close := byte('{'), byte('}')
	if s[0] == '[' {
		open, close = '[', ']'
	} else if s[0] != '{' {
		return 0, false
	}

	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}
