TASK_TYPES = {
    "sync/copy": "copy",
    "sync/sync": "sync",
}

# SFTP is hanging up, no error, just freeze
# More testing is needed
REMOTE_TYPES = {
    "s3": "S3",
    "sftp": "SFTP",
}

REMOTE_FIELDS = {
    "s3": [
        "access_key_id",
        "secret_access_key",
        "bucket",
        "provider",
        "region",
        "endpoint",
    ],
    "sftp": [
        "user",
        "pass",
        "host",
        "port",
    ]
}