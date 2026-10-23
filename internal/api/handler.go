package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/vj/docx/internal/broker"
	"github.com/vj/docx/internal/store"
	"github.com/vj/docx/internal/validator"
)

func (app *Application) healthcheckHandler(w http.ResponseWriter, r *http.Request) {
	env := envelope{
		"status":      "available",
		"environment": app.Config.Env,
		"version":     "1.0.0",
	}
	err := app.writeJSON(w, http.StatusOK, env, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

func (app *Application) createDocumentHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title         string `json:"title"`
		SourceContent string `json:"source_content"`
	}

	err := app.readJSON(w, r, &input)
	if err != nil {
		app.badRequestResponse(w, r, err)
		return
	}
	v := validator.New()
	v.Check(validator.NotBlank(input.Title), "title", "must be provided")
	v.Check(validator.MaxChars(input.Title, 255), "title", "must not exceed 255 characters")
	v.Check(validator.NotBlank(input.SourceContent), "source_content", "must be provided")

	if !v.Valid() {
		app.failedValidationResponse(w, r, v.Errors)
		return
	}
	doc := &store.Document{
		Title:         input.Title,
		SourceContent: input.SourceContent,
		Status:        store.StatusPending,
	}

	err = app.Store.Insert(r.Context(), doc)
	if err != nil {
		app.serverErrorResponse(w, r, err)
		return
	}

	event := broker.DocumentSubmittedEvent{
		DocumentID:    doc.ID,
		SourceContent: doc.SourceContent,
		SubmittedAt:   doc.CreatedAt,
	}
	err = app.Publisher.PublishDocumentSubmitted(r.Context(), event)
	if err != nil {
		_ = app.Store.UpdateStatus(r.Context(), doc.ID, store.StatusFailed, nil, 0)

		app.serverErrorResponse(w, r, errors.New("failed to enqueue document for processing"))
		return
	}

	err = app.writeJSON(w, http.StatusAccepted, envelope{"document": doc}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}

func (app *Application) getDocumentHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		app.notFoundResponse(w, r)
		return
	}

	doc, err := app.Store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrRecordNotFound) {
			app.notFoundResponse(w, r)
			return
		}
		app.serverErrorResponse(w, r, err)
		return
	}

	err = app.writeJSON(w, http.StatusOK, envelope{"document": doc}, nil)
	if err != nil {
		app.serverErrorResponse(w, r, err)
	}
}
