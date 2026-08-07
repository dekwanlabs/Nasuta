// Package investigationhttp exposes the authenticated delegated investigation API.
package investigationhttp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/investigation"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const maxQuestionLength = 8000

type Service interface {
	Run(context.Context, string, agentapi.Actor) (investigation.Result, error)
}

type Handler struct {
	service Service
}

func New(service Service) *Handler {
	return &Handler{service: service}
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/investigations", handler.Create)
}

func (handler *Handler) Create(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		httputil.WriteUnauthorized(w, "authentication required")
		return
	}
	var request struct {
		Question string `json:"question"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request.Question = strings.TrimSpace(request.Question)
	if request.Question == "" {
		httputil.WriteBadRequest(w, "question is required")
		return
	}
	if utf8.RuneCountInString(request.Question) > maxQuestionLength {
		httputil.WriteBadRequest(w, "question exceeds 8000 characters")
		return
	}
	if handler.service == nil {
		httputil.WriteServiceUnavailable(w, investigation.ErrUnavailable.Error())
		return
	}
	result, err := handler.service.Run(
		r.Context(), request.Question, agentapi.Actor{UserID: user.ID},
	)
	if err != nil {
		if errors.Is(err, investigation.ErrUnavailable) {
			httputil.WriteServiceUnavailable(w, err.Error())
			return
		}
		httputil.WriteErrStatus(w, http.StatusBadGateway, err)
		return
	}
	httputil.WriteJSON(w, result)
}
