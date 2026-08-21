package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/drive"
	webassets "github.com/applyinnovations/endlessfs/internal/web"
)

func (api *identityAPI) driveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/files", api.listFiles)
	mux.HandleFunc("GET /api/v1/files/storage-map", api.storageMap)
	mux.HandleFunc("GET /api/v1/files/stat", api.statFile)
	mux.HandleFunc("POST /api/v1/directories", api.createDirectory)
	mux.HandleFunc("POST /api/v1/uploads", api.createUpload)
	mux.HandleFunc("POST /api/v1/uploads/batch", api.createUploadBatch)
	mux.HandleFunc("GET /api/v1/uploads/{uploadID}", api.uploadStatus)
	mux.HandleFunc("POST /api/v1/uploads/{uploadID}/complete", api.completeUpload)
	mux.HandleFunc("DELETE /api/v1/uploads/{uploadID}", api.abortUpload)
	mux.HandleFunc("POST /api/v1/downloads", api.createDownload)
	mux.HandleFunc("POST /api/v1/files/copy", api.copyFile)
	mux.HandleFunc("POST /api/v1/files/move", api.moveFile)
	mux.HandleFunc("POST /api/v1/files/trash", api.trashFiles)
	mux.HandleFunc("GET /api/v1/operations/{operationID}", api.operation)
	mux.HandleFunc("GET /api/v1/trash", api.listTrash)
	mux.HandleFunc("POST /api/v1/trash/{trashID}/restore", api.restoreTrash)
	mux.HandleFunc("DELETE /api/v1/trash/{trashID}", api.deleteTrash)
	mux.HandleFunc("POST /api/v1/trash/empty", api.emptyTrash)
	mux.HandleFunc("GET /api/v1/shares", api.listShares)
	mux.HandleFunc("POST /api/v1/shares", api.createShare)
	mux.HandleFunc("DELETE /api/v1/shares/{shareID}", api.revokeShare)
	mux.HandleFunc("GET /api/v1/public/shares/{token}", api.publicShare)
	mux.HandleFunc("GET /api/v1/public/shares/{token}/stat", api.publicShareStat)
	mux.HandleFunc("POST /api/v1/public/shares/{token}/downloads", api.publicShareDownload)
	mux.HandleFunc("GET /s/{token}", api.publicShareShell)
}

func parsePath(value string) (domain.UserPath, error) { return domain.ParseUserPath(value) }

func parseLimit(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 1000 {
		return 0, domain.NewError(domain.ErrorInvalid, "limit must be between 1 and 1000")
	}
	return limit, nil
}

func (api *identityAPI) listFiles(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	pathValue := r.URL.Query().Get("path")
	if pathValue == "" {
		pathValue = "/"
	}
	path, err := parsePath(pathValue)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	sortField := domain.SortField(r.URL.Query().Get("sort"))
	descending := r.URL.Query().Get("order") == "desc"
	if order := r.URL.Query().Get("order"); order != "" && order != "asc" && order != "desc" {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "invalid sort order"))
		return
	}
	page, err := api.drive.List(r.Context(), current.Record.UserID, domain.ListRequest{Directory: path, PageSize: limit, Cursor: r.URL.Query().Get("cursor"), Sort: sortField, Descending: descending})
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *identityAPI) storageMap(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	pathValue := r.URL.Query().Get("path")
	if pathValue == "" {
		pathValue = "/"
	}
	path, err := parsePath(pathValue)
	if err == nil {
		var page drive.StorageMapPage
		page, err = api.drive.StorageMap(r.Context(), current.Record.UserID, path)
		if err == nil {
			writeJSON(w, http.StatusOK, page)
			return
		}
	}
	writeProblem(w, r, err)
}

func (api *identityAPI) statFile(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	path, err := parsePath(r.URL.Query().Get("path"))
	if err == nil {
		var entry domain.Entry
		entry, err = api.drive.Stat(r.Context(), current.Record.UserID, path)
		if err == nil {
			writeJSON(w, http.StatusOK, entry)
			return
		}
	}
	writeProblem(w, r, err)
}

