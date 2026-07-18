package eventtap

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SplitBrokers turns a comma-separated bootstrap list into a slice, dropping
// blanks (parity with the platform's Kafka clients).
func SplitBrokers(s string) []string {
	var out []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// MaybeTLS returns (nil, nil) when dir is empty or has no user.crt (PLAINTEXT —
// the in-cluster demo), a populated *tls.Config when the full mTLS triple is
// present, or an error when certs are partially present but unreadable (so the
// caller can leave the tap disabled rather than crash-loop).
func MaybeTLS(dir string) (*tls.Config, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	certPath := filepath.Join(dir, "user.crt")
	if _, err := os.Stat(certPath); err != nil {
		return nil, nil // no client cert => plaintext
	}
	keyPath := filepath.Join(dir, "user.key")
	caPath := filepath.Join(dir, "ca.crt")
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", keyPath, err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", caPath, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("ca.crt contains no valid certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}
