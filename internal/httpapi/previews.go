package httpapi

import (
	"net/http"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/preview"
)

func (api *identityAPI) previewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/previews/resolve", api.resolvePreviews)
	mux.HandleFunc("POST /api/v1/previews/generations", api.generatePreview)
	mux.HandleFunc("GET /api/v1/previews/operations/{operationID}", api.previewOperation)
}

func (api *identityAPI) resolvePreviews(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Items []struct {
			Path    string         `json:"path"`
			Version domain.Version `json:"version"`
			Variant int            `json:"variant"`
		} `json:"items"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	resolved := preview.ResolveRequest{Items: make([]preview.ItemRequest, 0, len(request.Items))}
	for _, item := range request.Items {
		path, err := parsePath(item.Path)
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		resolved.Items = append(resolved.Items, preview.ItemRequest{Path: path, Version: item.Version, Variant: item.Variant})
	}
	response, err := api.previews.Resolve(r.Context(), current.Record.UserID, resolved)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	for _, item := range response.Items {
		if item.State == preview.StateUnavailable {
			api.logPreviewUnavailable(r, "resolve")
			break
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (api *identityAPI) generatePreview(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Path    string         `json:"path"`
		Version domain.Version `json:"version"`
		Variant int            `json:"variant"`
		Action  string         `json:"action"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Action != "generate" && request.Action != "regenerate" {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "preview action must be generate or regenerate"))
		return
	}
	path, err := parsePath(request.Path)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	operation, err := api.previews.Generate(r.Context(), current.Record.UserID, preview.GenerateRequest{
		Path: path, Version: request.Version, Variant: request.Variant, Regenerate: request.Action == "regenerate", IdempotencyKey: r.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	if operation.ErrorKind == domain.ErrorUnavailable || operation.Result != nil && operation.Result.State == preview.StateUnavailable {
		api.logPreviewUnavailable(r, "generation")
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (api *identityAPI) logPreviewUnavailable(r *http.Request, operation string) {
	if api.logger != nil {
		api.logger.ErrorContext(r.Context(), "preview_unavailable", "operation", operation, "category", "preview_store", "result", "error")
	}
}

func (api *identityAPI) previewOperation(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	operation, err := api.previews.Operation(r.Context(), current.Record.UserID, domain.OperationID(r.PathValue("operationID")))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}