type pathMutation struct {
	Path            string              `json:"path"`
	Conflict        domain.ConflictMode `json:"conflict,omitempty"`
	ExpectedVersion domain.Version      `json:"expectedVersion,omitempty"`
}

func (api *identityAPI) createDirectory(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request pathMutation
	if !decodeJSON(w, r, &request) {
		return
	}
	path, err := parsePath(request.Path)
	if err == nil {
		var entry domain.Entry
		entry, err = api.drive.CreateDirectory(r.Context(), current.Record.UserID, domain.CreateDirectoryRequest{Path: path, Conflict: request.Conflict, ExpectedVersion: request.ExpectedVersion})
		if err == nil {
			writeJSON(w, http.StatusCreated, entry)
			return
		}
	}
	writeProblem(w, r, err)
}

type uploadRequest struct {
	Path            string              `json:"path"`
	Name            string              `json:"name,omitempty"`
	Size            int64               `json:"size"`
	MediaType       string              `json:"mediaType"`
	Conflict        domain.ConflictMode `json:"conflict,omitempty"`
	ExpectedVersion domain.Version      `json:"expectedVersion,omitempty"`
	Resumable       bool                `json:"resumable,omitempty"`
}

func uploadPath(request uploadRequest) (domain.UserPath, error) {
	base, err := parsePath(request.Path)
	if err != nil {
		return domain.UserPath{}, err
	}
	if request.Name == "" {
		return base, nil
	}
	return base.Join(request.Name)
}

func (api *identityAPI) createUpload(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request uploadRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	path, err := uploadPath(request)
	if err == nil {
		var capability domain.UploadCapability
		capability, err = api.drive.CreateUpload(r.Context(), current.Record.UserID, domain.CreateUploadRequest{Path: path, Size: request.Size, MediaType: request.MediaType, Conflict: request.Conflict, ExpectedVersion: request.ExpectedVersion, Resumable: request.Resumable, IdempotencyKey: r.Header.Get("Idempotency-Key")})
		if err == nil {
			writeJSON(w, http.StatusCreated, capability)
			return
		}
	}
	writeProblem(w, r, err)
}

func (api *identityAPI) createUploadBatch(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Uploads []uploadRequest `json:"uploads"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.Uploads) < 1 || len(request.Uploads) > drive.MaxBatchItems {
		writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "upload batch must contain 1 to 100 items"))
		return
	}
	type result struct {
		Index      int                      `json:"index"`
		Capability *domain.UploadCapability `json:"capability,omitempty"`
		ErrorKind  domain.ErrorKind         `json:"errorKind,omitempty"`
	}
	results := make([]result, 0, len(request.Uploads))
	key := r.Header.Get("Idempotency-Key")
	for index, item := range request.Uploads {
		path, err := uploadPath(item)
		var capability domain.UploadCapability
		if err == nil {
			capability, err = api.drive.CreateUpload(r.Context(), current.Record.UserID, domain.CreateUploadRequest{Path: path, Size: item.Size, MediaType: item.MediaType, Conflict: item.Conflict, ExpectedVersion: item.ExpectedVersion, Resumable: item.Resumable, IdempotencyKey: key + ":" + strconv.Itoa(index)})
		}
		if err != nil {
			results = append(results, result{Index: index, ErrorKind: domain.KindOf(err)})
		} else {
			results = append(results, result{Index: index, Capability: &capability})
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"uploads": results})
}

func (api *identityAPI) uploadStatus(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	status, err := api.drive.UploadStatus(r.Context(), current.Record.UserID, domain.UploadID(r.PathValue("uploadID")))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (api *identityAPI) completeUpload(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Path           string `json:"path"`
		Size           int64  `json:"size"`
		MediaType      string `json:"mediaType"`
		ChecksumSHA256 string `json:"checksumSHA256,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	path, err := parsePath(request.Path)
	if err == nil {
		var entry domain.Entry
		entry, err = api.drive.CompleteUpload(r.Context(), current.Record.UserID, domain.CompleteUploadRequest{UploadID: domain.UploadID(r.PathValue("uploadID")), Path: path, Size: request.Size, MediaType: request.MediaType, ChecksumSHA256: request.ChecksumSHA256})
		if err == nil {
			writeJSON(w, http.StatusOK, entry)
			return
		}
	}
	writeProblem(w, r, err)
}

