package rcloneenv

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Tests of the SMB, FTP, Azure Blob and Google Cloud Storage remotes: the
// definitions the controller's descriptors make are accepted, and the
// credential checks and denied options keep rclone away from the worker
// host's identity.

// gcsKey is a service account key as Google hands it out: several lines
// of JSON. Its private key is a placeholder: nothing here signs with it.
const gcsKey = `{
  "type": "service_account",
  "project_id": "demo-project",
  "private_key_id": "0123456789abcdef",
  "private_key": "-----BEGIN PRIVATE KEY-----\nTOP-SECRET-KEY-MATERIAL\n-----END PRIVATE KEY-----\n",
  "client_email": "backup@demo-project.iam.gserviceaccount.com",
  "token_uri": "https://oauth2.googleapis.com/token"
}`

// ftpWeird is weird without its line break, which FTP credentials cannot
// carry (TestFTPLineBreaksRefused).
const ftpWeird = "a'b\"c,d:e f g=h;i\\j`k$l{m}n#o[p]q\tr"

// TestNewTypeDefinitionsAccepted builds the definitions the descriptors of
// design §3 make (every field they map to rclone) and checks what rclone
// gets: the backend type, every value unchanged, and passwords obscured.
func TestNewTypeDefinitionsAccepted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		def  *syncrv1.RemoteDefinition
		// plain are variables (by option) rclone gets as given; obscured
		// are the ones it gets obscured, with their plain values.
		plain, obscured map[string]string
	}{
		{
			name: "smb",
			def: remote("nas", "smb",
				withOptions("host", "nas.local", "user", "alice", "domain", "HOME", "port", "4450", "spn", "cifs/nas.local", "case_insensitive", "false"),
				withSecrets("pass", weird), withObscure("pass")),
			plain:    map[string]string{"type": "smb", "host": "nas.local", "user": "alice", "domain": "HOME", "port": "4450", "spn": "cifs/nas.local", "case_insensitive": "false"},
			obscured: map[string]string{"pass": weird},
		},
		{
			name:  "smb guest",
			def:   remote("nas", "smb", withOptions("host", "nas.local", "user", "guest")),
			plain: map[string]string{"type": "smb", "user": "guest"},
		},
		{
			name: "ftps",
			def: remote("ftp", "ftp",
				withOptions("host", "ftp.example.com", "user", "u'ser \"q\"", "explicit_tls", "true", "port", "2121",
					"no_check_certificate", "true", "concurrency", "4", "disable_epsv", "true", "disable_mlsd", "true", "writing_mdtm", "true"),
				withSecrets("pass", ftpWeird), withObscure("pass")),
			plain: map[string]string{"type": "ftp", "host": "ftp.example.com", "user": "u'ser \"q\"", "explicit_tls": "true", "port": "2121",
				"no_check_certificate": "true", "concurrency": "4", "disable_epsv": "true", "disable_mlsd": "true", "writing_mdtm": "true"},
			obscured: map[string]string{"pass": ftpWeird},
		},
		{
			// rclone's ftp backend reveals pass even when it is empty, so
			// anonymous FTP sends an obscured empty password.
			name:     "anonymous ftp",
			def:      remote("ftp", "ftp", withOptions("host", "ftp.example.com", "user", "anonymous", "tls", "true"), withSecrets("pass", ""), withObscure("pass")),
			plain:    map[string]string{"type": "ftp", "user": "anonymous", "tls": "true"},
			obscured: map[string]string{"pass": ""},
		},
		{
			name: "azureblob shared key",
			def: remote("az", "azureblob",
				withOptions("account", "demoaccount", "access_tier", "cool", "endpoint", "https://demoaccount.blob.core.usgovcloudapi.net", "no_check_container", "true"),
				withSecrets("key", "c2VjcmV0LWtleQ==")),
			plain: map[string]string{"type": "azureblob", "account": "demoaccount", "key": "c2VjcmV0LWtleQ==", "access_tier": "cool",
				"endpoint": "https://demoaccount.blob.core.usgovcloudapi.net", "no_check_container": "true"},
		},
		{
			name:  "azureblob SAS URL",
			def:   remote("az", "azureblob", withSecrets("sas_url", "https://demoaccount.blob.core.windows.net/backups?sv=2024&sig=a%2Bb&sp=rwdl")),
			plain: map[string]string{"type": "azureblob", "sas_url": "https://demoaccount.blob.core.windows.net/backups?sv=2024&sig=a%2Bb&sp=rwdl"},
		},
		{
			name: "azureblob service principal",
			def: remote("az", "azureblob",
				withOptions("account", "demoaccount", "tenant", "11111111-2222-3333-4444-555555555555", "client_id", "66666666-7777-8888-9999-000000000000"),
				withSecrets("client_secret", weird)),
			plain: map[string]string{"type": "azureblob", "tenant": "11111111-2222-3333-4444-555555555555",
				"client_id": "66666666-7777-8888-9999-000000000000", "client_secret": weird},
		},
		{
			// The key reaches rclone as it was pasted, line breaks and all
			// (rclone parses it; the controller only compacts it).
			name: "gcs",
			def: remote("gcs", "gcs",
				withOptions("project_number", "123456789012", "location", "europe-west1", "storage_class", "NEARLINE", "bucket_policy_only", "true",
					"object_acl", "private", "bucket_acl", "private", "endpoint", "storage.example.org"),
				withSecrets("service_account_credentials", gcsKey)),
			plain: map[string]string{"type": "gcs", "service_account_credentials": gcsKey, "project_number": "123456789012", "location": "europe-west1",
				"storage_class": "NEARLINE", "bucket_policy_only": "true", "object_acl": "private", "bucket_acl": "private", "endpoint": "storage.example.org"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{tt.def})
			if err != nil {
				t.Fatalf("Remotes: %v", err)
			}
			vars, alias := env.Vars(), tt.def.GetAlias()
			for k, want := range tt.plain {
				if got := vars[EnvName(alias, k)]; got != want {
					t.Errorf("%s = %q, want %q", EnvName(alias, k), got, want)
				}
			}
			for k, want := range tt.obscured {
				if got := reveal(t, vars[EnvName(alias, k)]); got != want {
					t.Errorf("%s reveals %q, want %q", EnvName(alias, k), got, want)
				}
			}
			if got, ok := env.BackendType(alias); !ok || got != tt.def.GetBackendType() {
				t.Errorf("BackendType = %q, %v", got, ok)
			}
			for _, v := range tt.def.GetSecrets() {
				if v != "" && !contains(env.Secrets(), v) {
					t.Errorf("Secrets() lacks a secret of the remote")
				}
			}
		})
	}
}

