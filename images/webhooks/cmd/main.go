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
	"flag"
	"fmt"
	"net/http"
	"os"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/logger/kwhlogger"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/webhooks/handlers"
)

type config struct {
	certFile string
	keyFile  string
}

//goland:noinspection SpellCheckingInspection
func httpHandlerHealthz(w http.ResponseWriter, _ *http.Request) {
	_, err := fmt.Fprint(w, "Ok.")
	if err != nil {
		w.WriteHeader(500)
	}
}

func initFlags() (config, error) {
	cfg := config{}

	fl := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fl.StringVar(&cfg.certFile, "tls-cert-file", "", "TLS certificate file")
	fl.StringVar(&cfg.keyFile, "tls-key-file", "", "TLS key file")

	err := fl.Parse(os.Args[1:])
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

const (
	port                    = ":8443"
	volumeSnapshotMutatorID = "volumeSnapshotMutation"
	dataExportValidatorID   = "dataExportValidation"
)

func main() {
	// LOG_LEVEL arrives as a number 0..4 from lib-helm, which is what the
	// controller container in this same pod already reads. This webhook used to
	// ignore it, so its level was whatever main hard-coded no matter what the
	// module setting said.
	log, err := logger.NewLoggerFromEnv(logger.InfoLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to create the logger: %s\n", err)
		os.Exit(1)
	}
	kwhLog := kwhlogger.New(log)
	// One logger per process: the handlers are plain functions with nowhere to pass
	// one through, so they read it from their own package.
	handlers.SetLogger(log)

	cfg, err := initFlags()
	if err != nil {
		fmt.Printf("unable to parse config: err: %s", err.Error())
		os.Exit(1)
	}

	volumeSnapshotMutatingWebHookHandler, err := handlers.GetMutatingWebhookHandler(handlers.VolumeSnapshotMutate, volumeSnapshotMutatorID, &snapshotv1.VolumeSnapshot{}, kwhLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating volumeSnapshotMutatingWebHookHandler: %s", err)
		os.Exit(1)
	}

	dataExportValidatingWebHookHandler, err := handlers.NewValidatingWebhookHandler(handlers.DataExportValidateFunc(), dataExportValidatorID, &dev1alpha1.DataExport{}, kwhLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating dataExportValidatingWebHookHandler: %s", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/volume-snapshot-mutate", volumeSnapshotMutatingWebHookHandler)
	mux.Handle("/dataexport-validate", dataExportValidatingWebHookHandler)
	mux.HandleFunc("/healthz", httpHandlerHealthz)

	log.Info("listening", "address", port)
	err = http.ListenAndServeTLS(port, cfg.certFile, cfg.keyFile, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error serving webhook: %s", err)
		os.Exit(1)
	}
}
