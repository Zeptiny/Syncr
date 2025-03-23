from django.db import models
from django.contrib.auth.models import User
from django.utils import timezone

from .settings import TASK_TYPES, REMOTE_TYPES

from servers.models import Server

class Remote(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    
    name = models.CharField(max_length=127)
    type = models.CharField(choices=REMOTE_TYPES.items(), max_length=127) # There is a list of allowed remotes
    # I'm unsure what would be the best way to store the config, however, it is what it is
    config = models.JSONField() # Examples configs can be seen on utils.py
    
    def __str__(self):
        return f"{self.type} | {self.name}"

class Schedule(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    name = models.CharField(max_length=127)
    
    cron = models.CharField(max_length=127) # A string that represents the cron frequency
    
    type = models.CharField(choices=TASK_TYPES.items(), max_length=127, default="sync/copy")
    
    srcFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='schedule_srcfFs')
    dstFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='schedule_dstfFs')
    
    server = models.ForeignKey(Server, on_delete=models.SET_NULL, null=True, related_name="schedules") # The server where the jobs on this schedule should execute
    
class Job(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    schedule = models.ForeignKey(Schedule, on_delete=models.CASCADE, null=True, related_name="jobs")# The schedule that called the job, if it's null, it was created manually/forced
    
    type = models.CharField(choices=TASK_TYPES.items(), max_length=127, default="sync/copy")
    
    srcFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='job_srcFs')
    dstFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='job_dstFs') # There are jobs that may not require a destination (Eg. delete)
    
    server = models.ForeignKey(Server, on_delete=models.SET_NULL, null=True, related_name="jobs") # The server on where the job run

    # Status dependent
    # Gathered via rclone rc job/status jobid=<id> --rc-addr=
    startTime = models.DateTimeField(auto_now_add=True)
    duration = models.FloatField()
    endTime = models.DateTimeField(null=True)
    error = models.TextField(max_length=1023)
    finished = models.BooleanField(default=False)
    group = models.CharField(max_length=127)
    rcloneId = models.CharField(max_length=127)
    success = models.BooleanField(default=False)
    
    # Stats dependent
    # Gathred via rclone rc core/stats group=job/<id> --rc-addr=
    eta = models.IntegerField(null=True)
    output = models.TextField(max_length=1023, null=True)
    elapsedTime = models.IntegerField()
    bytes = models.IntegerField()
    checks = models.IntegerField()
    deletedDirs = models.IntegerField()
    deletes = models.IntegerField()
    errors = models.IntegerField()
    fatalError = models.BooleanField(default=False)
    renames = models.IntegerField()
    retryError = models.BooleanField(default=False)
    serverSideCopies = models.IntegerField()
    serverSideCopyBytes = models.IntegerField()
    serverSideMoveBytes = models.IntegerField()
    serverSideMoves = models.IntegerField()
    speed = models.FloatField()
    totalBytes = models.IntegerField()
    totalChecks = models.IntegerField()
    totalTransfers = models.IntegerField()
    transferTime = models.FloatField()
    transfers = models.IntegerField()

class DailyStatistics(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    date = models.DateField(auto_now_add=True)
    bytes = models.BigIntegerField(default=0)
    serverSideCopyBytes = models.BigIntegerField(default=0)
    serverSideMoveBytes = models.BigIntegerField(default=0)
    jobs_run = models.IntegerField(default=0)
    errored_jobs = models.IntegerField(default=0)


# Statistics gathered while the job is running
# There will be multiple (A lot) per job, one every 15 seconds
class jobRunStatistics(models.Model):
    job = models.ForeignKey(Job, on_delete=models.CASCADE)
    dateTime = models.DateTimeField(default=timezone.now)
    
    speed = models.IntegerField(default=0) # Rclone speed
    speedServerSideCopy = models.IntegerField(default=0) # Gathered from ServerSideCopyBytes from last stats subtracting the now ServerSideCopyBytes dividing by the stat update rate in seconds
    speedServerSideMove = models.IntegerField(default=0) # Gathered from ServerSideMoveBytes from last stats subtracting the now ServerSideMoveBytes dividing by the stat update rate in seconds
    
    transferSpeed = models.FloatField() # Gathered from Transfers from last stats subtracting the now Transfers dividing by the stat update rate in seconds
    transferSpeedServerSideCopy = models.FloatField(default=0)
    transferSpeedServerSideMove = models.FloatField(default=0) 
    
    checks = models.FloatField(default=0)