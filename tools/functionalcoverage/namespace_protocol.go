package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// This is a local execution boundary, not a resource or performance profile.
const consumerIsolationPolicy = "same-uid-private-user-pid-proc-seqpacket-ready-continue-v1"
const namespaceReady = "READY-v1\n"
const namespaceContinue = "CONTINUE-v1\n"

var errNamespaceTimeout = errors.New("namespace_startup_timeout")
var errNamespaceTransport = errors.New("namespace_transport_inadequate")

type consumerConfigKey struct{}
type isolatedConsumerKey struct{}

func isolatedConsumer(ctx context.Context) bool {
	v, _ := ctx.Value(isolatedConsumerKey{}).(bool)
	return v
}
func namespaceArguments(c config, configPath string, args []string) []string {
	base := []string{"--user", "--map-current-user", "--pid", "--fork", "--mount-proc", "--propagation", "private", "--kill-child", "--forward-signals", "--", c.Helper, "consume-isolated", configPath}
	return append(base, args...)
}
func namespaceArgv(c config, args []string) []string {
	return namespaceArguments(c, "<IMMUTABLE_CONSUMER_CONFIG>", args)
}
func readNamespaceToken(reader io.Reader, token string) error {
	body := make([]byte, len(token)+1)
	n, e := reader.Read(body)
	if e != nil {
		// A peer that exits before READY sends no message. Treat its clean close
		// as a startup rejection; fallback still requires normal child reaping.
		// Timeouts and uncertain transport failures must remain fail closed.
		if errors.Is(e, io.EOF) {
			return errContract
		}
		if os.IsTimeout(e) {
			return errNamespaceTimeout
		}
		return errNamespaceTransport
	}
	if n != len(token) || string(body[:n]) != token {
		return errContract
	}
	return nil
}
func writeNamespaceToken(writer io.Writer, token string) error {
	n, e := io.WriteString(writer, token)
	if e != nil || n != len(token) {
		return errContract
	}
	return nil
}

// Mark commitment BEFORE attempting CONTINUE: a partial or failed write can
// never justify another fresh run. The child cannot realize anything before it.
func namespaceFallbackAllowed(canceled, committed, childReaped, captureFailed bool) bool {
	return !canceled && !committed && childReaped && !captureFailed
}
func identityMapValid(body string, identity int) bool {
	fields := strings.Fields(body)
	if len(fields) != 3 || identity < 0 {
		return false
	}
	for i, want := range []uint64{uint64(identity), uint64(identity), 1} {
		value, e := strconv.ParseUint(fields[i], 10, 32)
		if e != nil || value != want {
			return false
		}
	}
	return true
}

type namespaceFacts struct {
	PID, PPID, UID, EUID, GID, EGID      int
	ProcSelf, ParentLink, UIDMap, GIDMap string
	SeparatePIDNamespace                 bool
}

func namespaceFactsValid(f namespaceFacts) bool {
	if f.PID != 1 || f.PPID != 0 || f.UID != f.EUID || f.GID != f.EGID || f.ProcSelf != "1" || !f.SeparatePIDNamespace {
		return false
	}
	if !strings.HasPrefix(f.ParentLink, "pid:[") || !strings.HasSuffix(f.ParentLink, "]") {
		return false
	}
	id, e := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(f.ParentLink, "pid:["), "]"), 10, 64)
	return e == nil && id > 0 && identityMapValid(f.UIDMap, f.UID) && identityMapValid(f.GIDMap, f.GID)
}
