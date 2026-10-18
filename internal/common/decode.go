package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
// is optional. An empty body, or one of JSON whitespace only, is absent and
// leaves dst untouched, whatever Content-Length says; any other body is
// decoded strictly.
func DecodeOptionalBoundedJSON(w http.ResponseWriter, r *http.Request, max int64, dst any) error {
	data, err := readBounded(w, r, max)
	if err != nil {
		return err
	}
	if NoJSONValue(data) {
		return nil
	}
	return strictjson.Decode(data, dst)
}

// NoJSONValue reports whether data holds no JSON value: it is empty or JSON
// whitespace only (space, tab, CR, LF). Any other byte — other Unicode white
// space included — is a body to decode, and is refused if it is not JSON.
// This is the one test for an optional body being absent.
func NoJSONValue(data []byte) bool {
	return len(bytes.Trim(data, " \t\r\n")) == 0
}

func readBounded(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	data, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	case err != nil:
		// The transport's error names socket addresses: logged, not answered.
		slog.Debug("request body could not be read", "pkg", "common", "error", err)
		return nil, errors.New("failed to read request body")
	}
	return data, nil
}

// InvalidBody is the 400 for a request body that does not decode. The
// message carries err's text: strictjson's names the JSON location and never
// a Go type.
func InvalidBody(err error) *AppError {
	return Operational(http.StatusBadRequest, ErrCodeBadRequest, "invalid request body: "+err.Error())
}
