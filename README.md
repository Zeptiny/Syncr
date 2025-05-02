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
### Notifications:
- Email notifications
- SMS
- Slack (Unsure how to use it)

### Protocols:
- Add more protocols (Only S3 and SFTP is supported)
- Add key support for SFTP

### Sincronization:
- Add retry support

### Servers:
- Authentication
- Server Creation outside of admin
- Fallback servers
- Multi selection
- Network cap

### Running:
- Docs

### Github Workflows
- Make them work?

### Accounts
- Option to disable registration

### Ideas (Unknown Feasibility)
- Distribute jobs across multiple servers to improve performance
- Suggest optimal rclone settings based on job analysis


## Other & technical information
- The scheduled jobs are checked every minute with cron_validator using huey if they should be executed, schedules cannot be less than 1 minute.
- Credentials are never saved to the rclone configuration of any server, they are always maintained on the django database.
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