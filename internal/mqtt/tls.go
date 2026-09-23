package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// buildTLSConfig returns a *tls.Config for the broker connection, or nil when no
// TLS option is set (paho then applies its own default once the broker scheme is
// ssl://). Invalid or unreadable certificate material is a hard error rather than
// a silent fallback to plaintext: a misconfigured alarm must fail loudly.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	if cfg.TLSCACert == "" && cfg.TLSClientCert == "" && cfg.TLSClientKey == "" && !cfg.TLSInsecure {
		return nil, nil
	}

	tc := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLSInsecure, //nolint:gosec // opt-in flag, documented as test-only
	}

	if cfg.TLSCACert != "" {
		pem, err := os.ReadFile(cfg.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("read mqtt tls_ca_cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mqtt tls_ca_cert %q: no valid certificate found", cfg.TLSCACert)
		}
		tc.RootCAs = pool
	}

	if cfg.TLSClientCert != "" || cfg.TLSClientKey != "" {
		if cfg.TLSClientCert == "" || cfg.TLSClientKey == "" {
			return nil, fmt.Errorf("mqtt mutual TLS needs both tls_client_cert and tls_client_key")
		}
		pair, err := tls.LoadX509KeyPair(cfg.TLSClientCert, cfg.TLSClientKey)
		if err != nil {
			return nil, fmt.Errorf("load mqtt client cert/key: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	}

	return tc, nil
}
