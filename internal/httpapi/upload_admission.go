package httpapi

import (
	"net/http"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func (api *identityAPI) admitUploads(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Defaults *uploadRequest  `json:"defaults,omitempty"`
		Uploads  []uploadRequest `json:"uploads"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.Uploads) < 1 || len(request.Uploads) > domain.MaxUploadLifecycleItems {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "admission page must contain 1 to 100 items"))
		return
	}
	items := make([]domain.CreateUploadRequest, len(request.Uploads))
	for i, item := range request.Uploads {
		if request.Defaults != nil {
			item = withUploadDefaults(item, *request.Defaults)
		}
		// Invalid path metadata is an item rejection; authentication, JSON structure,
		// page bounds and unique stable item keys remain whole-envelope boundaries.
		path, err := uploadPath(item)
		if err != nil {
			path = domain.UserPath{}
		}
		items[i] = domain.CreateUploadRequest{Path: path, Size: item.Size, MediaType: item.MediaType, Conflict: item.Conflict, ExpectedVersion: item.ExpectedVersion, Resumable: item.Resumable, IdempotencyKey: item.IdempotencyKey}
	}
	results, err := api.drive.AdmitUploads(r.Context(), current.Record.UserID, items)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	for i := range results {
		if results[i].ErrorKind != "" {
			results[i].RequestID = requestID(r)
		}
	}
	writeUploadLifecyclePage(w, r, results)
}
