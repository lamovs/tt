package tui

import (
	"fmt"

	"github.com/movsar/tt/internal/store"
)

func indicatorChoice(mode store.IndicatorMode, enabled bool) string {
	if mode == store.IndicatorDefault {
		return fmt.Sprintf("config (currently %t; follows timer.indicator)", enabled)
	}
	return mode.String() + " for this session"
}
