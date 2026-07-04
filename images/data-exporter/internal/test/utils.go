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

package test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/deckhouse/sds-common-lib/fs/mockfs"
)

func AssertHeader(t *testing.T, response *http.Response, name string, expected string) {
	header, ok := response.Header[name]
	if ok {
		expected := []string{expected}
		assert.Equal(t, expected, header)
	} else {
		assert.Fail(t, fmt.Sprintf("%s header is missing", name))
	}
}

func AssertNoHeader(t *testing.T, response *http.Response, name string) {
	_, ok := response.Header[name]
	if ok {
		assert.Fail(t, fmt.Sprintf("Unexpected header %s presents", name))
	}
}

func AssertFileContent(t *testing.T, reader io.ReadCloser, expected []byte) {
	buf := make([]byte, len(expected))
	n, err := reader.Read(buf)
	if err != nil && err != io.EOF {
		panic("Error reading from Reader")
	}

	if n != len(expected) {
		t.Errorf("Invalid buffer length: actual %d, expected %d", n, len(expected))
	}

	if !bytes.Equal(buf[:n], expected) {
		t.Error("Invalid buffer content")
	}
}

func SetContent(file *mockfs.MockFile, content string) {
	rw := mockfs.RWContentFromBytes([]byte(content))
	rw.SetupFile(file)
}
