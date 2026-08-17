package gcs_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeObject struct {
	body           []byte
	generation     int64
	metageneration int64
}

type fakeGCS struct {
	t                       *testing.T
	mu                      sync.Mutex
	objects                 map[string]fakeObject
	nextGeneration          int64
	nextStatus              int
	failUploadAfterCommit   bool
	uploadRequests          int
	corruptNextDownloadCRC  bool
	wrongNextMetadataSizeBy int
}

func newGCSServer(t *testing.T) *httptest.Server {
	server, _ := newGCSServerWithFake(t)
	return server
}

func newGCSServerWithFake(t *testing.T) (*httptest.Server, *fakeGCS) {
	t.Helper()
	fake := &fakeGCS{t: t, objects: make(map[string]fakeObject), nextGeneration: 100}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return server, fake
}

func (f *fakeGCS) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	status := f.nextStatus
	f.nextStatus = 0
	f.mu.Unlock()
	if status != 0 {
		f.problem(writer, status, "injected")
		return
	}
	if request.URL.Path == "/storage/v1/b/endlessfs-test/o" && request.Method == http.MethodGet {
		f.list(writer, request)
		return
	}
	if request.URL.Path == "/upload/storage/v1/b/endlessfs-test/o" && request.Method == http.MethodPost {
		f.upload(writer, request)
		return
	}
	prefix := "/storage/v1/b/endlessfs-test/o/"
	if strings.HasPrefix(request.URL.Path, prefix) {
		tail := strings.TrimPrefix(request.URL.Path, prefix)
		if request.Method == http.MethodPost && strings.Contains(tail, "/rewriteTo/b/") {
			f.rewrite(writer, request, tail)
			return
		}
		name, err := url.PathUnescape(tail)
		if err != nil {
			f.problem(writer, http.StatusBadRequest, "invalid")
			return
		}
		switch request.Method {
		case http.MethodGet:
			f.get(writer, request, name)
		case http.MethodDelete:
			f.delete(writer, request, name)
		default:
			f.problem(writer, http.StatusNotFound, "notFound")
		}
		return
	}
	f.problem(writer, http.StatusNotFound, "notFound")
}

func (f *fakeGCS) upload(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.uploadRequests++
	f.mu.Unlock()
	if request.URL.Query().Get("uploadType") != "multipart" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	name := request.URL.Query().Get("name")
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/related" || parameters["boundary"] == "" {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	metadataPart, err := reader.NextPart()
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	var metadata struct {
		Name   string `json:"name"`
		CRC32C string `json:"crc32c"`
	}
	if err := json.NewDecoder(metadataPart).Decode(&metadata); err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	bodyPart, err := reader.NextPart()
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	body, err := io.ReadAll(bodyPart)
	if err != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	if metadata.Name != "" && metadata.Name != name || metadata.CRC32C != "" && metadata.CRC32C != crc32c(body) {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, exists := f.objects[name]
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), current.generation, exists) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextGeneration++
	created := fakeObject{body: append([]byte(nil), body...), generation: f.nextGeneration, metageneration: 1}
	f.objects[name] = created
	if f.failUploadAfterCommit {
		f.failUploadAfterCommit = false
		f.problem(writer, http.StatusServiceUnavailable, "backendError")
		return
	}
	f.writeObject(writer, name, created)
}

func (f *fakeGCS) get(writer http.ResponseWriter, request *http.Request, name string) {
	f.mu.Lock()
	object, exists := f.objects[name]
	f.mu.Unlock()
	if !exists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if generation := request.URL.Query().Get("generation"); generation != "" && generation != strconv.FormatInt(object.generation, 10) {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if match := request.URL.Query().Get("ifGenerationMatch"); match != "" && match != strconv.FormatInt(object.generation, 10) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	if request.URL.Query().Get("alt") == "media" {
		f.mu.Lock()
		corruptCRC := f.corruptNextDownloadCRC
		f.corruptNextDownloadCRC = false
		f.mu.Unlock()
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		writer.Header().Set("X-Goog-Generation", strconv.FormatInt(object.generation, 10))
		writer.Header().Set("X-Goog-Metageneration", strconv.FormatInt(object.metageneration, 10))
		checksum := crc32c(object.body)
		if corruptCRC {
			checksum = "AAAAAA=="
		}
		writer.Header().Set("X-Goog-Hash", "crc32c="+checksum)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(object.body)
		return
	}
	f.mu.Lock()
	sizeDelta := f.wrongNextMetadataSizeBy
	f.wrongNextMetadataSizeBy = 0
	f.mu.Unlock()
	if sizeDelta != 0 {
		value := objectJSON(name, object)
		value["size"] = strconv.Itoa(len(object.body) + sizeDelta)
		f.writeJSON(writer, http.StatusOK, value)
		return
	}
	f.writeObject(writer, name, object)
}

func (f *fakeGCS) delete(writer http.ResponseWriter, request *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, exists := f.objects[name]
	if !exists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), object.generation, true) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	delete(f.objects, name)
	writer.WriteHeader(http.StatusNoContent)
}