// TestNewTypeCredentials checks the remotes that rclone would complete
// with the worker host's identity: a gcs remote without its own key (the
// host's Google credentials) and an ftp or smb remote without a user (the
// worker's account name). The messages name the type, never a value.
func TestNewTypeCredentials(t *testing.T) {
	t.Parallel()
	const secret = "TOP-SECRET-VALUE"
	tests := []struct {
		name string
		def  *syncrv1.RemoteDefinition
		want string // "" when the definition is accepted
	}{
		{"gcs without a key", remote("gg", "gcs", withOptions("project_number", "1", "location", secret)),
			"needs a service account key; the worker's own Google credentials are never used"},
		{"gcs with an empty key", remote("gg", "gcs", withSecrets("service_account_credentials", "", "client_secret", secret)),
			"needs a service account key"},
		{"gcs with an OAuth token only", remote("gg", "gcs", withSecrets("token", `{"access_token":"`+secret+`"}`)),
			"needs a service account key"},
		{"gcs with an access token only", remote("gg", "gcs", withSecrets("access_token", secret)),
			"needs a service account key"},
		{"gcs anonymous", remote("gg", "gcs", withOptions("anonymous", "true")),
			"needs a service account key"},
		{"gcs key in options", remote("gg", "gcs", withOptions("Service-Account-Credentials", gcsKey)), ""},
		{"gcs key and anonymous", remote("gg", "gcs", withOptions("anonymous", "true"), withSecrets("service_account_credentials", gcsKey)), ""},

		{"ftp without a user", remote("ff", "ftp", withOptions("host", "h"), withSecrets("pass", secret), withObscure("pass")),
			"an ftp remote needs a user; the worker's account name is never sent"},
		{"ftp with an empty user", remote("ff", "ftp", withOptions("host", "h", "user", ""), withSecrets("pass", secret), withObscure("pass")),
			"an ftp remote needs a user"},
		{"ftp with an empty user secret", remote("ff", "ftp", withOptions("host", "h"), withSecrets("user", "", "pass", secret), withObscure("pass")),
			"an ftp remote needs a user"},
		{"ftp user, uppercase key", remote("ff", "ftp", withOptions("host", "h", "USER", "bob")), ""},
		{"ftp user as a secret", remote("ff", "ftp", withOptions("host", "h"), withSecrets("user", "bob")), ""},

		{"smb without a user", remote("ss", "smb", withOptions("host", "h", "domain", secret), withSecrets("pass", secret), withObscure("pass")),
			"an smb remote needs a user; the worker's account name is never sent"},
		{"smb with an empty user", remote("ss", "smb", withOptions("host", "h", "user", "")),
			"an smb remote needs a user"},
		{"smb user", remote("ss", "smb", withOptions("host", "h", "User", "guest")), ""},

		// Azure has no host fallback once its host sign-ins are denied:
		// rclone refuses a remote without credentials or reads anonymously.
		{"azureblob without credentials", remote("aa", "azureblob"), ""},
		{"azureblob with an account only", remote("aa", "azureblob", withOptions("account", "demo")), ""},
		{"azureblob with a partial service principal", remote("aa", "azureblob", withOptions("account", "demo", "tenant", "t", "client_id", "c")), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{tt.def})
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Remotes: %v, want it accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, ErrInvalidRemote) {
				t.Fatalf("Remotes = %v, want an invalid remote error with %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), strconv.Quote(tt.def.GetAlias())) {
				t.Errorf("error %q does not name the alias", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "KEY-MATERIAL") {
				t.Errorf("error message leaks a value: %v", err)
			}
		})
	}
}

