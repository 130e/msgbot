package main

import (
	"net/http"
	"testing"
	"time"
)

func TestNewServerUsesExpectedTimeouts(t *testing.T) {
	cfg := Config{
		ListenAddr: "127.0.0.1",
		Port:       "8181",
	}

	server := newServer(cfg, http.NewServeMux())

	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("newServer() ReadHeaderTimeout = %v, want %v", server.ReadHeaderTimeout, 5*time.Second)
	}
	if server.ReadTimeout != 30*time.Second {
		t.Fatalf("newServer() ReadTimeout = %v, want %v", server.ReadTimeout, 30*time.Second)
	}
	if server.WriteTimeout != 60*time.Second {
		t.Fatalf("newServer() WriteTimeout = %v, want %v", server.WriteTimeout, 60*time.Second)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("newServer() IdleTimeout = %v, want %v", server.IdleTimeout, 60*time.Second)
	}
	if server.Addr != "127.0.0.1:8181" {
		t.Fatalf("newServer() Addr = %q, want %q", server.Addr, "127.0.0.1:8181")
	}
}
