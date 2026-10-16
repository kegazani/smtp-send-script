package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func recordTestInstance(t *testing.T, dir, url, session string) {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	if err := atomicJSON(filepath.Join(dir, "instance.json"), instance{Port: n, Session: session}); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUpdateRefusesBusyInstance(t *testing.T) {
	for _, operation := range []string{"sending", "generating"} {
		t.Run(operation, func(t *testing.T) {
			a := newTestApp(t)
			a.sending = operation == "sending"
			a.generating = operation == "generating"
			var stopped atomic.Bool
			a.shutdown = func() { stopped.Store(true) }
			server := httptest.NewServer(a.handler())
			defer server.Close()
			a.origin = server.URL
			recordTestInstance(t, a.dir, server.URL, a.session)
			if err := prepareUpdate(a.dir); err == nil || !strings.Contains(err.Error(), "отправка или генерация") {
				t.Fatal("busy instance must be left running", err)
			}
			if stopped.Load() {
				t.Fatal("update stopped an active operation")
			}
		})
	}
}

func TestPrepareUpdateStopsIdleInstanceAndPreservesProject(t *testing.T) {
	a := newTestApp(t)
	before, err := os.ReadFile(filepath.Join(a.dir, "project.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.handler())
	defer server.Close()
	a.origin = server.URL
	a.shutdown = server.Close
	recordTestInstance(t, a.dir, server.URL, a.session)
	if err := prepareUpdate(a.dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(a.dir, "project.json"))
	if string(before) != string(after) {
		t.Fatal("update preparation changed project data")
	}
	if err := prepareUpdate(a.dir); err != nil {
		t.Fatal("stale instance should not prevent reinstall", err)
	}
}

func TestPrepareUpdateNeverStopsDifferentSession(t *testing.T) {
	a := newTestApp(t)
	var stopped atomic.Bool
	a.shutdown = func() { stopped.Store(true) }
	server := httptest.NewServer(a.handler())
	defer server.Close()
	a.origin = server.URL
	recordTestInstance(t, a.dir, server.URL, "wrong-session")
	if err := prepareUpdate(a.dir); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() {
		t.Fatal("an unrelated instance was stopped")
	}
}

func TestPrepareUpdateHonorsQuitRefusal(t *testing.T) {
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("maildesk")
		if err != nil || cookie.Value != "fixture-session" || r.Header.Get("Origin") != "http://"+r.Host {
			t.Error("instance request lost session or origin")
		}
		if r.URL.Path == "/api/quit" {
			w.WriteHeader(409) // An operation started after the state check.
			return
		}
		respond(w, map[string]any{"sending": false, "generating": false})
	}))
	defer server.Close()
	recordTestInstance(t, dir, server.URL, "fixture-session")
	if err := prepareUpdate(dir); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatal("quit refusal must abort the update", err)
	}
}

func TestWaitReadyChecksActualVersionAndRecipientMode(t *testing.T) {
	a := newTestApp(t)
	server := httptest.NewServer(a.handler())
	defer server.Close()
	a.origin = server.URL
	recordTestInstance(t, a.dir, server.URL, a.session)
	if err := waitReady(a.dir, testRecipient); err != nil {
		t.Fatal(err)
	}
	if err := waitReady(a.dir, ""); err == nil {
		t.Fatal("a different recipient mode was treated as the newly installed instance")
	}
}
