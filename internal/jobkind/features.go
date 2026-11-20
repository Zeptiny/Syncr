package jobkind

import (
	"slices"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
)

// backendFeatures maps an rclone backend type to the link feature a worker
// must report to accept a RemoteDefinition of it: an older worker refuses
// the backend. Backends not listed need none. This is the one place that
// knows it: a new backend type that older workers refuse adds its line
// here, and the dispatcher (jobs) and the probes' worker pick follow.
func backendFeatures() map[string]string {
	return map[string]string{
		"crypt":     link.FeatureRcloneCrypt,
		"smb":       link.FeatureRcloneSMB,
		"ftp":       link.FeatureRcloneFTP,
		"azureblob": link.FeatureRcloneAzureBlob,
		"gcs":       link.FeatureRcloneGCS,
	}
}

// BackendFeature returns the link feature a worker must report to accept
// a RemoteDefinition of rclone backend backendType, or "" when it needs
// none.
func BackendFeature(backendType string) string { return backendFeatures()[backendType] }

// RemoteFeatures returns the link features a worker must report to accept
// the remote definitions defs (a spec's or a probe's remotes), sorted and
// without duplicates; nil when they need none.
func RemoteFeatures(defs []*syncrv1.RemoteDefinition) []string {
	var out []string
	for _, d := range defs {
		if f := BackendFeature(d.GetBackendType()); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}