// TestNewTypeOptionsDenied checks that otherwise valid remotes of the four
// types are refused with an option that runs a command, reads a local
// file, or reaches the worker host or its identity.
func TestNewTypeOptionsDenied(t *testing.T) {
	t.Parallel()
	smb := func(kv ...string) *syncrv1.RemoteDefinition {
		return remote("ss", "smb", withOptions(append([]string{"host", "h", "user", "u"}, kv...)...))
	}
	ftp := func(kv ...string) *syncrv1.RemoteDefinition {
		return remote("ff", "ftp", withOptions(append([]string{"host", "h", "user", "u"}, kv...)...))
	}
	az := func(kv ...string) *syncrv1.RemoteDefinition {
		return remote("aa", "azureblob", withOptions(append([]string{"account", "demo"}, kv...)...))
	}
	gcs := func(kv ...string) *syncrv1.RemoteDefinition {
		return remote("gg", "gcs", withOptions(kv...), withSecrets("service_account_credentials", gcsKey))
	}
	tests := []struct {
		def          *syncrv1.RemoteDefinition
		key, because string
	}{
		{az("use_emulator", "true"), "use_emulator", "storage emulator on the worker host"},
		{az("Use-Emulator", "true"), "Use-Emulator", "storage emulator on the worker host"},
		{az("tenant", "t", "client_id", "c", "msi_client_id", "m"), "msi_client_id", "managed identity"},
		{az("MSI-Client-ID", "m"), "MSI-Client-ID", "managed identity"},
		{az("use_msi", "true"), "use_msi", "managed identity"},
		{az("use_az", "true"), "use_az", "Azure CLI"},
		{az("env_auth", "true"), "env_auth", "host's credentials"},
		{az("service_principal_file", "/etc/sp.json"), "service_principal_file", "reads a local file"},
		{az("tenant", "t", "client_id", "c", "client_certificate_path", "/etc/cert.pem"), "client_certificate_path", "reads a local file"},
		{gcs("env_auth", "true"), "env_auth", "host's credentials"},
		{gcs("service_account_file", "/root/.config/gcloud/key.json"), "service_account_file", "reads a local file"},
		{smb("use_kerberos", "true"), "use_kerberos", "Kerberos"},
		{smb("kerberos_ccache", "FILE:/tmp/krb5cc_0"), "kerberos_ccache", "reads a local file"},
		{ftp("config_file", "/etc/x"), "config_file", "reads a local file"},
	}
	for _, tt := range tests {
		t.Run(tt.def.GetBackendType()+"/"+tt.key, func(t *testing.T) {
			t.Parallel()
			_, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{tt.def})
			if err == nil || !strings.Contains(err.Error(), tt.because) || !strings.Contains(err.Error(), strconv.Quote(tt.key)) {
				t.Fatalf("Remotes = %v, want an error naming %q and %q", err, tt.key, tt.because)
			}
			if strings.Contains(err.Error(), "/etc/") || strings.Contains(err.Error(), "FILE:") {
				t.Errorf("error message leaks a value: %v", err)
			}
		})
	}
}

