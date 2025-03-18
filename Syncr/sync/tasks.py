from huey import crontab
from huey.contrib.djhuey import periodic_task
import requests

from cron_validator import CronValidator
from datetime import datetime
from .models import Job, Task
from .utils import createJobHandler

@periodic_task(crontab(minute='*/1'))
def check_schedules_cron():
    schedules = Task.objects.all()
    dt = datetime.now()
    for schedule in schedules:
        # if CronValidator.parse(task.cron) is not None:
        #     print(f'Valid: {task.cron}')
        if CronValidator.match_datetime(schedule.cron, dt):
            # Run the task
            createJobHandler(type=schedule.type, 
                             srcFs=schedule.srcFs, 
                             dstFs=schedule.dstFs, 
                             user=schedule.user,
                             # Kwargs below
                             task=schedule)
            print(f"Starting schedule {schedule.id} - {schedule.name}")


# This task will check if I job has failed
# It can fail when the rclone server gives up or loses connection
# I'm unsure if this will work properly
@periodic_task(crontab(minute="*/15"))
def check_tasks_for_failure():
    running_jobs = Job.objects.filter(finished=False)
    for job in running_jobs:
        try:
            status = requests.post("http://127.0.0.1:5572/job/status", json={
                "jobid": job.rcloneId
            })
            status.raise_for_status()
        except requests.exceptions.RequestException as e:
            job.finished = True
            job.success = False
            job.error = f"Error contacting server: {str(e)}\nOur system was unable to query the job and was promptly finished"
            job.save()
            print(f"Job {job.rcloneId} failed due to server contact error")
            continue
        
        if status.json().get("error"):
            job.finished = True
            job.success = False
            job.error = status.json().get("error") + "\nOur system was unable to query the job and was promptly finished"
            job.save()
            print(f"Job {job.rcloneId} failed")