package storageformat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/applyinnovations/endlessfs/internal/domain"
	"github.com/applyinnovations/endlessfs/internal/telemetry"
)

func TestDomainPagePackAdmissionTelemetrySeparatesWaitingFromDecodeAndCancellation(t *testing.T) {
	if cap(domainPagePackDecodeSlot) != 1 {
		t.Fatal("process-wide expansion memory admission is no longer single-slot")
	}
	pack := testDomainPagePack(t)
	wire, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, canceled := range []bool{false, true} {
		observer := telemetry.New(nil, nil)
		base, cancel := context.WithCancel(telemetry.Context(context.Background(), observer))
		waiting := &packAdmissionWaitingContext{Context: base, ready: make(chan struct{})}
		domainPagePackDecodeSlot <- struct{}{}
		result := make(chan error, 1)
		go func() {
			_, err := DecodeDomainPagePack(waiting, wire, pack.DomainID, pack.Kind, pack.PackID)
			result <- err
		}()
		<-waiting.ready
		var active bytes.Buffer
		observer.WritePrometheus(&active)
		if !strings.Contains(active.String(), `endlessfs_operations_active{operation="metadata.pack.admission",role="state"} 1`) {
			<-domainPagePackDecodeSlot
			cancel()
			<-result
			t.Fatal("queued admission is invisible in telemetry")
		}
		if strings.Contains(active.String(), `operation="metadata.pack.decode",role="state",result=`) {
			t.Fatal("queued work decoded before expansion admission")
		}
		if canceled {
			cancel()
		} else {
			<-domainPagePackDecodeSlot
		}
		err := <-result
		if canceled {
			if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
				t.Fatal("admission cancellation semantics changed")
			}
			if len(domainPagePackDecodeSlot) != 1 {
				t.Fatal("canceled waiter consumed another operation's slot")
			}
			<-domainPagePackDecodeSlot
		} else if err != nil {
			t.Fatal(err)
		}
		cancel()
		var output bytes.Buffer
		observer.WritePrometheus(&output)
		resultName := "success"
		if canceled {
			resultName = "canceled"
		}
		if !strings.Contains(output.String(), `endlessfs_operations_total{operation="metadata.pack.admission",role="state",result="`+resultName+`"} 1`) || !strings.Contains(output.String(), `endlessfs_operations_active{operation="metadata.pack.admission",role="state"} 0`) {
			t.Fatal("admission completion/classification missing")
		}
		if canceled && strings.Contains(output.String(), `operation="metadata.pack.decode",role="state",result=`) {
			t.Fatal("canceled admission recorded decode work")
		}
		for _, secret := range []string{pack.DomainID, pack.PackID, "requestID=", "traceID=", "stage=", "reason="} {
			if strings.Contains(output.String(), secret) {
				t.Fatalf("private/cardinality admission metric %q", secret)
			}
		}
		if _, err := DecodeDomainPagePack(context.Background(), wire, pack.DomainID, pack.Kind, pack.PackID); err != nil {
			t.Fatal("admission telemetry leaked slot")
		}
	}
}
