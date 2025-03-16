TASK_TYPES = {
    "sync/copy": "copy",
}

# FTP is disabled, as rclone obscure appears to not be working, or the proccess hangs
REMOTE_TYPES = {
    "s3": "S3",
    # "ftp": "FTP",
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
    # "ftp": [
    #     "user",
    #     "pass",
    #     "host",
    #     "port",
    # ]
}