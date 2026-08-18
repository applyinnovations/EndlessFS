package imagegen

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/preview"
)

func TestWorkerProtocolValidationAndFailureBoundaries(t *testing.T) {
	options := normalizedOptions(Options{})
	source := preview.OnePixelWebP()
	request := workerRequest{Version: workerVersion, SourceSize: int64(len(source)), MediaType: "image/webp", Variant: 64, Options: options}
	input := encodeWorkerTestRequest(t, request, source)
	var output bytes.Buffer
	if err := RunWorker(bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	response, artifact, err := decodeWorkerResponse(output.Bytes())
	if err != nil || response.Width != 1 || response.Height != 1 || len(artifact) < 12 {
		t.Fatalf("worker protocol response = %+v, %d bytes, %v", response, len(artifact), err)
	}

	worker, err := NewWorker(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if worker.Capability() != "image" || worker.RecipeID() != recipeID || !worker.Supports("image/png") || worker.Supports("text/plain") {
		t.Fatal("worker capability registry changed")
	}
	inProcess := New(Options{})
	if inProcess.Capability() != "image" || inProcess.RecipeID() != recipeID {
		t.Fatal("in-process capability registry changed")
	}
	if err := worker.SelfTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.SelfTest(canceled); err == nil {
		t.Fatal("canceled worker self-test succeeded")
	}
	if _, err := worker.Generate(context.Background(), preview.GenerationRequest{}); err == nil {
		t.Fatal("invalid worker generation succeeded")
	}
	if _, err := worker.Generate(context.Background(), preview.GenerationRequest{Source: bytes.NewReader([]byte("x")), SourceSize: 1, MediaType: "image/png", Variant: 64}); err == nil {
		t.Fatal("worker accepted corrupt image source")
	}

	rejected := request
	rejected.SourceSize = 1
	rejected.MediaType = "image/png"
	output.Reset()
	if err := RunWorker(bytes.NewReader(encodeWorkerTestRequest(t, rejected, []byte("x"))), &output); err != nil {
		t.Fatal(err)
	}
	rejection, _, err := decodeWorkerResponse(output.Bytes())
	if err != nil || rejection.Error != "rejected" {
		t.Fatalf("worker rejection = %+v, %v", rejection, err)
	}

	invalidOptions := request
	invalidOptions.Options.MaxPixels = defaultMaxPixels + 1
	for index, invalid := range [][]byte{
		nil,
		{0, 0, 0, 1, '{'},
		encodeWorkerTestRequest(t, invalidOptions, source),
		encodeWorkerTestRequest(t, request, source[:len(source)-1]),
	} {
		if err := RunWorker(bytes.NewReader(invalid), io.Discard); err == nil {
			t.Fatalf("invalid worker request %d succeeded", index)
		}
	}
	unknownHeader := appendWorkerTestHeader(t, []byte(`{"version":1,"unknown":true}`), nil)
	if _, err := readWorkerHeader(bytes.NewReader(unknownHeader)); err == nil {
		t.Fatal("unknown worker header field succeeded")
	}
	trailingHeader := appendWorkerTestHeader(t, []byte(`{"version":1} {}`), nil)
	if _, err := readWorkerHeader(bytes.NewReader(trailingHeader)); err == nil {
		t.Fatal("trailing worker header content succeeded")
	}

	if err := writeWorkerResponse(failingWorkerWriter{}, workerResponse{Version: workerVersion}, nil); err == nil {
		t.Fatal("worker response write failure succeeded")
	}
	for _, failAfter := range []int{1, 2} {
		writer := &countingFailWriter{failAfter: failAfter}
		if err := writeWorkerResponse(writer, workerResponse{Version: workerVersion, Size: 1}, []byte("x")); err == nil {
			t.Fatalf("worker response write failure after %d writes succeeded", failAfter)
		}
	}
	if _, _, err := decodeWorkerResponse([]byte("bad")); err == nil {
		t.Fatal("invalid worker response succeeded")
	}
	for _, invalid := range [][]byte{
		{0, 0, 0, 1, '{'},
		appendWorkerTestHeader(t, []byte(`{"version":1,"unknown":true}`), nil),
		appendWorkerTestHeader(t, []byte(`{"version":1} {}`), nil),
	} {
		if _, err := readWorkerResponseHeader(bytes.NewReader(invalid)); err == nil {
			t.Fatal("invalid worker response header succeeded")
		}
	}
	oversized := &boundedBuffer{maximum: 1}
	if _, err := oversized.Write([]byte("too large")); err == nil || len(oversized.Bytes()) != 0 {
		t.Fatal("worker output bound was not enforced")
	}
}

func encodeWorkerTestRequest(t *testing.T, request workerRequest, source []byte) []byte {
	t.Helper()
	header, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return appendWorkerTestHeader(t, header, source)
}

func appendWorkerTestHeader(t *testing.T, header, source []byte) []byte {
	t.Helper()
	if len(header) > maxWorkerHeader {
		t.Fatal("test worker header too large")
	}
	result := make([]byte, 4, 4+len(header)+len(source))
	binary.BigEndian.PutUint32(result, uint32(len(header))) // #nosec G115 -- test helper rejects headers above the protocol's 4096-byte limit.
	result = append(result, header...)
	return append(result, source...)
}

type failingWorkerWriter struct{}

func (failingWorkerWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type countingFailWriter struct {
	writes    int
	failAfter int
}

func (w *countingFailWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, io.ErrClosedPipe
	}
	return len(data), nil
}

func TestWorkerCancellationTerminatesBlockedCodecProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source := &cancelingWorkerSource{started: make(chan struct{}), ctx: ctx}
	worker := &WorkerGenerator{
		options: normalizedOptions(Options{}), executable: executable,
		environment: []string{workerEnvironment + "=1", "ENDLESSFS_TEST_BLOCK_PREVIEW_WORKER=1"},
	}
	result := make(chan error, 1)
	go func() {
		_, generateErr := worker.Generate(ctx, preview.GenerationRequest{Source: source, SourceSize: 1, MediaType: "image/png", Variant: 64})
		result <- generateErr
	}()
	<-source.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked worker cancellation error = %v", err)
	}
}

type cancelingWorkerSource struct {
	started chan struct{}
	ctx     context.Context
}

func (s *cancelingWorkerSource) Read([]byte) (int, error) {
	select {
	case <-s.started:
	default:
		close(s.started)
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}
