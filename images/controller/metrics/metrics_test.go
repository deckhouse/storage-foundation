/*
Copyright 2026 Flant JSC

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

package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/deckhouse/storage-foundation/images/controller/metrics"
)

func TestRegister(t *testing.T) {
	t.Parallel()

	// A pedantic registry checks every collected metric against its descriptor on Gather.
	reg := prometheus.NewPedanticRegistry()

	if err := metrics.Register(reg); err != nil {
		t.Fatalf("Register(reg) = %v; want nil", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Errorf("reg.Gather() after Register(reg) = %v; want nil", err)
	}
}
