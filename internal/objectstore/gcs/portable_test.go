package gcs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/cachecontrol"
	"github.com/applyinnovations/endlessfs/internal/domain"
	gcstransport "github.com/applyinnovations/endlessfs/internal/objectstore/gcs"
	"github.com/applyinnovations/endlessfs/internal/portable"
	"github.com/applyinnovations/endlessfs/internal/preview"
	previewdurable "github.com/applyinnovations/endlessfs/internal/preview/durable"
	"github.com/applyinnovations/endlessfs/internal/preview/imagegen"
	"github.com/applyinnovations/endlessfs/internal/preview/storecontract"
	"github.com/applyinnovations/endlessfs/internal/provider/providercontract"
	"github.com/applyinnovations/endlessfs/internal/secret"
	"github.com/applyinnovations/endlessfs/internal/state"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func TestContractPortableProviderOverGCSProtocolFake(t *testing.T) {
	providercontract.Run(t, func(t *testing.T) providercontract.Harness {
		server, fake := newGCSServerWithFake(t)
		fake.rejectCompletedDelete = true
		client := protocolClient(t, server)
		clock := domain.NewFixedClock(time.Now().UTC().Truncate(time.Second))
		fake.clock = clock
		backend, err := gcstransport.NewWithTransfers(client, "endlessfs-test", gcstransport.TransferOptions{
			HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
			SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
			Hostname:  server.Listener.Addr().String(), Insecure: true,
			LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(gcsDeterministic(60, 1<<20)), Clock: clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		engine, err := portable.Open(context.Background(), portable.Options{
			Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(gcsDeterministic(61, 4<<20))),
			Writer: portable.WriterConfiguration{
				WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
				KeyringIdentifiers: []string{"session-v1"}, RequiredFeatures: []string{"directory-manifests"},
			},
			LeaseTTL: time.Minute, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute,
			CursorKey: bytes.Repeat([]byte{0x63}, 32),
		})
		if err != nil {
			t.Fatal(err)
		}
		return providercontract.Harness{
			Storage: engine.Files(), Client: server.Client(), Advance: clock.Advance,
			UploadOffset: func(ctx context.Context, scope domain.Scope, uploadID domain.UploadID) (int64, error) {
				status, err := engine.Files().UploadStatus(ctx, scope, uploadID)
				return status.ConfirmedOffset, err
			},
			SimulateOffset: func(_ context.Context, _ domain.Scope, _ domain.UploadID, offset int64) error {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				for id, session := range fake.sessions {
					fake.nextGeneration++
					fake.objects[session.name] = fakeObject{logicalSize: offset, generation: fake.nextGeneration, metageneration: 1}
					delete(fake.sessions, id)
					return nil
				}
				return domain.NewError(domain.ErrorNotFound, "upload session not found")
			},
			ByteCounts: func() providercontract.ByteCounts {
				fake.mu.Lock()
				defer fake.mu.Unlock()
				return providercontract.ByteCounts{Upload: fake.uploadBytes, Download: fake.downloadBytes}
			},
		}
	})
}

func TestContractDurablePreviewStoreOverGCSProtocolFake(t *testing.T) {
	storecontract.Run(t, func(t *testing.T) storecontract.Harness {
		server, fake := newGCSServerWithFake(t)
		clock := domain.NewFixedClock(time.Now().UTC().Truncate(time.Second))
		fake.clock = clock
		fake.allowedOrigin = "https://drive.example.test"
		fake.signedGetCacheControl = "no-cache, no-store, max-age=0"
		backend, err := gcstransport.NewWithTransfers(protocolClient(t, server), "endlessfs-test", gcstransport.TransferOptions{
			HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
			SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
			Hostname:  server.Listener.Addr().String(), Insecure: true,
			LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(gcsDeterministic(62, 4<<20)), Clock: clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		store, err := previewdurable.New(previewdurable.Options{
			Backend: backend, Transfers: backend, Clock: clock,
			IDs:           domain.NewIDGenerator(bytes.NewReader(gcsDeterministic(63, 4<<20))),
			Key:           secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x74}, 32))),
			CapabilityTTL: time.Minute, DataOrigin: server.URL, HTTPClient: server.Client(), AllowedOrigin: fake.allowedOrigin,
		})
		if err != nil {
			t.Fatal(err)
		}
		return storecontract.Harness{
			Store: store, Client: server.Client(), Advance: clock.Advance, Now: clock.Now,
			SetAvailable: func(available bool) {
				fake.mu.Lock()
				fake.unavailable = !available
				fake.mu.Unlock()
			},
		}
	})
}

