package common

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

// ParseDateTime reads an RFC 3339 date-time outside a request body — a query
// parameter — exactly as a body's date-time member is read, so one value has
// one reading at every door: fractional seconds after "." only, two-digit
// hours, the offset bounded. time.Parse takes a comma before the fractional
// seconds, a one-digit hour and an offset of +24:00.
func ParseDateTime(s string) (time.Time, error) {
	var t time.Time
	quoted, err := jsontext.AppendQuote(nil, s)
	if err != nil {
		return time.Time{}, err
	}
	err = json.Unmarshal(quoted, &t)
	return t, err
}
