package rcloneenv

import (
	"maps"
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
// shared_credentials_file; azureblob env_auth, use_msi, msi_client_id,
// use_az, use_emulator, client_certificate_path and
// service_principal_file; gcs env_auth and service_account_file; smb
// use_kerberos and kerberos_ccache; union upstreams), or because the worker
// sets it itself (crypt remote and strict_names) or it would hide a wrong
// password (crypt pass_bad_blocks and no_data_encryption).
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
//   - crypt: none of the accepted options names a file, a command or a
//     credential source; they only change how names and data are
//     encrypted on the way to the wrapped remote, whose own options were
//     reviewed with its type. password and password2 must arrive as
//     obscured secrets (addRemote). show_mapping logs each decrypted name
//     with its encrypted form (a NOTICE): names are not secrets, and the
//     log is the job's own output. server_side_across_configs (deprecated)
//     only lets rclone copy server-side between two remotes of the job.
//     The controller emits only the first six. no_data_encryption is
//     denied: file contents would be read without decrypting, so with
//     obfuscated or unencrypted names nothing would show a wrong password
//     (the runners check it by decrypting a file's first block).
//     Checked against rclone 1.75.2's "config providers" help and
//     backend/crypt (Options, NewFs), which reads no file and starts no
//     process: "remote" is opened with fs.NewFs like any other remote.
//   - azureblob: rclone 1.75.2 picks the first sign-in that its options
//     complete (backend/azureblob/auth NewClient): env_auth, use_emulator,
//     account with key, sas_url, connection_string, a service principal
//     (secret, certificate file, or user name and password), the service
//     principal file, use_msi, client_id with tenant and msi_client_id
//     ("workload identity", which asks the host's managed identity
//     endpoint for a token), use_az, and else anonymous access to the
//     account's endpoint. Every branch that uses the host or a local file
//     is denied; what remains signs in with the user's own secrets or
//     anonymously. connection_string carries the user's own AccountKey or
//     SAS and an endpoint, like key, sas_url and endpoint (the azblob
//     v1.8.1 parser has no UseDevelopmentStorage shortcut: such a string
//     fails). username and password are an Entra user's own sign-in.
//     client_certificate_password and client_send_certificate_chain only
//     apply with client_certificate_path, msi_object_id and msi_mi_res_id
//     only with use_msi (all denied): with them alone rclone reads
//     anonymously (checked against a local endpoint: no Authorization
//     header, and no token request). disable_instance_discovery
//     only skips a metadata request to the sign-in service. public_access
//     sets the access level of containers rclone creates on the user's
//     account. The controller emits account, key, sas_url, tenant,
//     client_id, client_secret, access_tier, endpoint and
//     no_check_container.
//   - gcs (rclone's "google cloud storage"): rclone uses anonymous first
//     (no token request at all), then service_account_credentials, which
//     checkCredentials requires, so token, access_token and the OAuth
//     client options (client_id, client_secret, auth_url, token_url,
//     client_credentials) are never used: they only matter to the last
//     branch, which without a token falls back to the host's Google
//     Application Default Credentials. The key's own token_uri is where
//     rclone posts the signed assertion; the controller pins it to
//     Google's, and the worker refuses any other (checkGCSKey).
//     user_project only bills requests to another project.
//   - ftp: ask_password with an empty pass reads a line from the child's
//     stdin (the null device for jobs: rclone then exits); it reaches
//     nothing of the host's, and the controller never emits it.
//     socks_proxy and http_proxy, like host, only pick where the remote
//     is. no_check_certificate and allow_insecure_tls_ciphers weaken TLS
//     to the user's own server. The user is required (checkCredentials):
//     rclone would send the worker's account name.
//   - smb: rclone signs in with NTLM in-process (go-smb2), with the
//     remote's own user and pass; use_kerberos and kerberos_ccache are
//     denied. spn is a name presented to the server. The user is required
//     (checkCredentials): rclone would send the worker's account name.
//
// TestSupportedBackendOptionsReviewed fails when the pinned rclone has an
// option that is in neither group, so an rclone upgrade cannot add one
// silently: review it, then list it here or deny it in deniedOptions.
func reviewedOptions() map[string][]string {
	return map[string][]string{
		"azureblob": strings.Fields(`account key sas_url connection_string tenant client_id
			client_secret client_certificate_password client_send_certificate_chain username
			password disable_instance_discovery msi_object_id msi_mi_res_id endpoint upload_cutoff
			chunk_size upload_concurrency copy_cutoff copy_concurrency copy_total_concurrency
			use_copy_blob list_chunk use_arrow_list list_parallelism access_tier archive_tier_delete
			disable_checksum memory_pool_flush_time memory_pool_use_mmap encoding public_access
			directory_markers no_check_container no_head_object delete_snapshots decompress
			description`),
		"b2": strings.Fields(`account key endpoint test_mode versions version_at hard_delete
			upload_cutoff copy_cutoff chunk_size upload_concurrency disable_checksum download_url
			download_auth_duration memory_pool_flush_time memory_pool_use_mmap lifecycle encoding
			sse_customer_algorithm sse_customer_key sse_customer_key_base64 sse_customer_key_md5
			description`),
		"crypt": strings.Fields(`filename_encryption directory_name_encryption password password2
			filename_encoding suffix description show_mapping server_side_across_configs`),
		"ftp": strings.Fields(`host user port pass tls explicit_tls concurrency
			no_check_certificate disable_epsv disable_mlsd disable_utf8 writing_mdtm
			force_list_hidden idle_timeout close_timeout tls_cache_size disable_tls13
			allow_insecure_tls_ciphers shut_timeout ask_password socks_proxy http_proxy
			no_check_upload encoding description`),
		"gcs": strings.Fields(`client_id client_secret token auth_url token_url
			client_credentials project_number user_project service_account_credentials
			access_token anonymous object_acl bucket_acl bucket_policy_only location storage_class
			directory_markers no_check_bucket decompress endpoint encoding description`),
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
		"smb": strings.Fields(`host user port pass domain spn idle_timeout hide_special_share
			case_insensitive encoding description`),
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
// Backends are looked up by rclone's name and by its prefix (the type a
// config uses: "gcs" is "google cloud storage").
func TestSupportedBackendOptionsReviewed(t *testing.T) {
	t.Parallel()
	backends := RcloneBackends(t)
	reviewed := reviewedOptions()
	for _, backend := range SupportedBackends() {
		b, ok := backends[backend]
		if !ok {
			t.Errorf("rclone has no backend %q", backend)
			continue
		}
		opts := b.OptionNames()
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
