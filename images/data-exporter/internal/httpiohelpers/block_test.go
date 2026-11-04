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

package httpiohelpers

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/deckhouse/sds-common-lib/fs/fake"
)

var errSeek = errors.New("seek failed")

// failingSeeker fails a Seek whose whence is failOn and otherwise behaves like the wrapped reader.
type failingSeeker struct {
	io.ReadSeeker
	failOn int
}

func (s failingSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence == s.failOn {
		return 0, errSeek
	}
	return s.ReadSeeker.Seek(offset, whence)
}

func TestBlockDeviceSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content io.ReadSeeker
		want    int64
		wantErr error
	}{
		{name: "empty", content: strings.NewReader("")},
		{name: "non-empty", content: strings.NewReader("block device content"), want: int64(len("block device content"))},
		{name: "seek to end fails", content: failingSeeker{ReadSeeker: strings.NewReader("content"), failOn: io.SeekEnd}, wantErr: errSeek},
		{name: "seek back to start fails", content: failingSeeker{ReadSeeker: strings.NewReader("content"), failOn: io.SeekStart}, wantErr: errSeek},
	} {
		t.Run(tc.name, func(t *testing.T) {
			size, err := BlockDeviceSize(tc.content)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Zero(t, size)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, size)
		})
	}
}

func TestBlockDeviceSizeRewindsToStart(t *testing.T) {
	content := strings.NewReader("block device content")
	_, err := content.Seek(5, io.SeekStart)
	require.NoError(t, err)

	_, err = BlockDeviceSize(content)
	require.NoError(t, err)

	data, err := io.ReadAll(content)
	require.NoError(t, err)
	require.Equal(t, "block device content", string(data))
}

func TestIsBlockDevice(t *testing.T) {
	fsys, err := fake.NewBuilder("/").
		WithFileAtPath("block", os.ModeDevice, fake.RWContentFromString("content")).
		WithFileAtPath("char", os.ModeDevice|os.ModeCharDevice).
		WithFileAtPath("regular", os.FileMode(0o640)).
		WithFileAtPath("dir", os.ModeDir).
		Build()
	require.NoError(t, err)

	for _, tc := range []struct {
		path    string
		want    bool
		wantErr bool
	}{
		{path: "/block", want: true},
		{path: "/char"},
		{path: "/regular"},
		{path: "/dir"},
		{path: "/missing", wantErr: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			isBlock, err := IsBlockDevice(fsys, tc.path)

			if tc.wantErr {
				require.Error(t, err)
				require.False(t, isBlock)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, isBlock)
		})
	}
}
