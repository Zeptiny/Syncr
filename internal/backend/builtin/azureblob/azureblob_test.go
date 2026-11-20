package azureblob_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/backendtest"
	"github.com/Zeptiny/Syncr/internal/backend/builtin/azureblob"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestContract(t *testing.T) {
	t.Parallel()
	backendtest.Run(t, azureblob.Type{})
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	want := backend.Capabilities{UnionMember: true, ResticRepository: true, ObjectStore: true}
	if got := (azureblob.Type{}).Capabilities(); got != want {
		t.Errorf("Capabilities = %+v, want %+v", got, want)
	}
}

// TestNeverHostIdentity: no field maps to an option that signs in with
// the worker host's identity or files, or to connection_string.
func TestNeverHostIdentity(t *testing.T) {
	t.Parallel()
	for _, f := range (azureblob.Type{}).Fields() {
		switch n := f.RcloneName(); n {
		case "msi_client_id", "msi_object_id", "msi_mi_res_id", "use_msi", "use_az", "env_auth", "use_emulator",
			"connection_string", "client_certificate_path", "client_certificate_password", "service_principal_file", "username", "password":
			t.Errorf("field %q maps to rclone option %q", f.Name, n)
		}
	}
}

func TestValidateSignIn(t *testing.T) {
	t.Parallel()
	const (
		key    = "account-key-secret-value"
		sas    = "https://acct.blob.core.windows.net/c?sig=sas-secret-value"
		secret = "client-secret-value"
	)
	for _, tt := range []struct {
		name    string
		opts    map[string]string
		secrets map[string]string
		fields  []string // nil for valid
	}{
		{"shared key", map[string]string{"account": "acct"}, map[string]string{"key": key}, nil},
		{"SAS URL without an account", nil, map[string]string{"sas_url": sas}, nil},
		{"SAS URL with an account", map[string]string{"account": "acct"}, map[string]string{"sas_url": sas}, nil},
		{"service principal", map[string]string{"account": "acct", "tenant": "t", "client_id": "c"}, map[string]string{"client_secret": secret}, nil},
		{"nothing", map[string]string{"account": "acct"}, nil, []string{"secrets.key"}},
		{"nothing at all", nil, nil, []string{"secrets.key", "options.account"}},
		{"key without an account", nil, map[string]string{"key": key}, []string{"options.account"}},
		{"service principal without an account", map[string]string{"tenant": "t", "client_id": "c"}, map[string]string{"client_secret": secret},
			[]string{"options.account"}},
		{"key and SAS URL", map[string]string{"account": "acct"}, map[string]string{"key": key, "sas_url": sas}, []string{"secrets.sas_url"}},
		{"key and a client ID", map[string]string{"account": "acct", "client_id": "c"}, map[string]string{"key": key}, []string{"options.client_id"}},
		{"all three", map[string]string{"account": "acct", "tenant": "t", "client_id": "c"},
			map[string]string{"key": key, "sas_url": sas, "client_secret": secret}, []string{"secrets.sas_url", "options.tenant"}},
		{"tenant only", map[string]string{"account": "acct", "tenant": "t"}, nil, []string{"options.client_id", "secrets.client_secret"}},
		{"tenant and client ID: no secret", map[string]string{"account": "acct", "tenant": "t", "client_id": "c"}, nil,
			[]string{"secrets.client_secret"}},
		{"secret only", map[string]string{"account": "acct"}, map[string]string{"client_secret": secret}, []string{"options.tenant", "options.client_id"}},
	} {
		err := azureblob.Type{}.Validate(backend.Config{Options: tt.opts, Secrets: tt.secrets})
		if tt.fields == nil {
			if err != nil {
				t.Errorf("%s: Validate = %v", tt.name, err)
			}
			continue
		}
		var verr *domain.ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: Validate = %v, want a validation error", tt.name, err)
			continue
		}
		var got []string
		for _, f := range verr.Fields {
			got = append(got, f.Field)
		}
		if !slices.Equal(got, tt.fields) {
			t.Errorf("%s: fields %v, want %v (%v)", tt.name, got, tt.fields, err)
		}
		for _, v := range []string{key, "sas-secret-value", secret} {
			if strings.Contains(err.Error(), v) {
				t.Errorf("%s: the error contains a secret: %v", tt.name, err)
			}
		}
	}
}

func TestToRclone(t *testing.T) {
	t.Parallel()
	got, err := azureblob.Type{}.ToRclone(backend.Config{
		Options: map[string]string{"account": "acct", "container": "/c/prefix/", "tenant": "t", "client_id": "c", "access_tier": "cold"},
		Secrets: map[string]string{"client_secret": "s"},
	})
	want := backend.Rclone{Backend: "azureblob", Root: "c/prefix",
		Options: map[string]string{"account": "acct", "tenant": "t", "client_id": "c", "access_tier": "cold"},
		Secrets: map[string]string{"client_secret": "s"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ToRclone = %+v, %v; want %+v", got, err, want)
	}
}
