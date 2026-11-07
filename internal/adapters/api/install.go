package api

// DefaultDownloadBaseURL is where install commands download worker
// releases from unless InstallConfig.DownloadBaseURL is set.
const DefaultDownloadBaseURL = "https://github.com/Zeptiny/Syncr/releases/latest/download"

// InstallConfig configures the worker install commands of
// WorkerAdminService (CreateEnrollmentToken, GetPassiveInstallCommands),
// rendered by this file.
//
// m3: pending track C: the commands are rendered once track C implements
// them; until then the responses carry none.
type InstallConfig struct {
	// PublicURL is the controller URL workers use (http.public_url). If
	// empty, CreateEnrollmentToken uses the request's controller_url (the
	// browser's origin), which must be https or a loopback http URL.
	PublicURL string
	// DownloadBaseURL is the base URL of the release assets
	// (workers.download_base_url); "" means DefaultDownloadBaseURL.
	DownloadBaseURL string
}
