# Syncr - Remote sincronization with rclone

THIS PROJECT IS NOT FINISHED AND DOES NOT REPRESENT IT'S FINAL STATE, THINGS WILL BREAK, PLEASE DO NOT USE IT.
DOCKER IMAGES WILL BE PROVIDED ONCE IT'S STABLE ENOUGH

## TO-DO
- Notifications:
    - Email notifications
    - SMS
    - Slack (Unsure how to use it)

- Protocols:
    - Add more protocols (Only S3 and SFTP is supported)
    - Add key support for SFTP

- Sincronization:
    - Add retries

- Servers:
    - Fallback servers
    - Multi selection

- Running:
    - Docs
    - Docker 

- Workflows
    - Add actual testing

## Things that I may add, but I'm unsure how and if they are even fisable:
- Split the work between multiple servers to improve effiency
- Analyze the jobs and provide recommendations for rclone configs that may improve effiency

## Description
This is a sincronization app made with Django and using Rclone to transfer the files.

Jobs can be scheduled with a crontab, with the minimum frequency of 1 minute.

It works by sending the remote configuration and creating a on-the-fly remote with rclone, the remotes are not saved on the rclone config, nothing is.

Each rclone instance is considered a "server", making possible to have multiple instances and be select on a per job or schedule basis


## Currently supported
### Protocols:
- S3
- SFTP

### Rclone operations
- Sync
- Copy
- Move

### Notification medium
- Discord Webhook