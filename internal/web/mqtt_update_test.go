package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/caligone/openqiara/internal/config"
)

// mqttTestServer builds a Server whose store is seeded with a TLS-configured
// broker, and returns both the auth-wrapped handler and the store so a test can
// PUT and then read back the persisted config.
func mqttTestServer(t *testing.T) (http.Handler, *config.Store) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(c *config.Config) {
		c.MQTT = config.MQTTConfig{
			Broker:      "ssl://broker:8883",
			Username:    "openqiara",
			TopicPrefix: "openqiara",
			TLSCACert:   "/data/mqtt/ca.pem",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	s := NewServer(&stubCamera{}, store,
		func() bool { return false }, &MQTTCallbacks{},
		fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ui")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux, err := s.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	return s.basicAuth(mux), store
}

func putMQTT(t *testing.T, h http.Handler, jsonBody string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/mqtt", strings.NewReader(jsonBody))
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestUpdateMQTT_PartialPUTPreservesTLS guards the regression where the web UI
// form (which omits tls_* keys) would wipe a file-configured TLS setup and
// silently downgrade the broker to plaintext on the next reboot.
func TestUpdateMQTT_PartialPUTPreservesTLS(t *testing.T) {
	h, store := mqttTestServer(t)

	// Exactly what the UI MQTT form sends: no tls_* fields.
	code := putMQTT(t, h, `{"broker":"ssl://broker:8883","username":"openqiara","topic_prefix":"newprefix"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", code)
	}

	got := store.Get().MQTT
	if got.TLSCACert != "/data/mqtt/ca.pem" {
		t.Errorf("partial PUT wiped TLSCACert: got %q, want /data/mqtt/ca.pem", got.TLSCACert)
	}
	if got.TopicPrefix != "newprefix" {
		t.Errorf("TopicPrefix not applied: got %q, want newprefix", got.TopicPrefix)
	}
}

// TestUpdateMQTT_ExplicitTLSAppliesAndClears verifies that an explicit tls_*
// value is applied, and that an explicit empty string legitimately disables it
// (the reason the fields are pointers rather than "" guards).
func TestUpdateMQTT_ExplicitTLSAppliesAndClears(t *testing.T) {
	h, store := mqttTestServer(t)

	if code := putMQTT(t, h, `{"broker":"ssl://broker:8883","tls_ca_cert":"/new/ca.pem"}`); code != http.StatusOK {
		t.Fatalf("set PUT status = %d, want 200", code)
	}
	if got := store.Get().MQTT.TLSCACert; got != "/new/ca.pem" {
		t.Errorf("explicit tls_ca_cert not applied: got %q", got)
	}

	if code := putMQTT(t, h, `{"broker":"tcp://broker:1883","tls_ca_cert":""}`); code != http.StatusOK {
		t.Fatalf("clear PUT status = %d, want 200", code)
	}
	if got := store.Get().MQTT.TLSCACert; got != "" {
		t.Errorf("explicit empty tls_ca_cert did not clear it: got %q", got)
	}
}
