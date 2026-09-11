package store

type IndicatorMode int

const (
	IndicatorDefault IndicatorMode = iota
	IndicatorOn
	IndicatorOff
)

func (m IndicatorMode) Valid() bool { return m >= IndicatorDefault && m <= IndicatorOff }

func (m IndicatorMode) Visible(defaultValue bool) bool {
	return m == IndicatorOn || m == IndicatorDefault && defaultValue
}

func (m IndicatorMode) String() string {
	switch m {
	case IndicatorDefault:
		return "config"
	case IndicatorOn:
		return "on"
	case IndicatorOff:
		return "off"
	default:
		return "invalid"
	}
}
