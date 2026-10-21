package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type namespaceWriter struct {
	n   int
	err error
}

func (w namespaceWriter) Write([]byte) (int, error) { return w.n, w.err }

type namespaceReader struct{ err error }

func (r namespaceReader) Read([]byte) (int, error) { return 0, r.err }

type namespaceTimeoutError struct{}

func (namespaceTimeoutError) Error() string { return "private timeout sentinel" }
func (namespaceTimeoutError) Timeout() bool { return true }

func TestNamespaceHandshakeRequiresCompleteExactTokens(t *testing.T) {
	for _, token := range []string{namespaceReady, namespaceContinue} {
		var b bytes.Buffer
		if writeNamespaceToken(&b, token) != nil || b.String() != token || readNamespaceToken(&b, token) != nil {
			t.Fatal("complete token rejected")
		}
		for _, body := range []string{"", strings.TrimSuffix(token, "\n"), token + "extra", "OTHER-v1\n", strings.Repeat("x", 4096)} {
			if readNamespaceToken(strings.NewReader(body), token) == nil {
				t.Fatal("incomplete/extra/unknown token accepted")
			}
		}
		for _, n := range []int{0, len(token) - 1, len(token) + 1} {
			if writeNamespaceToken(namespaceWriter{n: n}, token) == nil {
				t.Fatal("short/invalid write accepted")
			}
		}
		if writeNamespaceToken(namespaceWriter{n: len(token), err: io.ErrClosedPipe}, token) == nil {
			t.Fatal("failed write accepted")
		}
	}
	if !errors.Is(readNamespaceToken(namespaceReader{namespaceTimeoutError{}}, namespaceReady), errNamespaceTimeout) {
		t.Fatal("timeout class lost")
	}
	if !errors.Is(readNamespaceToken(namespaceReader{errors.New("private/path")}, namespaceReady), errNamespaceTransport) {
		t.Fatal("transport class lost")
	}
}

func TestNamespaceFallbackIsImpossibleAfterContinueAttempt(t *testing.T) {
	for mask := 0; mask < 16; mask++ {
		canceled, committed, reaped, capture := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		got := namespaceFallbackAllowed(canceled, committed, reaped, capture)
		if got != (!canceled && !committed && reaped && !capture) {
			t.Fatal("fallback boundary changed")
		}
		if committed && got {
			t.Fatal("possible realization selected another fresh run")
		}
	}
}

func TestNamespaceMappingPreservesExactNumericCaller(t *testing.T) {
	for _, id := range []int{0, 1, 1000, 2147483647} {
		text := strings.Join([]string{strconv.Itoa(id), strconv.Itoa(id), "1"}, " ")
		if !identityMapValid(" \t"+text+"\n", id) {
			t.Fatal("identity mapping rejected")
		}
	}
	for _, body := range []string{"", "0 1000 1", "1000 0 1", "1000 1000 2", "1000 1000 1\n1001 1001 1", "1000 1000 -1", "4294967296 1000 1", "private 1000 1"} {
		if identityMapValid(body, 1000) {
			t.Fatal("changed/root/multiple/overflow mapping accepted")
		}
	}
	if identityMapValid("0 0 1", -1) {
		t.Fatal("negative caller identity accepted")
	}
}

func TestNamespaceLauncherShapeAndContextAreClosed(t *testing.T) {
	c := config{Helper: "/pinned/helper"}
	want := []string{"--user", "--map-current-user", "--pid", "--fork", "--mount-proc", "--propagation", "private", "--kill-child", "--forward-signals", "--", c.Helper, "consume-isolated", "<IMMUTABLE_CONSUMER_CONFIG>"}
	if !reflect.DeepEqual(namespaceArgv(c, nil), want) {
		t.Fatal("namespace/mapping/signal policy changed")
	}
	if isolatedConsumer(context.Background()) {
		t.Fatal("ordinary context considered isolated")
	}
	if !isolatedConsumer(context.WithValue(context.Background(), isolatedConsumerKey{}, true)) {
		t.Fatal("validated isolated context lost")
	}
	if consumerIsolationPolicy == "" {
		t.Fatal("isolation policy unbound")
	}
}

func TestNamespaceStartupNeedsAllIsolationAndMappingEvidence(t *testing.T) {
	base := namespaceFacts{PID: 1, PPID: 0, UID: 1000, EUID: 1000, GID: 1000, EGID: 1000, ProcSelf: "1", ParentLink: "pid:[12345]", UIDMap: "1000 1000 1\n", GIDMap: "1000 1000 1\n", SeparatePIDNamespace: true}
	if !namespaceFactsValid(base) {
		t.Fatal("complete isolated identity rejected")
	}
	for _, change := range []func(*namespaceFacts){
		func(f *namespaceFacts) { f.PID = 2 }, func(f *namespaceFacts) { f.PPID = 1 }, func(f *namespaceFacts) { f.EUID = 0 }, func(f *namespaceFacts) { f.EGID = 0 }, func(f *namespaceFacts) { f.ProcSelf = "123" }, func(f *namespaceFacts) { f.SeparatePIDNamespace = false },
		func(f *namespaceFacts) { f.ParentLink = "user:[12345]" }, func(f *namespaceFacts) { f.ParentLink = "pid:[0]" }, func(f *namespaceFacts) { f.ParentLink = "pid:[private]" }, func(f *namespaceFacts) { f.ParentLink = "pid:[12345]trailing" },
		func(f *namespaceFacts) { f.UIDMap = "0 1000 1" }, func(f *namespaceFacts) { f.GIDMap = "1000 0 1" },
	} {
		f := base
		change(&f)
		if namespaceFactsValid(f) {
			t.Fatal("incomplete, foreign, root-mapped or unseparated startup accepted")
		}
	}
}
