package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// NullableJSON maps a MySQL JSON column cleanly, treating empty values as SQL NULL.
type NullableJSON json.RawMessage

// GormDataType declares the underlying SQL column type for GORM schema inspection.
func (NullableJSON) GormDataType() string {
	return "json"
}

// Value implements driver.Valuer.
func (j NullableJSON) Value() (driver.Value, error) {
	if len(j) == 0 || string(j) == "null" {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *NullableJSON) Scan(value any) error {
	if value == nil {
		*j = nil
		return nil
	}
	switch v := value.(type) {
	case []byte:
		if len(v) == 0 {
			*j = nil
			return nil
		}
		cp := make([]byte, len(v))
		copy(cp, v)
		*j = NullableJSON(cp)
		return nil
	case string:
		if v == "" {
			*j = nil
			return nil
		}
		*j = NullableJSON([]byte(v))
		return nil
	default:
		return fmt.Errorf("cannot scan %T into NullableJSON", value)
	}
}

// MarshalJSON implements json.Marshaler.
func (j NullableJSON) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (j *NullableJSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return errors.New("NullableJSON: UnmarshalJSON on nil pointer")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	*j = NullableJSON(cp)
	return nil
}
