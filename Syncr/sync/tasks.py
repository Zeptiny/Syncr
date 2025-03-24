from huey import crontab
from huey.contrib.djhuey import periodic_task
import requests
from django.utils import timezone
from django.contrib.auth.models import User
from django.db.models import Sum

from cron_validator import CronValidator
from datetime import datetime, timedelta
from .models import Job, Schedule, DailyStatistics
from .utils import createJobHandler

@periodic_task(crontab(minute='*/1'))
def check_schedules_cron():
    schedules = Schedule.objects.all()
    dt = datetime.now()
    for schedule in schedules:
        # if CronValidator.parse(task.cron) is not None:
        #     print(f'Valid: {task.cron}')
        if CronValidator.match_datetime(schedule.cron, dt):
            # Run the task
            createJobHandler(type=schedule.type, 
                             srcFs=schedule.srcFs, 
                             srcFsPath=schedule.srcFsPath,
                             dstFs=schedule.dstFs, 
                             dstFsPath=schedule.dstFsPath,
                             server=schedule.server,
                             user=schedule.user,
                             # Kwargs below
                             schedule=schedule)
            print(f"Starting schedule {schedule.id} - {schedule.name}")


# This task will check if I job has failed
# It can fail when the rclone server gives up or loses connection
# I'm unsure if this will work properly
@periodic_task(crontab(minute="*/15"))
def check_tasks_for_failure():
    running_jobs = Job.objects.filter(finished=False)
    for job in running_jobs:
        try:
            status = requests.post(f"http://{job.server.host}:{job.server.port}/job/status", json={
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
            
            
# Collect user usage
# we are starting 5 minutes after midnight to ensure that the task won't try to get data from the wrong day (Yesterday yesterday)
# We check endTime to ensure that we won't get jobs that arent finished but started the day before checking (They would need to run for more than 24 hours)
@periodic_task(crontab(hour=0, minute=5))
def collect_daily_statistics():
    yesterday = timezone.now().date() - timedelta(days=1)
    users = User.objects.all()

    for user in users:
        jobs_yesterday = Job.objects.filter(user=user, endTime__date=yesterday, finished=True)
        
        bandwidth = jobs_yesterday.aggregate(
            bytes=Sum('bytes'),
            serverSideCopyBytes=Sum('serverSideCopyBytes'),
            serverSideMoveBytes=Sum('serverSideMoveBytes')
        )
        
        bytes = bandwidth['bytes'] or 0
        serverSideCopyBytes = bandwidth['serverSideCopyBytes'] or 0
        serverSideMoveBytes = bandwidth['serverSideMoveBytes'] or 0
        
        jobs_run = jobs_yesterday.count()
        errored_jobs = jobs_yesterday.filter(success=False).count()

        DailyStatistics.objects.create(
            user=user,
            date=yesterday,
            bytes=bytes,
            serverSideCopyBytes = serverSideCopyBytes,
            serverSideMoveBytes = serverSideMoveBytes,
            jobs_run=jobs_run,
            errored_jobs=errored_jobs
        )