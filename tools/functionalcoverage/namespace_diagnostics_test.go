//go:build linux && amd64

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func namespaceDiagnosticLine(t *testing.T, row helperDiagnosticRow) []byte {
	t.Helper()
	body, e := json.Marshal(row)
	if e != nil {
		t.Fatal("diagnostic fixture")
	}
	return append(append([]byte(helperDiagnosticPrefix), body...), '\n')
}
func TestNamespaceDiagnosticForwardingIsClosedBoundedAndPrivate(t *testing.T) {
	base := helperDiagnosticRow{Schema: "endlessfs-functional-diagnostic-v1", Role: "consume", Stage: "namespace_startup", Event: "stage", Result: "entered"}
	secret := "private/path/user-id/capability"
	var output bytes.Buffer
	line := namespaceDiagnosticLine(t, base)
	withUnknown := bytes.Replace(line, []byte(`"schema":`), []byte(`"private_unknown":"`+secret+`","schema":`), 1)
	if !closedNamespaceDiagnostics(append([]byte("unshare: "+secret+"\n"), withUnknown...), &output) || strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "namespace_startup") {
		t.Fatal("raw launcher/unknown diagnostic content escaped")
	}
	for _, mutate := range []func(*helperDiagnosticRow){
		func(r *helperDiagnosticRow) { r.Schema = secret }, func(r *helperDiagnosticRow) { r.Role = secret }, func(r *helperDiagnosticRow) { r.Stage = secret }, func(r *helperDiagnosticRow) { r.Event = secret }, func(r *helperDiagnosticRow) { r.Result = secret }, func(r *helperDiagnosticRow) { r.Command = secret }, func(r *helperDiagnosticRow) { r.ScanStage = secret }, func(r *helperDiagnosticRow) { r.ScanReason = secret }, func(r *helperDiagnosticRow) { r.PackageSHA256 = secret }, func(r *helperDiagnosticRow) { r.RootSHA256 = secret }, func(r *helperDiagnosticRow) { r.FailureLine = 10001 }, func(r *helperDiagnosticRow) { r.FailureFile = 16 }, func(r *helperDiagnosticRow) { r.DurationNanoseconds = -1 },
	} {
		r := base
		mutate(&r)
		output.Reset()
		if closedNamespaceDiagnostics(namespaceDiagnosticLine(t, r), &output) || output.Len() != 0 {
			t.Fatal("unsafe diagnostic forwarded")
		}
	}
	if closedNamespaceDiagnostics([]byte(helperDiagnosticPrefix+"{invalid}\n"), &output) {
		t.Fatal("invalid JSON forwarded")
	}
	if closedNamespaceDiagnostics([]byte(helperDiagnosticPrefix+strings.Repeat("x", helperDiagnosticRowBytes)+"\n"), &output) {
		t.Fatal("oversized row accepted")
	}
	output.Reset()
	if closedNamespaceDiagnostics(bytes.Repeat(line, helperDiagnosticRows+1), &output) {
		t.Fatal("row budget exceeded")
	}
	if closedNamespaceDiagnostics(line, namespaceWriter{}) {
		t.Fatal("failed diagnostic forwarding ignored")
	}
	for _, event := range []string{"stage", "command", "terminal"} {
		r := base
		r.Event = event
		if !consumerDiagnosticAllowed(r) {
			t.Fatal("known event rejected")
		}
	}
	for _, result := range []string{"entered", "incomplete", "failed", "passed", "success", "fallback_selected", "deadline", "canceled"} {
		r := base
		r.Result = result
		if !consumerDiagnosticAllowed(r) {
			t.Fatal("known result rejected")
		}
	}
}
