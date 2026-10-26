package util

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// status returns the status ServerError replies with for err, and the body.
func status(err error) (int, string) {
	w := httptest.NewRecorder()
	ServerError(w, err)
	return w.Code, w.Body.String()
}

func TestServerErrorBadRequestShowsMessage(t *testing.T) {
	code, body := status(fmt.Errorf("%w: note is empty", ErrBadRequest))
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "note is empty")
}

func TestServerErrorForbidden(t *testing.T) {
	code, _ := status(fmt.Errorf("%w: not yours", ErrForbidden))
	assert.Equal(t, http.StatusForbidden, code)
}

func TestServerErrorNotFoundHidesDetails(t *testing.T) {
	code, body := status(fmt.Errorf("get row 42: %w", ErrNotFound))
	assert.Equal(t, http.StatusNotFound, code)
	assert.NotContains(t, body, "row 42")
}

func TestServerErrorBadGateway(t *testing.T) {
	code, _ := status(fmt.Errorf("%w: remote said no", ErrBadGateway))
	assert.Equal(t, http.StatusBadGateway, code)
}

func TestServerErrorOtherHidesDetails(t *testing.T) {
	code, body := status(errors.New("password=hunter2"))
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.NotContains(t, body, "hunter2")
}
