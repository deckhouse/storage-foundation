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

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deckhouse/sds-common-lib/fs/real"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/certutil"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/repository"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/ttl_control"
)

const (
	tlsTTL           time.Duration = 24 * time.Hour
	kubernetesCAPath               = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	// ControllerNamespace               = "d8-data-exporter"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	fsys := real.GetOS()

	logger.Info("Start main()")

	// Parse CLI arguments
	cfg := &config.Config{}
	cfg.Parse()
	cfg.Port = fmt.Sprintf(":%s", cfg.Port)

	// get kube client
	k8sClient, err := repository.NewK8sClient()
	if err != nil {
		logger.Error("Failed to initialize kubernetes client", "error", err)
		os.Exit(1)
	}

	var mainHTTPHandler http.Handler
	switch cfg.Operation {
	case common.OperationExport:
		mainHTTPHandler, err = NewExporterHandler(fsys, cfg.Path, cfg.Mode, logger)
	case common.OperationImport:
		mainHTTPHandler, err = NewImportHandler(fsys, cfg.Path, cfg.Mode, logger, k8sClient, cfg, k8sClient)
	default:
		err = fmt.Errorf("invalid operation: %s", cfg.Operation)
	}
	if err != nil {
		logger.Error("Failed to initialize handler", "error", err)
		os.Exit(1)
	}

	// Run Idle Timer
	ctx, cancel := context.WithCancel(context.Background())
	ttlControl, err := ttl_control.NewTTLControl(
		cfg.Operation,
		cfg.TTLStr,
		cfg.URLOpt.DataManagerNamespace,
		cfg.DataManagerName,
		k8sClient,
		k8sClient,
		logger,
	)
	if err != nil {
		logger.Error("Failed to initialize ttl control", "error", err)
		os.Exit(1)
	}

	// Receive OS signals for graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(
		signalChan,
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGHUP,  // kill -SIGHUP XXXX
		syscall.SIGQUIT, // kill -SIGQUIT XXXX
	)

	// Generate TLS bundle
	tlsBundle, err := certutil.GenerateTLSBundle(logger, os.Getenv("POD_IP"), cfg.DataManagerServiceName, cfg.ControllerNamespace, tlsTTL)
	if err != nil {
		logger.Error("Generate TLS bundle", "failed", err.Error())
		os.Exit(1)
	}

	err = k8sClient.CreateOrUpdateDataManagerCASecretIfNeeded(ctx, cfg.ControllerNamespace, cfg.DataManagerCASecretName, tlsBundle.CACertPEM)
	if err != nil {
		logger.Error("Create DataManager CA secret", "failed", err.Error())
		os.Exit(1)
	}

	// Get kubernetes CA cert pool
	kubernetesCACertPool := x509.NewCertPool()
	kubernetesCACert, err := os.ReadFile(kubernetesCAPath)
	if err != nil {
		logger.Error("Read Kubernetes CA certificate", "failed", err.Error())
		os.Exit(1)
	}
	if !kubernetesCACertPool.AppendCertsFromPEM(kubernetesCACert) {
		logger.Error("Append Kubernetes CA certificate to cert pool", "failed", "invalid CA certificate")
		os.Exit(1)
	}

	wg := sync.WaitGroup{}
	mux := http.NewServeMux()

	ttlControl.Start(ctx, &wg)

	// ttlControl.Wrap is handed to the assembly rather than applied here: where the transfer accounting sits
	// in the chain decides what counts as activity, and that decision lives with the chain.
	mainHandler, err := NewDataServerHandler(
		mux,
		cfg,
		mainHTTPHandler,
		NewFinishedHandler(k8sClient, cfg, logger),
		k8sClient,
		ttlControl.Wrap,
		logger,
	)
	if err != nil {
		logger.Error("Failed to assemble HTTP handler", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:    cfg.Port,
		Handler: logRequest(mainHandler, *logger),
		// ReadHeaderTimeout (not ReadTimeout) guards against slowloris on the request line/headers.
		// ReadTimeout caps the entire request including the body, which would abort large streaming
		// block/filesystem uploads mid-flight (io.Copy(dev, r.Body)) once the device write is slower
		// than the timeout. The connection lifetime/idle is governed by ttl_control instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       10 * time.Second,
		TLSConfig: &tls.Config{
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    kubernetesCACertPool,
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{tlsBundle.ServerCert},
		},
	}

	// Run file server
	logger.Info(
		"Starting https server",
		"operation", cfg.Operation,
		"mode", cfg.Mode,
		"port", cfg.Port,
		"path", cfg.Path,
		"ttl", cfg.TTLStr,
		"DataManager namespace", cfg.URLOpt.DataManagerNamespace,
		"name", cfg.DataManagerName,
		"short kind", cfg.URLOpt.DataManagerTargetKindShort,
		"served path prefix", GetPathPrefix(cfg.URLOpt))
	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			logger.Error(err.Error())
			os.Exit(1)
		}
	}()

	// Update DataManager resource with CA certificate
	err = k8sClient.UpdateDataManagerCA(ctx, cfg.Operation, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, tlsBundle.CACertPEM)
	if err != nil {
		logger.Error("Update DataManager CA certificate", "failed", err.Error())
		os.Exit(1)
	}

	// Publish serverState=Ready now that the server is listening and its CA is published: this is the
	// pod's only readiness signal to the controller, which turns it into the Ready condition/phase. The
	// pod is the sole writer of serverState and SetServerState is monotonic (Ready is set only from the
	// unset state), so on a restart over an already-terminal object (Finished/IdleExpired) this is a
	// no-op. A failed write exits rather than continuing without the signal: this pod is disposable and its
	// Deployment recreates it, so dying and republishing on the next start is the recovery, whereas a server
	// that listens while the controller still sees no readiness is a transfer nobody can begin. The CA step
	// above exits for the same reason.
	err = k8sClient.SetServerState(ctx, cfg.Operation, cfg.URLOpt.DataManagerNamespace, cfg.DataManagerName, common.ServerStateReady)
	if err != nil {
		logger.Error("Set DataManager serverState Ready", "failed", err.Error())
		os.Exit(1)
	}

	// Waiting for shutdown events
	<-signalChan
	logger.Info("OS interrupt - shutting down...")

	// waiting for update DataManager resource goroutines
	cancel()
	wg.Wait()

	// Graceful shutdown server
	ctx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	err = server.Shutdown(ctx)
	if err != nil {
		logger.Error(err.Error())
	} else {
		logger.Info("web server gracefully stopped")
	}
}

