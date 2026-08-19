package imagegen

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRawSelfTestFixtureRejectsOverflow(t *testing.T) {
	tests := []struct {
		name string
		run  func()
	}{
		{name: "uint32 negative", run: func() { rawSelfTestUint32(-1) }},
		{name: "uint32 overflow", run: func() { rawSelfTestUint32(1 << 32) }},
		{name: "uint16 negative", run: func() { rawSelfTestUint16(-1) }},
		{name: "uint16 overflow", run: func() { rawSelfTestUint16(1 << 16) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				test.run()
			}()
			if !panicked {
				t.Fatal("RAW self-test fixture accepted an overflowing integer")
			}
		})
	}
}

func TestRawDecoderBoundaryRejectsUnpackagedPathsAndMalformedOutput(t *testing.T) {
	decoderPath := os.Getenv("ENDLESSFS_TEST_RAW_DECODER")
	if decoderPath == "" {
		t.Fatal("ENDLESSFS_TEST_RAW_DECODER is required by the Nix test environment")
	}
	fixture := rawSelfTestDNG()
	generator := New(Options{RawDecoderPath: decoderPath})
	generated, err := generator.Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(fixture), SourceSize: int64(len(fixture)), MediaType: "image/x-adobe-dng", Variant: 64,
	})
	if err != nil || len(generated.Bytes) < 12 || generated.Width < 1 || generated.Height < 1 || generated.Width > 64 || generated.Height > 64 {
		t.Fatalf("in-process RAW generation = %+v, %v", generated, err)
	}
	if _, err := generator.Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(preview.OnePixelWebP()), SourceSize: int64(len(preview.OnePixelWebP())), MediaType: "image/x-sony-arw", Variant: 64,
	}); err == nil {
		t.Fatal("in-process RAW generator accepted WebP bytes")
	}
	missingDecoderGenerator := New(Options{RawDecoderPath: filepath.Join(t.TempDir(), "missing-raw-decoder")})
	if _, err := missingDecoderGenerator.Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(fixture), SourceSize: int64(len(fixture)), MediaType: "image/x-adobe-dng", Variant: 64,
	}); err == nil {
		t.Fatal("missing RAW decoder was accepted during generation")
	}
	t.Setenv(rawDecoderEnvironment, decoderPath)
	if packaged, err := PackagedRawDecoderPath(); err != nil || packaged != decoderPath {
		t.Fatalf("packaged RAW decoder = %q, %v", packaged, err)
	}
	t.Setenv(rawDecoderEnvironment, "")
	if _, err := PackagedRawDecoderPath(); err == nil {
		t.Fatal("missing packaged RAW decoder was accepted")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invalidDecoder := filepath.Join(t.TempDir(), "endlessfs-invalid-raw-decoder")
	if err := os.Symlink(executable, invalidDecoder); err != nil {
		t.Fatal(err)
	}
	invalidGenerator := New(Options{RawDecoderPath: invalidDecoder})
	if _, err := invalidGenerator.Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(fixture), SourceSize: int64(len(fixture)), MediaType: "image/x-adobe-dng", Variant: 64,
	}); err == nil {
		t.Fatal("invalid RAW decoder output was accepted")
	}
	invalidWorker, err := NewWorker(Options{RawDecoderPath: invalidDecoder})
	if err != nil {
		t.Fatal(err)
	}
	if err := invalidWorker.SelfTest(context.Background()); err == nil {
		t.Fatal("invalid RAW decoder passed the worker self-test")
	}
	blockingDecoder := filepath.Join(t.TempDir(), "endlessfs-blocking-raw-decoder")
	if err := os.Symlink(executable, blockingDecoder); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := New(Options{RawDecoderPath: blockingDecoder}).Generate(deadline, preview.GenerationRequest{
		Source: bytes.NewReader(fixture), SourceSize: int64(len(fixture)), MediaType: "image/x-adobe-dng", Variant: 64,
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked RAW decoder cancellation = %v", err)
	}
	if _, err := NewWorker(Options{RawDecoderPath: "relative-decoder"}); err == nil {
		t.Fatal("relative RAW decoder path was accepted")
	}
	if _, err := NewWorker(Options{RawDecoderPath: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing RAW decoder path was accepted")
	}
	valid := []byte("P6\n2 1\n255\n\x01\x02\x03\x04\x05\x06")
	decoded, err := decodePPM(valid, 8, 64)
	if err != nil || decoded.Bounds().Dx() != 2 || decoded.Bounds().Dy() != 1 {
		t.Fatalf("valid PPM decode = %v, %v", decoded, err)
	}
	for index, value := range [][]byte{
		nil,
		[]byte("P3\n1 1\n255\n\x00\x00\x00"),
		[]byte("P6\n9 1\n255\n" + string(make([]byte, 27))),
		[]byte("P6\n1 1\n65535\n\x00\x00\x00"),
		append(append([]byte(nil), valid...), 0),
		[]byte("P6\n" + strings.Repeat("9", 33) + " 1\n255\n"),
		[]byte("P6\nnot-a-number 1\n255\n"),
		[]byte("P6\n1 1\n255\n\x00\x00"),
		[]byte("P6\n1 \n"),
		[]byte("P6\n# unterminated comment"),
	} {
		if _, err := decodePPM(value, 8, 64); err == nil {
			t.Fatalf("invalid PPM output %d was accepted", index)
		}
	}
	commented := []byte("P6\n# deterministic fixture\n 1 1\n255\n\x01\x02\x03")
	if _, err := decodePPM(commented, 8, 64); err != nil {
		t.Fatalf("commented PPM output was rejected: %v", err)
	}
	request := workerRequest{
		Version: workerVersion, SourceSize: 1, MediaType: "image/x-sony-arw", Variant: 64,
		Options: normalizedOptions(Options{RawDecoderPath: filepath.Join(t.TempDir(), "must-not-be-serialized")}),
	}
	header, err := json.Marshal(request)
	if err != nil || bytes.Contains(header, []byte("must-not-be-serialized")) || bytes.Contains(header, []byte("rawDecoderPath")) {
		t.Fatalf("worker header exposed the RAW decoder path: %s, %v", header, err)
	}
	brokenWorker := &WorkerGenerator{
		options: normalizedOptions(Options{}), executable: filepath.Join(t.TempDir(), "missing-worker"), environment: []string{workerEnvironment + "=1"},
	}
	source := preview.OnePixelWebP()
	if _, err := brokenWorker.Generate(context.Background(), preview.GenerationRequest{
		Source: bytes.NewReader(source), SourceSize: int64(len(source)), MediaType: "image/webp", Variant: 64,
	}); err == nil {
		t.Fatal("missing worker executable succeeded")
	}
	mismatchedResponse := appendWorkerTestHeader(t, []byte(`{"version":1,"width":1,"height":1,"size":1}`), nil)
	if _, _, err := decodeWorkerResponse(mismatchedResponse); err == nil {
		t.Fatal("worker response with missing artifact bytes succeeded")
	}
	truncatedHeader := appendWorkerTestHeader(t, []byte(`{"version":1}`), nil)
	truncatedHeader = truncatedHeader[:len(truncatedHeader)-1]
	if _, err := readWorkerHeader(bytes.NewReader(truncatedHeader)); err == nil {
		t.Fatal("truncated worker request header succeeded")
	}
	if _, err := readWorkerResponseHeader(bytes.NewReader(truncatedHeader)); err == nil {
		t.Fatal("truncated worker response header succeeded")
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
