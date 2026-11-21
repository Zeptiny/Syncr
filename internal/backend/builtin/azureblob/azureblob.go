// Package azureblob is the backend descriptor of Azure Blob Storage
// (rclone backend "azureblob").
//
// A remote signs in exactly one way: with the account's shared key, with a
// SAS URL, or as a service principal (tenant, client ID and client
// secret). Nothing else is offered: rclone's managed identity, Azure CLI,
// environment and certificate sign-ins would use the worker host's
// identity or files (the worker refuses them), and anonymous access to
// public containers is not supported. ToRclone never emits msi_client_id
// (with a client ID and tenant, rclone 1.75.2 then asks the host's managed
// identity endpoint) or connection_string.
package azureblob

import (
	"regexp"
	"strings"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Type is the azureblob backend type.
type Type struct{}

var (
	_ backend.Exampler        = Type{}
	_ backend.InvalidExampler = Type{}
)

// ID implements backend.Type.
func (Type) ID() string { return "azureblob" }

// Label implements backend.Type.
func (Type) Label() string { return "Azure Blob Storage" }

// Description implements backend.Type.
func (Type) Description() string {
	return "Microsoft Azure Blob Storage, with the account key, a SAS URL or a service principal."
}

// Fields implements backend.Type.
func (Type) Fields() []backend.Field {
	return []backend.Field{
		{Name: "account", Label: "Storage account", Kind: backend.KindString, Placeholder: "mystorageaccount",
			Help: "The storage account name: 3 to 24 lowercase letters and digits. Required unless you sign in with a SAS URL."},
		{Name: "key", Label: "Account key", Kind: backend.KindString, Secret: true,
			Help: "One of the storage account's access keys (shared key)."},
		{Name: "sas_url", Label: "SAS URL", Kind: backend.KindString, Secret: true,
			Help: "A SAS URL for the account or a container, instead of the account key."},
		{Name: "container", Label: "Container", Kind: backend.KindString, Rclone: backend.NotRclone,
			Help: "Container, optionally with a path prefix (container/prefix). Leave empty to name the container in each job's path."},
		{Name: "tenant", Label: "Tenant ID", Kind: backend.KindString, Advanced: true,
			Help: "To sign in as a service principal (an app registration) instead: its tenant, client ID and client secret."},
		{Name: "client_id", Label: "Client ID", Kind: backend.KindString, Advanced: true},
		{Name: "client_secret", Label: "Client secret", Kind: backend.KindString, Secret: true, Advanced: true},
		{Name: "access_tier", Label: "Access tier", Kind: backend.KindEnum, Advanced: true,
			Help: "The tier of uploaded blobs; empty keeps the account's default. Archive blobs cannot be read until they are rehydrated in Azure: not for a remote that holds a restic repository.",
			Choices: []backend.Choice{
				{Value: "hot", Label: "Hot"}, {Value: "cool", Label: "Cool"},
				{Value: "cold", Label: "Cold"}, {Value: "archive", Label: "Archive"},
			}},
		{Name: "endpoint", Label: "Endpoint", Kind: backend.KindString, Advanced: true,
			Placeholder: "https://mystorageaccount.blob.core.chinacloudapi.cn",
			Help:        "The blob service URL, only for other Azure clouds."},
		{Name: "no_check_container", Label: "Don't check or create the container", Kind: backend.KindBool, Advanced: true},
	}
}

// Capabilities implements backend.Type: Azure keeps MD5 checksums.
func (Type) Capabilities() backend.Capabilities {
	return backend.Capabilities{UnionMember: true, ResticRepository: true, ObjectStore: true}
}

// servicePrincipal are the fields of a service principal sign-in, in form
// order, with their field paths.
func servicePrincipal() [][2]string {
	return [][2]string{{"tenant", "options.tenant"}, {"client_id", "options.client_id"}, {"client_secret", "secrets.client_secret"}}
}

// accountRe is Azure's rule for storage account names: 3 to 24 lowercase
// letters and digits. rclone builds the service URL from the name
// (https://<account>.blob.core.windows.net), so nothing else may reach it.
var accountRe = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

// Validate implements backend.Type: the field checks, exactly one sign-in
// method (the account key, a SAS URL, or all three fields of a service
// principal, whose missing fields are named), an account unless the
// remote signs in with a SAS URL, an account name as Azure defines it, and
// a well-formed container path.
func (t Type) Validate(cfg backend.Config) error {
	var v domain.Validation
	v.AddAll("", backend.CheckFields(t.Fields(), cfg))
	key, sas := cfg.Secrets["key"] != "", cfg.Secrets["sas_url"] != ""
	var spSet, spMissing []string
	for _, f := range servicePrincipal() {
		if cfg.Options[f[0]] != "" || cfg.Secrets[f[0]] != "" {
			spSet = append(spSet, f[1])
		} else {
			spMissing = append(spMissing, f[1])
		}
	}
	sp := len(spSet) > 0
	methods := 0
	for _, set := range []bool{key, sas, sp} {
		if set {
			methods++
		}
	}
	switch {
	case methods == 0:
		v.Add("secrets.key", "an account key, a SAS URL or a service principal is required")
	case methods > 1:
		const msg = "use one sign-in method: the account key, a SAS URL or a service principal"
		if sas {
			v.Add("secrets.sas_url", msg)
		}
		if sp {
			v.Add(spSet[0], msg)
		}
	case sp && len(spMissing) > 0:
		for _, path := range spMissing {
			v.Add(path, "is required to sign in as a service principal (tenant, client ID and client secret)")
		}
	}
	switch account := cfg.Options["account"]; {
	case account == "" && !sas:
		v.Add("options.account", "is required unless you sign in with a SAS URL")
	case account != "" && !accountRe.MatchString(account):
		v.Add("options.account", "must be 3 to 24 lowercase letters and digits, as Azure names storage accounts")
	}
	if _, err := backend.JoinRemote(cfg.Options["container"], ""); err != nil {
		v.Add("options.container", err.Error())
	}
	return v.Err()
}

// ToRclone implements backend.Type; the container (and prefix) becomes the
// Root. Only the declared fields are emitted (see the package
// documentation).
func (t Type) ToRclone(cfg backend.Config) (backend.Rclone, error) {
	r := backend.MapFields(t.Fields(), cfg)
	r.Backend = "azureblob"
	r.Root = strings.Trim(cfg.Options["container"], "/")
	return r, nil
}

// Examples implements backend.Exampler: one per sign-in method.
func (Type) Examples() []backend.Config {
	return []backend.Config{
		{
			Options: map[string]string{"account": "examplestorage", "container": "backups/syncr", "access_tier": "cool"},
			Secrets: map[string]string{"key": "ZXhhbXBsZS1hY2NvdW50LWtleQ=="},
		},
		{
			Options: map[string]string{"container": "backups", "no_check_container": "true"},
			Secrets: map[string]string{"sas_url": "https://examplestorage.blob.core.windows.net/backups?sv=2024-01-01&sig=example"},
		},
		{
			Options: map[string]string{
				"account": "examplestorage", "tenant": "00000000-0000-0000-0000-000000000001",
				"client_id": "00000000-0000-0000-0000-000000000002", "access_tier": "archive",
				"endpoint": "https://examplestorage.blob.core.chinacloudapi.cn",
			},
			Secrets: map[string]string{"client_secret": "example-client-secret"},
		},
	}
}

// InvalidExamples implements backend.InvalidExampler: no sign-in method,
// two of them, a partial service principal and a missing account.
func (Type) InvalidExamples() []backend.InvalidExample {
	return []backend.InvalidExample{
		{Name: "no sign-in method", Config: backend.Config{Options: map[string]string{"account": "examplestorage"}}, Field: "secrets.key"},
		{Name: "key and SAS URL", Config: backend.Config{
			Options: map[string]string{"account": "examplestorage"},
			Secrets: map[string]string{"key": "account-key-value", "sas_url": "https://examplestorage.blob.core.windows.net/?sig=x"},
		}, Field: "secrets.sas_url"},
		{Name: "key and service principal", Config: backend.Config{
			Options: map[string]string{"account": "examplestorage", "tenant": "tenant-id-value"},
			Secrets: map[string]string{"key": "account-key-value"},
		}, Field: "options.tenant"},
		{Name: "service principal without a secret", Config: backend.Config{
			Options: map[string]string{"account": "examplestorage", "tenant": "tenant-id-value", "client_id": "client-id-value"},
		}, Field: "secrets.client_secret"},
		{Name: "service principal with only a secret", Config: backend.Config{
			Options: map[string]string{"account": "examplestorage"},
			Secrets: map[string]string{"client_secret": "client-secret-value"},
		}, Field: "options.client_id"},
		{Name: "key without an account", Config: backend.Config{
			Secrets: map[string]string{"key": "account-key-value"},
		}, Field: "options.account"},
		{Name: "account with a host", Config: backend.Config{
			Options: map[string]string{"account": "127.0.0.1:8080/x?"},
			Secrets: map[string]string{"key": "account-key-value"},
		}, Field: "options.account"},
		{Name: "account with capitals", Config: backend.Config{
			Options: map[string]string{"account": "ExampleStorage"},
			Secrets: map[string]string{"key": "account-key-value"},
		}, Field: "options.account"},
		{Name: "dot-dot in the container", Config: backend.Config{
			Options: map[string]string{"account": "examplestorage", "container": "a/../b"},
			Secrets: map[string]string{"key": "account-key-value"},
		}, Field: "options.container"},
	}
}