func (f *fakeGCS) list(writer http.ResponseWriter, request *http.Request) {
	prefix := request.URL.Query().Get("prefix")
	limit := 1000
	if raw := request.URL.Query().Get("maxResults"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			f.problem(writer, http.StatusBadRequest, "invalid")
			return
		}
		limit = parsed
	}
	startAfter := ""
	if token := request.URL.Query().Get("pageToken"); token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			f.problem(writer, http.StatusBadRequest, "invalidPageToken")
			return
		}
		startAfter = string(decoded)
	}
	f.mu.Lock()
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) && key > startAfter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	items := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		items = append(items, objectJSON(key, f.objects[key]))
	}
	more := false
	if len(keys) > 0 {
		last := keys[len(keys)-1]
		for key := range f.objects {
			if strings.HasPrefix(key, prefix) && key > last {
				more = true
				break
			}
		}
	}
	f.mu.Unlock()
	response := map[string]any{"kind": "storage#objects", "items": items}
	if more {
		response["nextPageToken"] = base64.RawURLEncoding.EncodeToString([]byte(keys[len(keys)-1]))
	}
	f.writeJSON(writer, http.StatusOK, response)
}

func (f *fakeGCS) rewrite(writer http.ResponseWriter, request *http.Request, tail string) {
	parts := strings.SplitN(tail, "/rewriteTo/b/endlessfs-test/o/", 2)
	if len(parts) != 2 {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	source, sourceErr := url.PathUnescape(parts[0])
	destination, destinationErr := url.PathUnescape(parts[1])
	if sourceErr != nil || destinationErr != nil {
		f.problem(writer, http.StatusBadRequest, "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sourceObject, sourceExists := f.objects[source]
	if !sourceExists {
		f.problem(writer, http.StatusNotFound, "notFound")
		return
	}
	if !generationMatch(request.URL.Query().Get("ifSourceGenerationMatch"), sourceObject.generation, true) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	destinationObject, destinationExists := f.objects[destination]
	if !generationMatch(request.URL.Query().Get("ifGenerationMatch"), destinationObject.generation, destinationExists) {
		f.problem(writer, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	f.nextGeneration++
	created := fakeObject{body: append([]byte(nil), sourceObject.body...), generation: f.nextGeneration, metageneration: 1}
	f.objects[destination] = created
	f.writeJSON(writer, http.StatusOK, map[string]any{
		"kind": "storage#rewriteResponse", "totalBytesRewritten": strconv.Itoa(len(created.body)),
		"objectSize": strconv.Itoa(len(created.body)), "done": true, "resource": objectJSON(destination, created),
	})
}

func generationMatch(raw string, generation int64, exists bool) bool {
	if raw == "" {
		return true
	}
	if raw == "0" {
		return !exists
	}
	return exists && raw == strconv.FormatInt(generation, 10)
}

func objectJSON(name string, object fakeObject) map[string]any {
	return map[string]any{
		"kind": "storage#object", "bucket": "endlessfs-test", "name": name,
		"size": strconv.Itoa(len(object.body)), "generation": strconv.FormatInt(object.generation, 10),
		"metageneration": strconv.FormatInt(object.metageneration, 10), "crc32c": crc32c(object.body),
		"contentType": "application/octet-stream",
	}
}

func crc32c(body []byte) string {
	value := crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	encoded := make([]byte, 4)
	binary.BigEndian.PutUint32(encoded, value)
	return base64.StdEncoding.EncodeToString(encoded)
}

func (f *fakeGCS) writeObject(writer http.ResponseWriter, name string, object fakeObject) {
	f.writeJSON(writer, http.StatusOK, objectJSON(name, object))
}

func (f *fakeGCS) problem(writer http.ResponseWriter, status int, reason string) {
	f.writeJSON(writer, status, map[string]any{"error": map[string]any{
		"code": status, "message": http.StatusText(status),
		"errors": []map[string]string{{"domain": "global", "reason": reason, "message": http.StatusText(status)}},
	}})
}

func (f *fakeGCS) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		f.t.Errorf("encode fake GCS response: %v", err)
	}
}