func logRequest(handler http.Handler, logger slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The limit of the withholding below: the request line is logged whole, and requestUri includes the
		// query string. That is safe only because of a property of the protocol, not because of anything this
		// function does — no endpoint here takes a credential as a query parameter. An endpoint that ever
		// does will leak it into the pod log by this line, whatever the header handling further down decides,
		// so such a parameter needs withholding here first.
		logger.Info("received request",
			"method", r.Method,
			"path", r.URL.Path,
			"host", r.Host,
			"pattern", r.Pattern,
			"remoteAddr", r.RemoteAddr,
			"requestUri", r.RequestURI,
		)

		for name, value := range r.Header {
			logHeader(&logger, name, value)
		}

		handler.ServeHTTP(w, r)
	})
}

// loggableHeaders is the allow-list of request headers whose VALUE may be written to the log. A header that
// is not here is logged by name only.
//
// The default has to be this way round. A deny-list of the names known to carry credentials leaks the moment
// somebody introduces a header this file has never heard of, it leaks silently, and it leaks into a place
// where the damage lasts: pod logs are readable by more people than the credential ever was and outlive the
// transfer the credential was minted for. An allow-list fails the other way — a new header shows up in the
// log without its value, somebody notices, and the fix is one line here.
//
// The list is deliberately NOT built from the required-upload-header set in internal/middleware. That set
// says what a write must carry; if it ever grows a name that carries a credential, the growth must not widen
// what this function prints.
var loggableHeaders = map[string]bool{
	// The upload protocol: these are what a person debugging a rejected or resumed transfer reads.
	"X-Content-Length":        true,
	"X-Offset":                true,
	"X-Attribute-Permissions": true,
	"X-Attribute-Uid":         true,
	"X-Attribute-Gid":         true,
	// Transport and content negotiation.
	"Accept":          true,
	"Accept-Encoding": true,
	"Connection":      true,
	"Content-Length":  true,
	"Content-Type":    true,
	"Expect":          true,
	"Range":           true,
	"User-Agent":      true,
	// Enough of the CORS preflight to tell a browser-side failure from a server-side one.
	"Origin":                         true,
	"Access-Control-Request-Method":  true,
	"Access-Control-Request-Headers": true,
}

// hiddenValue is what a withheld header value is logged as. It is the whole value: a prefix of a token is
// still token material, so there is no truncated form of this that would be safe to print.
const hiddenValue = "<hidden>"

// schemeHeaders name their authentication scheme in the first word of the value. For those the scheme is
// logged next to the hidden value, because "Authorization arrived and it was a Bearer" is what someone
// debugging a refused transfer actually needs, and the scheme name is not the secret. Cookie is absent on
// purpose: it has no scheme word, and every byte of it is credential material.
var schemeHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
}

// knownSchemes are the scheme names that may be logged, mapped from their lowercase form to the spelling
// used in the log. Only a name from this set is printed. Anything else is reported as unrecognized rather
// than echoed — a value with no scheme word at all, a bare token pasted into the header, would otherwise be
// printed as if it were the scheme name, which is precisely the leak this is here to prevent.
var knownSchemes = map[string]string{
	"bearer":    "Bearer",
	"basic":     "Basic",
	"negotiate": "Negotiate",
}

const unrecognizedScheme = "unrecognized"

// logHeader writes one request header to the log: its name always, its value only if the name is
// allow-listed. Keeping the name is what makes the log useful — the presence of a header is diagnostics,
// its value is a secret we have no business copying anywhere.
//
// Guard: TestLogRequestHidesCredentialHeaderValues.
func logHeader(logger *slog.Logger, name string, values []string) {
	canonical := http.CanonicalHeaderKey(name)

	switch {
	case loggableHeaders[canonical]:
		logger.Info("request header", "name", name, "value", values)
	case schemeHeaders[canonical]:
		logger.Info("request header", "name", name, "value", hiddenValue, "scheme", authScheme(values))
	default:
		logger.Info("request header", "name", name, "value", hiddenValue)
	}
}

func authScheme(values []string) string {
	if len(values) == 0 {
		return unrecognizedScheme
	}

	scheme, _, found := strings.Cut(values[0], " ")
	if !found {
		return unrecognizedScheme
	}

	if logged, ok := knownSchemes[strings.ToLower(scheme)]; ok {
		return logged
	}

	return unrecognizedScheme
}
