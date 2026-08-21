package main

import (
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type activeHandler struct {
	http.Handler
}

// startupControlHandler keeps process liveness available while the durable
// runtime is still opening. It never claims readiness or exposes application
// routes until the complete handler is atomically activated.
type startupControlHandler struct {
	active atomic.Pointer[activeHandler]
}

func (handler *startupControlHandler) Activate(application http.Handler) {
	if application == nil {
		panic("activate nil application handler")
	}
	handler.active.Store(&activeHandler{Handler: application})
}

func (handler *startupControlHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if active := handler.active.Load(); active != nil {
		active.ServeHTTP(response, request)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok\n"))
		return
	}
	response.WriteHeader(http.StatusServiceUnavailable)
	_, _ = response.Write([]byte("starting\n"))
}

func startControlServer(listenAddress string, writeTimeout time.Duration, logger *slog.Logger) (*http.Server, net.Listener, *startupControlHandler, <-chan error, error) {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	handler := &startupControlHandler{}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	errors := make(chan error, 1)
	go func() {
		logger.Info("server_started", "listenAddress", listener.Addr().String(), "state", "starting")
		errors <- server.Serve(listener)
	}()
	return server, listener, handler, errors, nil
}