// TestFTPLineBreaksRefused checks that no line break reaches a value the
// ftp backend writes into a command line as it is: with one, a remote
// could make the worker send commands of its choosing ("MAIL FROM:...")
// to whatever service its host and port name. Other backends' values,
// and ftp paths (rclone encodes their line breaks), are not concerned.
func TestFTPLineBreaksRefused(t *testing.T) {
	t.Parallel()
	const injected = "joe\r\nMAIL FROM:<TOP-SECRET-VALUE>"
	tests := []struct {
		name string
		def  *syncrv1.RemoteDefinition
		key  string
	}{
		{"user", remote("ff", "ftp", withOptions("host", "h", "user", injected)), "user"},
		{"user, LF only", remote("ff", "ftp", withOptions("host", "h", "User", "joe\nNOOP")), "User"},
		{"user as a secret", remote("ff", "ftp", withOptions("host", "h"), withSecrets("user", "joe\rNOOP")), "user"},
		{"password", remote("ff", "ftp", withOptions("host", "h", "user", "u"), withSecrets("pass", injected), withObscure("pass")), "pass"},
		{"host", remote("ff", "ftp", withOptions("host", "h\r\nX-Injected: 1", "user", "u")), "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{tt.def})
			if err == nil || !strings.Contains(err.Error(), strconv.Quote(tt.key)+" must not contain a line break") {
				t.Fatalf("Remotes = %v, want a line break error naming %q", err, tt.key)
			}
			if strings.Contains(err.Error(), "TOP-SECRET-VALUE") || strings.Contains(err.Error(), "NOOP") || strings.Contains(err.Error(), "Injected") {
				t.Errorf("error message leaks a value: %v", err)
			}
		})
	}
	for name, def := range map[string]*syncrv1.RemoteDefinition{
		"smb user":         remote("ss", "smb", withOptions("host", "h", "user", "u", "spn", "a\nb")),
		"webdav password":  remote("dd", "webdav", withOptions("url", "https://dav.example.com"), withSecrets("pass", "a\nb"), withObscure("pass")),
		"ftp other option": remote("ff", "ftp", withOptions("host", "h", "user", "u", "description", "two\nlines")),
	} {
		if _, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{def}); err != nil {
			t.Errorf("%s: %v, want it accepted", name, err)
		}
	}
}
