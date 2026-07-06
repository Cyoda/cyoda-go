package api

import (
	"net/http"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// BindingErrorHandler converts oapi-codegen request-parameter binding errors
// into a uniform RFC-9457 ProblemDetail 400 (application/problem+json),
// replacing oapi-codegen's default text/plain response so a malformed path or
// query parameter matches the API's error convention like every other 4xx.
func BindingErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	detail := "invalid request parameter"
	var param string
	switch e := err.(type) {
	case *genapi.InvalidParamFormatError:
		param = e.ParamName
		detail = "invalid value for parameter '" + param + "'"
	case *genapi.RequiredParamError:
		param = e.ParamName
		detail = "missing required parameter '" + param + "'"
	}
	appErr := common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, detail)
	if param != "" {
		appErr.Props = map[string]any{"parameter": param}
	}
	common.WriteError(w, r, appErr)
}
