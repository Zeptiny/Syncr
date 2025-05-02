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
    - Authentication
    - Server Creation outside of admin
    - Fallback servers
    - Multi selection
    - Network cap

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


# Running this thing on docker
If you REALLY want to trust this while in development, or want to give some feedback, here's how to run it:

1. Clone this repository and go into it

```
git clone https://github.com/Zeptiny/Sync.git && cd Syncr
``` 

1. Edit the .env

Make a copy of the example:
```
cp .env.example .env
```
Edit the `.env` with your prefereed editor (May it be our lord and savior `vim`)


3. Start the docker compose
At this moment, you will need to build the project, the command is as follows:
```
docker compose up -d --build
```
It will take a few seconds/minutes, depending on your system

4. Create the admin user
First, enter into the web container
```
docker exec -it syncr-web sh
```
Next, create the super user:
```
python manage.py createsuperuser
```
Enter the required information, when done, exit with the command `exit` 

5. Login into the interface

Access the Syncr by the IP and port you configured, enter your credentials, you should be logged into the home page.

6. Add the servers

For the app to be usable, you need to add the server from witch will be made the transferences, those being rclone instances.
In the provided docker compose, there already is a rclone instance, however, you can connect from other machines, providers, etc.

Go to the admin interface -> `<ip>:<port>/admin` (Yes, I will make a form, I sort of forgot)
Add a server: servers -> add

Fill the form with the rclone information, rclone authentication isn't yet inplemented.
If you are using the default provided docker compose, the rclone host can be `syncr-rclone`