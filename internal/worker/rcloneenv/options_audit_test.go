package rcloneenv

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// reviewedOptions lists, for each supported backend, the options of the
// pinned rclone (v1.75.2) that were reviewed and are accepted from the
// controller: they only change how rclone talks to the remote service.
// Every other option of a supported backend is rejected by checkOption
// because it runs a local command, touches local files or sockets, or uses
// the worker host's identity (sftp ssh, key_use_agent and *_file; webdav
// bearer_token_command and unix_socket; s3 env_auth and
// shared_credentials_file; union upstreams).
//
// Judgment calls:
//   - s3 profile only selects a profile of the host's AWS config, which
//     rclone reads only with env_auth (denied).
//   - sftp *sum_command, server_command, subsystem, path_override and
//     set_env apply on the SFTP server, not on the worker.
//   - sftp pin_host_key writes host_keys into the per-job rclone config
//     file, which Isolate creates empty and the runner removes.
//   - sftp ask_password reads the child's stdin (the null device); an
//     sftp remote needs a pass or key_pem anyway (checkCredentials).
//   - sftp socks_proxy and http_proxy, like host and endpoint, only pick
//     where the remote is.
//
// TestSupportedBackendOptionsReviewed fails when the pinned rclone has an
// option that is in neither group, so an rclone upgrade cannot add one
// silently: review it, then list it here or deny it in deniedOptions.
func reviewedOptions() map[string][]string {
	return map[string][]string{
		"b2": strings.Fields(`account key endpoint test_mode versions version_at hard_delete
			upload_cutoff copy_cutoff chunk_size upload_concurrency disable_checksum download_url
			download_auth_duration memory_pool_flush_time memory_pool_use_mmap lifecycle encoding
			sse_customer_algorithm sse_customer_key sse_customer_key_base64 sse_customer_key_md5
			description`),
		"memory": strings.Fields(`discard description`),
		"s3": strings.Fields(`provider access_key_id secret_access_key region endpoint
			location_constraint acl bucket_acl requester_pays server_side_encryption
			sse_customer_algorithm sse_kms_key_id sse_customer_key sse_customer_key_base64
			sse_customer_key_md5 storage_class upload_cutoff chunk_size max_upload_parts copy_cutoff
			disable_checksum profile session_token role_arn role_session_name role_session_duration
			role_external_id upload_concurrency force_path_style v2_auth use_dual_stack
			use_accelerate_endpoint use_arn_region leave_parts_on_error list_chunk list_version
			list_url_encode no_check_bucket no_head no_head_object encoding memory_pool_flush_time
			memory_pool_use_mmap disable_http2 download_url directory_markers use_multipart_etag
			use_unsigned_payload use_presigned_request use_data_integrity_protections versions
			version_at version_deleted decompress might_gzip use_accept_encoding_gzip
			no_system_metadata sts_endpoint use_already_exists use_multipart_uploads
			list_versions_oldest_first use_x_id sign_accept_encoding directory_bucket sdk_log_mode
			ibm_api_key ibm_resource_instance_id ibm_iam_endpoint object_lock_mode
			object_lock_retain_until_date object_lock_legal_hold_status bypass_governance_retention
			bucket_object_lock_enabled object_lock_set_after_upload object_lock_supported
			description`),
		"sftp": strings.Fields(`host user port pass key_pem key_file_pass pubkey pin_host_key
			host_keys use_insecure_cipher disable_hashcheck ask_password path_override encoding
			set_modtime shell_type hashes md5sum_command sha1sum_command crc32sum_command
			sha256sum_command blake3sum_command xxh3sum_command xxh128sum_command skip_links
			subsystem server_command use_fstat disable_concurrent_reads disable_concurrent_writes
			idle_timeout chunk_size concurrency connections set_env ciphers key_exchange macs
			host_key_algorithms socks_proxy http_proxy copy_is_hardlink description`),
		"union": strings.Fields(`action_policy create_policy search_policy cache_time
			min_free_space description`),
		"webdav": strings.Fields(`url vendor user pass bearer_token encoding headers
			pacer_min_sleep nextcloud_chunk_size owncloud_exclude_shares owncloud_exclude_mounts
			auth_redirect description`),
	}
}

func TestReviewedOptionsAccepted(t *testing.T) {
	t.Parallel()
	reviewed := reviewedOptions()
	if got := slices.Sorted(maps.Keys(reviewed)); !slices.Equal(got, SupportedBackends()) {
		t.Errorf("reviewedOptions covers %q, want every supported backend %q", got, SupportedBackends())
	}
	for backend, opts := range reviewed {
		for _, o := range opts {
			if err := checkOption(backend, o); err != nil {
				t.Errorf("reviewed option %s/%s is rejected: %v", backend, o, err)
			}
		}
		if len(slices.Compact(slices.Sorted(slices.Values(opts)))) != len(opts) {
			t.Errorf("reviewedOptions[%q] lists an option twice", backend)
		}
	}
}

// TestSupportedBackendOptionsReviewed checks with the real rclone
// (SYNCR_TEST_RCLONE) that every option of every supported backend has
// been reviewed: it is either denied by checkOption or in reviewedOptions.
func TestSupportedBackendOptionsReviewed(t *testing.T) {
	t.Parallel()
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	out, err := exec.CommandContext(t.Context(), bin, "config", "providers").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("rclone config providers: %v: %s", err, ee.Stderr)
		}
		t.Fatalf("rclone config providers: %v", err)
	}
	var providers []struct {
		Name    string
		Options []struct{ Name string }
	}
	if err := json.Unmarshal(out, &providers); err != nil {
		t.Fatalf("decode rclone config providers: %v", err)
	}
	options := map[string][]string{}
	for _, p := range providers {
		for _, o := range p.Options {
			options[p.Name] = append(options[p.Name], o.Name)
		}
	}

	reviewed := reviewedOptions()
	for _, backend := range SupportedBackends() {
		opts, ok := options[backend]
		if !ok {
			t.Errorf("rclone has no backend %q", backend)
			continue
		}
		for _, o := range opts {
			if checkOption(backend, o) != nil || slices.Contains(reviewed[backend], o) {
				continue
			}
			t.Errorf("rclone option %s/%s has not been reviewed: deny it in deniedOptions if it runs a local command, "+
				"touches local files or sockets, or uses the worker host's identity; otherwise add it to reviewedOptions", backend, o)
		}
		for _, o := range reviewed[backend] {
			if !slices.Contains(opts, o) {
				t.Errorf("reviewed option %s/%s is not an option of the pinned rclone; remove it", backend, o)
			}
		}
	}
}
