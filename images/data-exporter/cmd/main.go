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
	"sync"
	"syscall"
	"time"

	"github.com/deckhouse/sds-common-lib/fs/realfs"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/certutil"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/middleware"
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

	// "dummy" mode: the controller schedules a throwaway Job pod that references a WaitForFirstConsumer
	// scratch PVC purely to trigger its binding (scheduling sets the selected node, the provisioner then
	// creates and binds the PV). The pod only needs to start and exit cleanly; running the real binary
	// (the image entrypoint) instead of a shell avoids a StartError on the distroless image, which has no
	// /bin/sh. It exits immediately so the scratch PVC mount is released for the real upload server.
	if len(os.Args) > 1 && os.Args[1] == "dummy" {
		logger.Info("Dummy consumer mode: scratch PVC referenced to trigger WaitForFirstConsumer binding, exiting")
		return
	}

	fsys := realfs.GetFS()

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

	middlewares := []func(http.Handler) http.Handler{
		func(next http.Handler) http.Handler {
			return authorization.Authorize(next, k8sClient, cfg.Operation, cfg.URLOpt.DataManagerNamespace)
		},
	}
	// TODO refactor this later
	if cfg.Operation == common.OperationImport {
		middlewares = append(middlewares, middleware.CheckRequiredHeaders)
	}

	chainedHandler := middleware.Chain(mainHTTPHandler, middlewares...)

	mainHandler := ttlControl.Start(
		ctx,
		chainedHandler,
		&wg,
	)

	switch cfg.Mode {
	case config.VolumeModeBlock:
		MuxAddBlockHandler(mux, cfg.URLOpt, mainHandler, logger)
	case config.VolumeModeFilesystem:
		MuxAddFSHandler(mux, cfg.URLOpt, mainHandler, logger)
	default:
		logger.Error("Invalid program argument, mode", "mode", cfg.Mode)
		os.Exit(1)
	}

	if cfg.Operation == common.OperationImport {
		MuxAddFinishedHandler(mux, cfg.URLOpt, k8sClient, cfg, logger)
	}

	server := &http.Server{
		Addr:    cfg.Port,
		Handler: logRequest(mux, *logger),
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
		logger.Info("received request",
			"method", r.Method,
			"path", r.URL.Path,
			"host", r.Host,
			"pattern", r.Pattern,
			"remoteAddr", r.RemoteAddr,
			"requestUri", r.RequestURI,
		)

		for name, value := range r.Header {
			logger.Info("request header", "name", name, "value", value)
		}

		handler.ServeHTTP(w, r)
	})
}
