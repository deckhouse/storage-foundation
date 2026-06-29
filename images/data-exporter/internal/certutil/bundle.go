/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package certutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"time"
)

type TLSBundle struct {
	ServerCert tls.Certificate
	CACertPEM  []byte
}

func GenerateTLSBundle(logger *slog.Logger, ip, host, namespace string, ttl time.Duration) (*TLSBundle, error) {
	ipParsed := net.ParseIP(ip)
	if ipParsed == nil {
		return nil, fmt.Errorf("invalid IP for SAN: %q", ip)
	}

	if ttl <= 0 {
		return nil, fmt.Errorf("invalid TTL: %s", ttl)
	}

	logger.Debug("Generating TLS bundle", "ip", ip, "ttl", ttl)
	// --- CA -----------------------------------------------------------------
	logger.Debug("Generating CA ECDSA key (P-256)")
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	sn, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	caTpl := x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: "data-exporter-CA"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(ttl),

		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	logger.Debug("Creating self-signed CA certificate")
	caDER, err := x509.CreateCertificate(rand.Reader, &caTpl, &caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	// --- Server -------------------------------------------------------------
	logger.Debug("Generating server ECDSA key (P-256)")
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		logger.Error("generate server key", "err", err)
		return nil, fmt.Errorf("generate server key: %w", err)
	}

	sn, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	srvTpl := x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: ip},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(ttl),

		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{ipParsed},
	}

	if host != "" {
		logger.Debug("Adding SAN for host", "host", host)
		srvTpl.DNSNames = []string{
			host,
			fmt.Sprintf("%s.%s", host, namespace),
			fmt.Sprintf("%s.%s.svc", host, namespace),
			fmt.Sprintf("%s.%s.svc.cluster.local", host, namespace),
		}
	}

	logger.Debug("Signing server certificate with CA")
	srvDER, err := x509.CreateCertificate(rand.Reader, &srvTpl, &caTpl, &srvKey.PublicKey, caKey)
	if err != nil {
		logger.Error("create server cert", "err", err)
		return nil, fmt.Errorf("create server cert: %w", err)
	}

	// --- PEM -------------------------------------------------------
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	srvKeyMarshalled, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return nil, fmt.Errorf("MarshalECPrivateKey: %w", err)
	}
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyMarshalled})

	srvTLS, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		logger.Error("X509KeyPair", "err", err)
		return nil, fmt.Errorf("X509KeyPair: %w", err)
	}

	logger.Info("ECDSA TLS bundle generated", "ip", ip, "ttl", ttl.String())
	return &TLSBundle{
		ServerCert: srvTLS,
		CACertPEM:  caPEM,
	}, nil
}
