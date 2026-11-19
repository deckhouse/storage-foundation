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

package volumes

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/sys/mountinfo"
)

const (
	uidA = "0b6f6c1e-0d5a-4c39-9e1f-3a8c2f0d7a11"
	uidB = "5f0e2a9c-61b4-4f0f-8c55-2c1d0e9b4b22"
)

func pubPath(uid, pv string) string {
	return "/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/" + pv + "/mount"
}

func dataPath(uid, pv string) string {
	return "/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/" + pv + "/vol_data.json"
}

func parseMounts(t *testing.T, lines ...string) []*mountinfo.Info {
	t.Helper()
	mounts, err := mountinfo.GetMountsFromReader(strings.NewReader(strings.Join(lines, "\n")+"\n"), nil)
	if err != nil {
		t.Fatalf("parse mountinfo: %v", err)
	}
	return mounts
}

func files(m map[string]string) ReadFileFunc {
	return func(name string) ([]byte, error) {
		v, ok := m[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(v), nil
	}
}

func persistentData(pv, driver string) string {
	return `{"attachmentID":"csi-1","driverName":"` + driver + `","nodeName":"worker-0","specVolID":"` + pv + `","volumeHandle":"h-1","volumeLifecycleMode":"Persistent"}`
}

func TestScan(t *testing.T) {
	staging := "/var/lib/kubelet/plugins/kubernetes.io/csi/rbd.csi.ceph.com/1f3a/globalmount"

	tests := []struct {
		name     string
		mounts   []string
		data     map[string]string
		want     []Volume
		wantErrs int
	}{
		{
			name: "writable mount of a writable filesystem",
			mounts: []string{
				"3100 29 252:16 / " + staging + " rw,relatime shared:900 - ext4 /dev/rbd0 rw,stripe=16",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:900 - ext4 /dev/rbd0 rw,stripe=16",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", Major: 252, Minor: 16}},
		},
		{
			name: "writable mount of a filesystem the kernel turned read-only",
			mounts: []string{
				"3100 29 252:16 / " + staging + " rw,relatime shared:900 - ext4 /dev/rbd0 ro,stripe=16",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:900 - ext4 /dev/rbd0 ro,stripe=16",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, Major: 252, Minor: 16}},
		},
		{
			name: "read-only mount the pod asked for",
			mounts: []string{
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " ro,relatime shared:900 - xfs /dev/dm-3 rw,attr2,inode64",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "local.csi.storage.deckhouse.io")},
		},
		{
			name: "read-only NFS mount without staging",
			mounts: []string{
				"3200 3150 0:77 / " + pubPath(uidA, "pvc-n") + " ro,relatime shared:901 - nfs4 10.0.0.1:/export/pvc-n ro,vers=4.2",
			},
			data: map[string]string{dataPath(uidA, "pvc-n"): persistentData("pvc-n", "nfs.csi.storage.deckhouse.io")},
		},
		{
			name: "writable NFS mount without staging",
			mounts: []string{
				"3200 3150 0:77 / " + pubPath(uidA, "pvc-n") + " rw,relatime shared:901 - nfs4 10.0.0.1:/export/pvc-n rw,vers=4.2",
			},
			data: map[string]string{dataPath(uidA, "pvc-n"): persistentData("pvc-n", "nfs.csi.storage.deckhouse.io")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-n", Driver: "nfs.csi.storage.deckhouse.io", FSType: "nfs4", Major: 0, Minor: 77}},
		},
		{
			name: "staging mount alone is not reported",
			mounts: []string{
				"3100 29 252:16 / " + staging + " rw,relatime shared:900 - ext4 /dev/rbd0 ro,stripe=16",
			},
		},
		{
			name: "inline ephemeral volume",
			mounts: []string{
				"3200 3150 0:90 / " + pubPath(uidA, "secrets-store") + " rw,relatime - tmpfs tmpfs ro,size=1024k",
			},
			data: map[string]string{
				dataPath(uidA, "secrets-store"): `{"driverName":"secrets-store.csi.k8s.io","specVolID":"secrets-store","volumeLifecycleMode":"Ephemeral"}`,
			},
		},
		{
			name: "block volume is not a filesystem mount",
			mounts: []string{
				"3300 29 0:5 /rbd0 /var/lib/kubelet/pods/" + uidA + "/volumeDevices/kubernetes.io~csi/pvc-b rw,nosuid shared:2 - devtmpfs udev ro,size=4096k",
			},
		},
		{
			name: "mount over the same path hides the one below",
			mounts: []string{
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:900 - ext4 /dev/rbd0 ro",
				"3201 3200 252:32 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:902 - ext4 /dev/rbd1 rw",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", Major: 252, Minor: 32}},
		},
		{
			name: "volume the driver staged read-only, published without ro",
			mounts: []string{
				"3100 29 252:16 / " + staging + " ro,relatime shared:900 - ext4 /dev/rbd0 ro,stripe=16",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:900 - ext4 /dev/rbd0 ro,stripe=16",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
		},
		{
			name: "read-only staging of another device does not hide a broken volume",
			mounts: []string{
				"3100 29 252:32 / " + staging + " ro,relatime shared:900 - ext4 /dev/rbd1 ro",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:901 - ext4 /dev/rbd0 ro",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, Major: 252, Minor: 16}},
		},
		{
			name: "read-only mount outside the staging directory does not hide a broken volume",
			mounts: []string{
				"3100 29 252:16 / /var/lib/kubelet/plugins/kubernetes.io/csi/rbd.csi.ceph.com/1f3a rw - ext4 /dev/rbd0 ro",
				"3101 29 252:16 / /var/lib/kubelet/plugins/kubernetes.io/csi/rbd.csi.ceph.com/1f3a/globalmount/snap ro - ext4 /dev/rbd0 ro",
				"3102 29 252:16 / /var/lib/kubelet/plugins-old/kubernetes.io/csi/rbd.csi.ceph.com/1f3a/globalmount ro - ext4 /dev/rbd0 ro",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime - ext4 /dev/rbd0 ro",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, Major: 252, Minor: 16}},
		},
		{
			name: "mount over the same path listed before the one below",
			mounts: []string{
				"3201 3200 252:32 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:902 - ext4 /dev/rbd1 rw",
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime shared:900 - ext4 /dev/rbd0 ro",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com")},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", Major: 252, Minor: 32}},
		},
		{
			name: "two pods of one volume",
			mounts: []string{
				"3200 3150 252:16 / " + pubPath(uidB, "pvc-a") + " rw,relatime - ext4 /dev/rbd0 ro",
				"3201 3151 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime - ext4 /dev/rbd0 ro",
			},
			data: map[string]string{
				dataPath(uidA, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com"),
				dataPath(uidB, "pvc-a"): persistentData("pvc-a", "rbd.csi.ceph.com"),
			},
			want: []Volume{
				{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, Major: 252, Minor: 16},
				{PodUID: uidB, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", ReadOnly: true, Major: 252, Minor: 16},
			},
		},
		{
			name: "volume data without the name falls back to the directory",
			mounts: []string{
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime - ext4 /dev/rbd0 rw",
			},
			data: map[string]string{dataPath(uidA, "pvc-a"): `{"driverName":"rbd.csi.ceph.com"}`},
			want: []Volume{{PodUID: uidA, PersistentVolume: "pvc-a", Driver: "rbd.csi.ceph.com", FSType: "ext4", Major: 252, Minor: 16}},
		},
		{
			name: "missing and broken volume data",
			mounts: []string{
				"3200 3150 252:16 / " + pubPath(uidA, "pvc-a") + " rw,relatime - ext4 /dev/rbd0 ro",
				"3201 3150 252:32 / " + pubPath(uidA, "pvc-c") + " rw,relatime - ext4 /dev/rbd1 ro",
			},
			data:     map[string]string{dataPath(uidA, "pvc-c"): `{"driverName":`},
			wantErrs: 2,
		},
		{
			name: "paths outside the publish directory",
			mounts: []string{
				"3200 3150 252:16 / /var/lib/kubelet/pods/" + uidA + "/volumes/kubernetes.io~csi/pvc-a rw - ext4 /dev/rbd0 ro",
				"3201 3150 252:16 / /var/lib/kubelet/pods/" + uidA + "/volumes/kubernetes.io~csi/pvc-a/mount/sub rw - ext4 /dev/rbd0 ro",
				"3202 3150 252:16 / /var/lib/kubelet/pods/" + uidA + "/volume-subpaths/pvc-a/app/0 rw - ext4 /dev/rbd0 ro",
				"3203 3150 0:91 / /var/lib/kubelet/pods/" + uidA + "/volumes/kubernetes.io~empty-dir/cache/mount rw - tmpfs tmpfs ro",
				"3204 3150 252:16 / /srv/kubelet/pods/" + uidA + "/volumes/kubernetes.io~csi/pvc-a/mount rw - ext4 /dev/rbd0 ro",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := range tt.want {
				tt.want[i].Mountpoint = pubPath(tt.want[i].PodUID, tt.want[i].PersistentVolume)
			}
			got, errs := Scan(parseMounts(t, tt.mounts...), "/var/lib/kubelet/", files(tt.data))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Scan() volumes = %+v; want %+v", got, tt.want)
			}
			if len(errs) != tt.wantErrs {
				t.Errorf("Scan() returned %d errors (%v); want %d", len(errs), errs, tt.wantErrs)
			}
		})
	}
}

func TestParsePublishPath(t *testing.T) {
	tests := []struct {
		path    string
		wantUID string
		wantDir string
		wantOK  bool
	}{
		{path: pubPath(uidA, "pvc-a"), wantUID: uidA, wantDir: "pvc-a", wantOK: true},
		{path: "/var/lib/kubelet/pods/../volumes/kubernetes.io~csi/pvc-a/mount"},
		{path: "/var/lib/kubelet/pods//volumes/kubernetes.io~csi/pvc-a/mount"},
		{path: "/var/lib/kubelet/pods/" + uidA + "/volumes/kubernetes.io~csi//mount"},
		{path: "/var/lib/kubelet/pods-old/" + uidA + "/volumes/kubernetes.io~csi/pvc-a/mount"},
	}
	for _, tt := range tests {
		uid, dir, ok := parsePublishPath("/var/lib/kubelet/pods", tt.path)
		if uid != tt.wantUID || dir != tt.wantDir || ok != tt.wantOK {
			t.Errorf("parsePublishPath(%q) = %q, %q, %v; want %q, %q, %v", tt.path, uid, dir, ok, tt.wantUID, tt.wantDir, tt.wantOK)
		}
	}
}
