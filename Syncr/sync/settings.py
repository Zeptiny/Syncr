TASK_TYPES = {
    "sync/copy": {
        'display': "Copy",
        'description': "Copy files from source to destination",
    },
    "sync/sync": {
        'display': "Sync",
        'description': "Sync files from source to destination",
    },
    "sync/move": {
        'display': "Move",
        'description': "Move files from source to destination",
    },
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