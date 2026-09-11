package api

import "encoding/json"

func (value *Project) UnmarshalJSON(data []byte) error {
	type wire Project
	var out wire
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	out.Raw = append(json.RawMessage(nil), data...)
	*value = Project(out)
	return nil
}

func (value *Column) UnmarshalJSON(data []byte) error {
	type wire Column
	var out wire
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	out.Raw = append(json.RawMessage(nil), data...)
	*value = Column(out)
	return nil
}
