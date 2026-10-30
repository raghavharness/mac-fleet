package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type enrollRequest struct {
	Token    string `json:"token,omitempty"`
	Hostname string `json:"hostname"`
	CSR      string `json:"csr"`
}

type enrollResponse struct {
	Cert string `json:"cert"`
	CA   string `json:"ca"`
}

func paths(c *Config) (key, crt, ca string) {
	return filepath.Join(c.DataDir, "agent.key"), filepath.Join(c.DataDir, "agent.crt"), filepath.Join(c.DataDir, "ca.pem")
}

func newKeyAndCSR(hostname string) (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkixName(hostname),
	}, key)
	if err != nil {
		return nil, "", err
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// pinnedClient trusts the server only if a certificate in its chain matches the
// pinned SHA-256. Used for the first contact, before the CA is stored locally.
func pinnedClient(pin string) *http.Client {
	pin = strings.ToLower(strings.ReplaceAll(pin, ":", ""))
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // verification is done against the pin below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			for _, der := range raw {
				sum := sha256.Sum256(der)
				if hex.EncodeToString(sum[:]) == pin {
					return nil
				}
			}
			return errors.New("server certificate chain does not contain the pinned CA")
		},
	}}}
}

// Enroll exchanges a one-time token for a client certificate and stores it.
func Enroll(c *Config, token string) error {
	if c.CAPin == "" {
		return errors.New("ca_sha256 is required for enrollment")
	}
	key, csr, err := newKeyAndCSR(c.Hostname)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(enrollRequest{Token: token, Hostname: c.Hostname, CSR: csr})
	resp, err := pinnedClient(c.CAPin).Post(strings.TrimRight(c.Server, "/")+"/v1/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("enroll failed: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return store(c, key, data)
}

// Renew replaces the client certificate over the existing mTLS connection.
func Renew(c *Config) error {
	key, csr, err := newKeyAndCSR(c.Hostname)
	if err != nil {
		return err
	}
	tc, err := TLSConfig(c)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(enrollRequest{Hostname: c.Hostname, CSR: csr})
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	resp, err := hc.Post(strings.TrimRight(c.Server, "/")+"/v1/renew", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("renew failed: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return store(c, key, data)
}

func store(c *Config, key *ecdsa.PrivateKey, respBody []byte) error {
	var er enrollResponse
	if err := json.Unmarshal(respBody, &er); err != nil {
		return err
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	kp, cp, ap := paths(c)
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(ap, []byte(er.CA), 0o600); err != nil {
		return err
	}
	return os.WriteFile(cp, []byte(er.Cert), 0o600)
}

// TLSConfig builds the runtime mTLS config from the stored credentials.
func TLSConfig(c *Config) (*tls.Config, error) {
	kp, cp, ap := paths(c)
	pair, err := tls.LoadX509KeyPair(cp, kp)
	if err != nil {
		return nil, fmt.Errorf("load client cert (run enroll first): %w", err)
	}
	caPEM, err := os.ReadFile(ap)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid ca.pem")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, RootCAs: pool}, nil
}

// CertExpiry returns the NotAfter of the stored client certificate.
func CertExpiry(c *Config) (time.Time, error) {
	_, cp, _ := paths(c)
	b, err := os.ReadFile(cp)
	if err != nil {
		return time.Time{}, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return time.Time{}, errors.New("invalid agent.crt")
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return crt.NotAfter, nil
}