func (api *identityAPI) abortUpload(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	if !decodeEmptyJSON(w, r) {
		return
	}
	if err := api.drive.AbortUpload(r.Context(), current.Record.UserID, domain.UploadID(r.PathValue("uploadID"))); err != nil {
		writeProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) createDownload(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Path    string         `json:"path"`
		Version domain.Version `json:"version"`
		Preview bool           `json:"preview,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	path, err := parsePath(request.Path)
	if err == nil {
		var capability domain.DownloadCapability
		var previewKind string
		capability, previewKind, err = api.drive.Download(r.Context(), current.Record.UserID, domain.CreateDownloadRequest{Path: path, Version: request.Version}, request.Preview)
		if err == nil {
			writeJSON(w, http.StatusCreated, map[string]any{"capability": capability, "mode": previewKind})
			return
		}
	}
	writeProblem(w, r, err)
}

type copyMoveItem struct {
	Source         string              `json:"source"`
	Destination    string              `json:"destination"`
	Conflict       domain.ConflictMode `json:"conflict,omitempty"`
	ExpectedSource domain.Version      `json:"expectedSource,omitempty"`
	ExpectedTarget domain.Version      `json:"expectedTarget,omitempty"`
}

type copyMoveRequest struct {
	Source         string              `json:"source,omitempty"`
	Destination    string              `json:"destination,omitempty"`
	Conflict       domain.ConflictMode `json:"conflict,omitempty"`
	ExpectedSource domain.Version      `json:"expectedSource,omitempty"`
	ExpectedTarget domain.Version      `json:"expectedTarget,omitempty"`
	Items          []copyMoveItem      `json:"items,omitempty"`
}

func (api *identityAPI) copyFile(w http.ResponseWriter, r *http.Request) { api.copyOrMove(w, r, false) }
func (api *identityAPI) moveFile(w http.ResponseWriter, r *http.Request) { api.copyOrMove(w, r, true) }
func (api *identityAPI) copyOrMove(w http.ResponseWriter, r *http.Request, move bool) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request copyMoveRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.Items) != 0 {
		if request.Source != "" || request.Destination != "" || request.Conflict != "" || request.ExpectedSource != "" || request.ExpectedTarget != "" {
			writeProblem(w, r, domain.NewError(domain.ErrorInvalid, "batch and singular copy/move fields cannot be combined"))
			return
		}
		items := make([]domain.CopyRequest, 0, len(request.Items))
		for _, item := range request.Items {
			source, err := parsePath(item.Source)
			if err != nil {
				writeProblem(w, r, err)
				return
			}
			destination, err := parsePath(item.Destination)
			if err != nil {
				writeProblem(w, r, err)
				return
			}
			items = append(items, domain.CopyRequest{Source: source, Destination: destination, Conflict: item.Conflict, ExpectedSource: item.ExpectedSource, ExpectedTarget: item.ExpectedTarget})
		}
		result, err := api.drive.BatchCopyMove(r.Context(), current.Record.UserID, items, move, r.Header.Get("Idempotency-Key"))
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusAccepted, result)
		return
	}
	source, err := parsePath(request.Source)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	destination, err := parsePath(request.Destination)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	providerRequest := domain.CopyRequest{Source: source, Destination: destination, Conflict: request.Conflict, ExpectedSource: request.ExpectedSource, ExpectedTarget: request.ExpectedTarget, IdempotencyKey: r.Header.Get("Idempotency-Key")}
	var operation domain.Operation
	if move {
		operation, err = api.drive.Move(r.Context(), current.Record.UserID, providerRequest)
	} else {
		operation, err = api.drive.Copy(r.Context(), current.Record.UserID, providerRequest)
	}
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}

func (api *identityAPI) trashFiles(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	paths := make([]domain.UserPath, 0, len(request.Paths))
	for _, value := range request.Paths {
		path, err := parsePath(value)
		if err != nil {
			writeProblem(w, r, err)
			return
		}
		paths = append(paths, path)
	}
	result, err := api.drive.Trash(r.Context(), current.Record.UserID, paths, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (api *identityAPI) operation(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	operation, err := api.drive.Operation(r.Context(), current.Record.UserID, domain.OperationID(r.PathValue("operationID")))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, operation)
}
func (api *identityAPI) listTrash(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	page, err := api.drive.TrashPage(r.Context(), current.Record.UserID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *identityAPI) restoreTrash(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Conflict domain.ConflictMode `json:"conflict,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	operation, err := api.drive.Restore(r.Context(), current.Record.UserID, r.PathValue("trashID"), request.Conflict, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}
func (api *identityAPI) deleteTrash(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	if !decodeEmptyJSON(w, r) {
		return
	}
	operation, err := api.drive.PermanentDelete(r.Context(), current.Record.UserID, r.PathValue("trashID"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, operation)
}
func (api *identityAPI) emptyTrash(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	result, err := api.drive.EmptyTrash(r.Context(), current.Record.UserID, request.Confirm, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (api *identityAPI) listShares(w http.ResponseWriter, r *http.Request) {
	current, ok := api.authenticated(w, r)
	if !ok {
		return
	}
	records, err := api.drive.Shares(r.Context(), current.Record.UserID)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": records})
}
func (api *identityAPI) createShare(w http.ResponseWriter, r *http.Request) {
	current, ok := api.idempotentMutation(w, r)
	if !ok {
		return
	}
	var request struct {
		Path      string     `json:"path"`
		ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	path, err := parsePath(request.Path)
	if err == nil {
		var created drive.CreatedShare
		created, err = api.drive.CreateShare(r.Context(), current.Record.UserID, path, request.ExpiresAt, r.Header.Get("Idempotency-Key"))
		if err == nil {
			writeJSON(w, http.StatusCreated, map[string]any{"share": created.Record, "link": created.Link.Reveal()})
			return
		}
	}
	writeProblem(w, r, err)
}
func (api *identityAPI) revokeShare(w http.ResponseWriter, r *http.Request) {
	current, ok := api.mutation(w, r)
	if !ok {
		return
	}
	if !decodeEmptyJSON(w, r) {
		return
	}
	if err := api.drive.RevokeShare(r.Context(), current.Record.UserID, r.PathValue("shareID")); err != nil {
		writeProblem(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (api *identityAPI) publicShare(w http.ResponseWriter, r *http.Request) {
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	page, err := api.drive.PublicShare(r.Context(), r.PathValue("token"), r.URL.Query().Get("path"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (api *identityAPI) publicShareStat(w http.ResponseWriter, r *http.Request) {
	entry, err := api.drive.PublicStat(r.Context(), r.PathValue("token"), r.URL.Query().Get("path"))
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (api *identityAPI) publicShareDownload(w http.ResponseWriter, r *http.Request) {
	if !api.requireOrigin(w, r) {
		return
	}
	var request struct {
		Path    string         `json:"path"`
		Version domain.Version `json:"version"`
		Preview bool           `json:"preview,omitempty"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	capability, mode, err := api.drive.PublicDownload(r.Context(), r.PathValue("token"), request.Path, request.Version, request.Preview)
	if err != nil {
		writeProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"capability": capability, "mode": mode})
}

func (api *identityAPI) publicShareShell(w http.ResponseWriter, r *http.Request) {
	if strings.ContainsAny(r.PathValue("token"), "\r\n\x00") {
		http.NotFound(w, r)
		return
	}
	clone := r.Clone(r.Context())
	clone.URL.Path = "/"
	webassets.Handler().ServeHTTP(w, clone)
}
