package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func diagnosticRows(t *testing.T, body []byte) []helperDiagnosticRow {
	t.Helper()
	var rows []helperDiagnosticRow
	for _, line := range bytes.Split(bytes.TrimSuffix(body, []byte{'\n'}), []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte(helperDiagnosticPrefix)) || len(line) > helperDiagnosticRowBytes+len(helperDiagnosticPrefix) {
			t.Fatal("diagnostic framing/bound violated")
		}
		var row helperDiagnosticRow
		d := json.NewDecoder(bytes.NewReader(line[len(helperDiagnosticPrefix):]))
		d.DisallowUnknownFields()
		if e := d.Decode(&row); e != nil {
			t.Fatal("closed diagnostic JSON invalid")
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			t.Fatal("diagnostic trailing value")
		}
		if row.Schema != "endlessfs-functional-diagnostic-v1" || row.ElapsedNanoseconds < 0 {
			t.Fatal("diagnostic identity/time invalid")
		}
		rows = append(rows, row)
	}
	return rows
}

type privateDiagnosticPanic struct{}

func (*privateDiagnosticPanic) Error() string { panic("Error must never be evaluated") }
func TestFunctionalCoverageDiagnosticFailurePrecedesCleanupAndPreservesPanic(t *testing.T) {
	var body bytes.Buffer
	cleaned := false
	sawTerminalBeforeCleanup := false
	writer := diagnosticTestWriter{write: func(p []byte) (int, error) {
		if bytes.Contains(p, []byte(`"event":"terminal"`)) && !cleaned {
			sawTerminalBeforeCleanup = true
		}
		return body.Write(p)
	}}
	ctx := newHelperDiagnostic(context.Background(), writer, "produce")
	cause := &privateDiagnosticPanic{}
	var caught any
	func() {
		defer func() { caught = recover() }()
		func() {
			defer finishHelperDiagnostic(ctx) // Early boundary, shared once state.
			defer func() { cleaned = true }()
			defer finishHelperDiagnostic(ctx) // MUST execute before private cleanup.
			setHelperStage(ctx, diagnosticEvents)
			panic(cause)
		}()
	}()
	if caught != cause || !cleaned || !sawTerminalBeforeCleanup {
		t.Fatal("panic identity or before-cleanup failure evidence lost")
	}
	rows := diagnosticRows(t, body.Bytes())
	terminals := 0
	for _, r := range rows {
		if r.Event == "terminal" {
			terminals++
			if r.Stage != "event_validation" || r.Result != "failed" {
				t.Fatal("wrong closed failure location")
			}
		}
	}
	if terminals != 1 {
		t.Fatal("nested boundaries duplicated terminal observation")
	}
}
func TestFunctionalCoverageDiagnosticProjectsOnlyClosedCommandFields(t *testing.T) {
	secret := "PRIVATE_SENTINEL/path/user/token/capability/body"
	var body bytes.Buffer
	ctx := newHelperDiagnostic(context.Background(), &body, secret)
	setHelperStage(ctx, helperStage(255))
	r := commandResult{Argv: []string{secret}, StartedUTC: time.Now(), PhysicalNanoseconds: 123, StdoutBytes: 456, StderrBytes: 789, StdoutSHA256: secret, StderrSHA256: secret}
	r.Cleanup = cleanupObservation{Adequate: true, NaturalZero: true, FinalZero: true, Scope: secret, Failure: secret, InitialKinds: map[string]uint64{secret: 1}}
	recordHelperCommand(ctx, secret, r, true)
	finishHelperDiagnostic(ctx)
	if strings.Contains(body.String(), secret) || strings.Contains(body.String(), "Argv") || strings.Contains(body.String(), "Scope") || strings.Contains(body.String(), "SHA256") {
		t.Fatal("private command/error metadata escaped projection")
	}
	rows := diagnosticRows(t, body.Bytes())
	found := false
	for _, row := range rows {
		if row.Role != "unknown" || row.Stage != "unknown" || !row.Invalid {
			t.Fatal("unknown labels escaped finite registry")
		}
		if row.Event == "command" {
			found = true
			if row.Command != "unknown" || row.DurationNanoseconds != 123 || row.StdoutBytes != 456 || row.StderrBytes != 789 || !row.CleanupObserved || !row.NaturalZero {
				t.Fatal("safe outcome projection incomplete")
			}
		}
	}
	if !found {
		t.Fatal("missing command observation")
	}
}
func TestFunctionalCoverageDiagnosticBoundsAndScalarRejections(t *testing.T) {
	var body bytes.Buffer
	ctx := newHelperDiagnostic(context.Background(), &body, "consume")
	for i := 0; i < helperDiagnosticRows+7; i++ {
		setHelperStage(ctx, diagnosticArtifact)
	}
	setHelperSuccess(ctx)
	finishHelperDiagnostic(ctx)
	finishHelperDiagnostic(ctx)
	rows := diagnosticRows(t, body.Bytes())
	last := rows[len(rows)-1]
	if len(rows) != helperDiagnosticRows || last.Event != "terminal" || last.Result != "success" || last.Dropped != 8 {
		t.Fatal("bounded tail/reserved terminal accounting changed")
	}
	var invalid bytes.Buffer
	ctx = newHelperDiagnostic(context.Background(), &invalid, "produce")
	recordHelperCommand(ctx, "coverage", commandResult{ExitCode: 999, PhysicalNanoseconds: -1, StdoutBytes: -1, StderrBytes: -1}, false)
	finishHelperDiagnostic(ctx)
	row := diagnosticRows(t, invalid.Bytes())[0]
	if !row.Invalid || row.ExitCode != -1 || row.DurationNanoseconds != 0 || row.StdoutBytes != 0 || row.StderrBytes != 0 || row.CommandObserved || row.CleanupObserved || row.Result != "incomplete" {
		t.Fatal("invalid scalar/unfinished command credited")
	}
}
func TestFunctionalCoverageDiagnosticSuccessCancellationFallbackAndIO(t *testing.T) {
	for _, kind := range []string{"success", "canceled", "deadline", "fallback_selected"} {
		t.Run(kind, func(t *testing.T) {
			var body bytes.Buffer
			base := context.Background()
			var cancel context.CancelFunc
			if kind == "canceled" {
				base, cancel = context.WithCancel(base)
				cancel()
			}
			if kind == "deadline" {
				base, cancel = context.WithDeadline(base, time.Unix(1, 0))
				defer cancel()
			}
			ctx := newHelperDiagnostic(base, &body, "consume")
			setHelperStage(ctx, diagnosticComplete)
			if kind == "success" {
				setHelperSuccess(ctx)
			}
			if kind == "fallback_selected" {
				setHelperFallback(ctx)
			}
			finishHelperDiagnostic(ctx)
			rows := diagnosticRows(t, body.Bytes())
			if rows[len(rows)-1].Result != kind {
				t.Fatal("terminal result changed existing control outcome")
			}
		})
	}
	var body bytes.Buffer
	calls := 0
	writer := diagnosticTestWriter{write: func(p []byte) (int, error) {
		calls++
		if calls == 1 {
			return 0, io.ErrShortWrite
		}
		return body.Write(p)
	}}
	ctx := newHelperDiagnostic(context.Background(), writer, "produce")
	setHelperStage(ctx, diagnosticProfile)
	finishHelperDiagnostic(ctx)
	rows := diagnosticRows(t, body.Bytes())
	if len(rows) != 1 || !rows[0].DiagnosticIOFailed || rows[0].Result != "failed" {
		t.Fatal("diagnostic write failure hidden")
	}
}

type diagnosticTestWriter struct{ write func([]byte) (int, error) }

func (w diagnosticTestWriter) Write(p []byte) (int, error) { return w.write(p) }
