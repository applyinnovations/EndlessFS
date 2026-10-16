package storageformat

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

type cancelPackExpansionContext struct {
	context.Context
	checks   int
	cancelAt int
}

type packAdmissionWaitingContext struct {
	context.Context
	once  sync.Once
	ready chan struct{}
}

func (ctx *packAdmissionWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.ready) })
	return ctx.Context.Done()
}

func (ctx *cancelPackExpansionContext) Err() error {
	ctx.checks++
	if ctx.cancelAt > 0 && ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestDomainPagePackDecodeCancellationBeforeDuringAndWaiting(t *testing.T) {
	pack := testDomainPagePack(t)
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{canceled, &cancelPackExpansionContext{Context: context.Background(), cancelAt: 2}} {
		if _, err := DecodeDomainPagePack(ctx, wire, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
	counting := &cancelPackExpansionContext{Context: context.Background()}
	if _, err := DecodeDomainPagePack(counting, wire, pack.DomainID, pack.Kind, pack.PackID); err != nil {
		t.Fatal(err)
	}
	for check := 1; check <= counting.checks; check++ {
		ctx := &cancelPackExpansionContext{Context: context.Background(), cancelAt: check}
		if _, err := DecodeDomainPagePack(ctx, wire, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation boundary %d: %v", check, err)
		}
	}
	// Occupy the process-wide expansion slot: a canceled queued request must
	// leave without expanding any bytes or consuming the occupied slot.
	domainPagePackDecodeSlot <- struct{}{}
	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiting := &packAdmissionWaitingContext{Context: waiterContext, ready: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := DecodeDomainPagePack(waiting, wire, pack.DomainID, pack.Kind, pack.PackID)
		result <- err
	}()
	<-waiting.ready
	cancelWaiter()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-domainPagePackDecodeSlot
	if _, err := DecodeDomainPagePack(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID); err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
}

func TestDomainPagePackCanonicalComparisonRejectsAmbiguousJSON(t *testing.T) {
	pack := testDomainPagePack(t)
	payload, err := EncodeCanonical(pack)
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"duplicate":        bytes.Replace(payload, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"unknown":          bytes.Replace(payload, []byte(`"schemaVersion":1`), []byte(`"unknown":0,"schemaVersion":1`), 1),
		"nested-duplicate": bytes.Replace(payload, []byte(`"key":"alpha"`), []byte(`"key":"alpha","key":"alpha"`), 1),
		"nested-unknown":   bytes.Replace(payload, []byte(`"key":"alpha"`), []byte(`"unknown":0,"key":"alpha"`), 1),
		"case-insensitive": bytes.Replace(payload, []byte(`"schemaVersion"`), []byte(`"SchemaVersion"`), 1),
		"invalid-utf8":     bytes.Replace(payload, []byte(`alpha`), []byte{0xff}, 1),
		"trailing":         append(append([]byte(nil), payload...), []byte(`{}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(payload, candidate) {
				t.Fatal("denial fixture unchanged")
			}
			wire := encodeDomainPackPayload(t, candidate)
			if _, err := DecodeDomainPagePack(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("ambiguous JSON accepted: %v", err)
			}
		})
	}
}
