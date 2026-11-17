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

	"github.com/deckhouse/sds-common-lib/logger"
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

const port = ":8443"

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
	cfg, err := initFlags()
	if err != nil {
		log.Error("unable to parse config", logger.Err(err))
		os.Exit(1)
	}

	hs, err := handlers.Handlers(log)
	if err != nil {
		log.Error("error creating the webhook handlers", logger.Err(err))
		os.Exit(1)
	}

	mux := http.NewServeMux()
	for path, h := range hs {
		mux.Handle(path, h)
	}
	mux.HandleFunc("/healthz", httpHandlerHealthz)

	log.Info("listening", "address", port)
	err = http.ListenAndServeTLS(port, cfg.certFile, cfg.keyFile, mux)
	if err != nil {
		log.Error("error serving webhook", logger.Err(err))
		os.Exit(1)
	}
}
