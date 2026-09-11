package model

import "strings"

func ValidProjectID(id string) bool {
	return id != "" && !strings.ContainsFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	})
}

func (p Project) CreateUnavailable() string {
	if !ValidProjectID(p.Id) {
		return "list ID is missing or unsupported"
	}
	if p.Closed {
		return "list is closed"
	}
	switch strings.ToUpper(p.Kind) {
	case "", "TASK", "NOTE":
		return ""
	default:
		return "list kind is unsupported"
	}
}