func TestIntegrationGeneratedPreviewReadsPortableGCSSource(t *testing.T) {
	server, fake := newGCSServerWithFake(t)
	clock := domain.NewFixedClock(time.Now().UTC().Truncate(time.Second))
	fake.clock = clock
	backend, err := gcstransport.NewWithTransfers(protocolClient(t, server), "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  server.Listener.Addr().String(), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(gcsDeterministic(90, 1<<20)), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := portable.Open(context.Background(), portable.Options{
		Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(gcsDeterministic(91, 4<<20))),
		Writer: portable.WriterConfiguration{
			WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1",
			KeyringIdentifiers: []string{"session-v1"}, RequiredFeatures: []string{"directory-manifests"},
		},
		LeaseTTL: time.Minute, UploadTTL: 5 * time.Minute, DownloadTTL: time.Minute,
		CursorKey: bytes.Repeat([]byte{0x63}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}

	owner, _ := domain.ParseUserID(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x67}, 16)))
	scope, _ := domain.NewScope(owner, domain.AreaLive)
	path := domain.MustParseUserPath("/portable-gcs.png")
	pixels := image.NewNRGBA(image.Rect(0, 0, 96, 48))
	for y := range 48 {
		for x := range 96 {
			pixels.SetNRGBA(x, y, color.NRGBA{R: 0x25, G: 0x68, B: 0x9d, A: 0xff})
		}
	}
	var source bytes.Buffer
	if err := png.Encode(&source, pixels); err != nil {
		t.Fatal(err)
	}
	capability, err := engine.Files().CreateUpload(context.Background(), scope, domain.CreateUploadRequest{Path: path, Size: int64(source.Len()), MediaType: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(capability.Method, capability.URL, bytes.NewReader(source.Bytes()))
	for name, value := range capability.Headers {
		request.Header.Set(name, value)
	}
	if capability.Framing == domain.UploadFramingContentRange {
		request.Header.Set("Content-Range", "bytes 0-"+fmt.Sprint(source.Len()-1)+"/"+fmt.Sprint(source.Len()))
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("GCS source upload status = %d", response.StatusCode)
	}
	entry, err := engine.Files().CompleteUpload(context.Background(), scope, domain.CompleteUploadRequest{
		UploadID: capability.UploadID, Path: path, Size: int64(source.Len()), MediaType: "image/png",
	})
	if err != nil {
		t.Fatal(err)
	}

	previewServer, previewFake := newGCSServerWithFake(t)
	previewFake.clock = clock
	previewFake.allowedOrigin = "https://drive.example.test"
	previewBackend, err := gcstransport.NewWithTransfers(protocolClient(t, previewServer), "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: previewServer.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  previewServer.Listener.Addr().String(), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x72}, 32), Random: bytes.NewReader(gcsDeterministic(92, 2<<20)), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewIDs := domain.NewIDGenerator(bytes.NewReader(gcsDeterministic(93, 2<<20)))
	previewStore, err := previewdurable.New(previewdurable.Options{
		Backend: previewBackend, Transfers: previewBackend, Clock: clock, IDs: previewIDs,
		Key:           secret.Value(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x73}, 32))),
		CapabilityTTL: time.Minute, DataOrigin: previewServer.URL, HTTPClient: previewServer.Client(), AllowedOrigin: previewFake.allowedOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := preview.NewService(preview.Options{Automatic: true, Resolutions: []int{256}, MaxConcurrency: 1, ApplicationState: engine}, engine.Files(), previewStore, []preview.Generator{imagegen.New(imagegen.Options{})}, server.Client(), previewIDs, clock)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.Resolve(context.Background(), owner, preview.ResolveRequest{Items: []preview.ItemRequest{{Path: path, Version: entry.Version, Variant: 256}}})
	if err != nil || len(resolved.Items) != 1 || resolved.Items[0].State != preview.StateReady || resolved.Items[0].Capability == nil {
		t.Fatalf("GCS-backed preview resolve = %+v, %v", resolved, err)
	}
	artifactRequest, _ := http.NewRequest(http.MethodGet, resolved.Items[0].Capability.URL, http.NoBody)
	artifactRequest.Header.Set("Origin", previewFake.allowedOrigin)
	artifactResponse, err := previewServer.Client().Do(artifactRequest)
	if err != nil {
		t.Fatal(err)
	}
	artifact, _ := io.ReadAll(artifactResponse.Body)
	_ = artifactResponse.Body.Close()
	if artifactResponse.StatusCode != http.StatusOK || artifactResponse.Header.Get("Access-Control-Allow-Origin") != previewFake.allowedOrigin || artifactResponse.Header.Get("Content-Type") != preview.ContentTypeWebP || !cachecontrol.HasNoStore(artifactResponse.Header) || len(artifact) < 12 || string(artifact[:4]) != "RIFF" || string(artifact[8:12]) != "WEBP" {
		t.Fatalf("preview artifact = status %d type %q bytes %d", artifactResponse.StatusCode, artifactResponse.Header.Get("Content-Type"), len(artifact))
	}
	deniedRequest, _ := http.NewRequest(http.MethodGet, resolved.Items[0].Capability.URL, http.NoBody)
	deniedRequest.Header.Set("Origin", "https://attacker.example.test")
	deniedResponse, err := previewServer.Client().Do(deniedRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = deniedResponse.Body.Close()
	if deniedResponse.StatusCode != http.StatusForbidden || deniedResponse.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("wrong-origin preview status = %d, origin = %q", deniedResponse.StatusCode, deniedResponse.Header.Get("Access-Control-Allow-Origin"))
	}
}

func TestGCSLostMutationSuccessIsRecoveredByAnotherReplica(t *testing.T) {
	server, fake := newGCSServerWithFake(t)
	client := protocolClient(t, server)
	clock := domain.NewFixedClock(time.Now().UTC().Truncate(time.Second))
	fake.clock = clock
	backend, err := gcstransport.NewWithTransfers(client, "endlessfs-test", gcstransport.TransferOptions{
		HTTPClient: server.Client(), GoogleAccessID: "writer@example.iam.gserviceaccount.com",
		SignBytes: func([]byte) ([]byte, error) { return bytes.Repeat([]byte{0x5a}, 256), nil },
		Hostname:  server.Listener.Addr().String(), Insecure: true,
		LeaseKey: bytes.Repeat([]byte{0x42}, 32), Random: bytes.NewReader(gcsDeterministic(70, 1<<20)), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	open := func(seed byte) *portable.Engine {
		engine, openErr := portable.Open(context.Background(), portable.Options{
			Backend: backend, Clock: clock, IDs: domain.NewIDGenerator(bytes.NewReader(gcsDeterministic(seed, 2<<20))),
			Writer:   portable.WriterConfiguration{WriterSetID: "d3JpdGVyLXNldC0wMDAx", ConfigurationDigest: "config-v1", KeyringIdentifiers: []string{"session-v1"}},
			LeaseTTL: time.Minute, CursorKey: bytes.Repeat([]byte{0x63}, 32),
		})
		if openErr != nil {
			t.Fatal(openErr)
		}
		return engine
	}
	first, second := open(71), open(72)
	key := state.MustKey(state.NamespaceAccounts, "lost-success")
	fake.mu.Lock()
	fake.failUploadAfterCommitName = storageformat.StateKey("accounts", key.String()).String()
	fake.mu.Unlock()
	if _, err := first.Create(context.Background(), key, []byte("durable")); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Create() lost-success error = %v", err)
	}
	clock.Advance(2 * time.Minute)
	if _, err := second.CreateCheckpoint(context.Background(), "gcs-lost-success"); err != nil {
		t.Fatalf("CreateCheckpoint() recovery error = %v", err)
	}
	value, err := second.Get(context.Background(), key)
	if err != nil || string(value.Data) != "durable" {
		t.Fatalf("recovered value = %+v, %v", value, err)
	}
}

func gcsDeterministic(seed byte, size int) []byte {
	value := make([]byte, size)
	state := uint64(seed) + 0x9e3779b97f4a7c15
	for index := range value {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		value[index] = byte(state >> 29)
	}
	return value
}
