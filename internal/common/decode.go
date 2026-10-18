package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cyoda-platform/cyoda-go-spi/strictjson"
)

// DecodeBoundedJSON reads at most max bytes of the request body and decodes
// them with strictjson.Decode: member names exact, duplicates and unknown
// members refused. Returns a non-nil error on an oversize body or a body
// strictjson refuses; callers translate that to 400 BAD_REQUEST via
// WriteError.
func DecodeBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	data, err := readBounded(w, r, max)
	if err != nil {
		return err
	}
	return strictjson.Decode(data, dst)
}

// DecodeOptionalBoundedJSON is DecodeBoundedJSON for an operation whose body
// is optional. An empty or whitespace-only body is absent (present false, dst
// untouched), whatever Content-Length says; any other body is decoded
// strictly.
func DecodeOptionalBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) (present bool, err error) {
	data, err := readBounded(w, r, max)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return false, nil
	}
	return true, strictjson.Decode(data, dst)
}

func readBounded(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	data, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	case err != nil:
		return nil, fmt.Errorf("read body: %w", err)
	}
	return data, nil
}

// InvalidBody is the 400 for a request body that does not decode. The
// message carries err's text: strictjson's names the JSON location and never
// a Go type.
func InvalidBody(err error) *AppError {
	return Operational(http.StatusBadRequest, ErrCodeBadRequest, "invalid request body: "+err.Error())
}
