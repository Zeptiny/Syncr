# Syncr - Remote sincronization with rclone

THIS PROJECT IS NOT FINISHED AND DOES NOT REPRESENT IT'S FINAL STATE, THINGS WILL BREAK, PLEASE DO NOT USE IT.
DOCKER IMAGES WILL BE PROVIDED ONCE IT'S STABLE ENOUGH

## Overview
**Syncr** is a Django-based app that uses [Rclone](https://rclone.org/) to handle file synchronization tasks between remote storage services. It allows you to:

- Manage multiple Rclone servers
- Schedule or manually run sync jobs
- Receive notifications (currently via Discord)
- Use temporary, on-the-fly remotes with zero persistence


## Features

- **Rclone operations**: `sync`, `copy`, `move`
- **Remote configuration on the fly**
- **Rclone Unions support**
- **Job scheduling via crontab**
- **Protocol support**: S3 and SFTP
- **Notifications**: Discord Webhooks
- **Multi-server support**: allows to have multiple remote rclone instances.


## TO-DO

### Slave Server - Priority
Make a "slave" server, that will be responsible to communicate with a local rclone and restic instance.
This will allow better control, as well monitoring and simplified communication.
The "Control" server, this repository, will communicate with the slaves.

### Notifications:
- Email notifications
- SMS
- Slack (Unsure how to use it)

### Protocols:
- Add more protocols (Only S3 and SFTP is supported)
- Add key support for SFTP

### Sincronization:
- Add retry support
- Warn if no data changed in x time or x transfers
- Add Restic support

### Servers:
- Authentication
- Server Creation outside of admin
- Fallback servers
- Multi selection
- Network cap

### Remotes/Unions
- Labels/tags
- Groups (For better visualization)
- Multi bucket support for S3 remotes
- Per remote path for union (At the time, it only accepts root)
- Add Union policy options

### Running:
- Docs

### Github Workflows
- Make them work?

### Known Bugs
- S3 remotes with KMS encryption fails checksum
- Sometimes the bandwidth can go negative, specially in jobs that are running for longer periods

## Screenshots
### Home page
![Home Page](./screenshots/home.png)

### Job Creation
![Job Creation](./screenshots/job_creation.png)

### Job Detail
![Job Detail](./screenshots/job_detail.png)

### Job Search
![Job Search](./screenshots/job_search.png)

## Other & technical information
- The scheduled jobs are checked every minute with cron_validator, using huey, if they should be executed, schedules cannot be less than 1 minute.
- Huey uses redis as the backend storage
- Remote credentials are never saved to the rclone configuration of any server, they are always maintained on the django database.
- There is support for multiple users, as well as a registration page.
- Jobs are executed sending the information to the rclone remote API, the remotes/unions are then created on-the-fly.

# Installation & Setup
If you REALLY want to trust this while in development, or want to give some feedback, here's how to run it:

1. Clone this repository

```
git clone https://github.com/Zeptiny/Syncr.git && cd Syncr
``` 

2. Configure the environment

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

4. Create the superuser
```
docker exec -it syncr-web sh
python manage.py createsuperuser
exit
```
Enter the required information.

5. Access the interface

Access your browser and go to `http://127.0.0.1:8000`

6. Add the servers

For the app to be usable, you need to add the server from where the transfers will be done, those being rclone instances.
In the provided docker compose, there already is a rclone instance, you can also add how many you want.

Go to the admin interface -> `http://127.0.0.1:8000/admin` (Yes, I will make a form, I sort of forgot)
Add a server: servers -> add

Rclone authentication isn't yet implemented, if you are using the default provided docker compose, use `syncr-rclone` as the host.

7. Add the Remotes

For the sincronization to work, you will need to specify the remotes from witch syncr can access.
Returning to the home screen, click on "Remotes" and then "Create New Remote".
Fill the form for each remote you want to use.

8. Running your first Job

Return to the home screen, and then click on "Run Manual Job"
You will be able to select the sync type, remote, path, and other options
When running the job, you will be redirected to the job description with it's information.


9. Scheduling job

On the navbar, select "Schedules" and then "Create New Schedule"
The creation is similar to the manual job creation, except for the need of a cron frequency to be added, that's when your job will be run.


10. Done!

That's it, you can also specify a contact list to be notified when a job goes wrong, currently, only Discord Webhook is working.

### Using TLS 

You can also use your own domain by using the `.env.tls.example` and `docker-compose-tls.yaml` files.
Fill the information as normal, also, do not forget to create the docker network with `docker network create proxy` and to change the permissions of the traefik directory with `sudo chown -R $USER:$USER traefik`, TLS should be handled automatically with Let's Encrypt.
