TASK_TYPES = {
    "sync/copy": "copy",
}


REMOTE_TYPES = {
    "s3": "S3",
    "ftp": "FTP",
}

REMOTE_FIELDS = {
    "s3": [
        "access_key",
        "secret_key",
        "bucket",
        "provider",
        "region",
        "endpoint",
    ],
    "ftp": [
        "username",
        "password",
        "host",
        "port",
    ]
}