from django.db import models
from django.contrib.auth.models import User

from .settings import TASK_TYPES, REMOTE_TYPES

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
    
class Job(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    schedule = models.ForeignKey(Schedule, on_delete=models.CASCADE, null=True, related_name="jobs")# The schedule that called the job, if it's null, it was created manually/forced
    
    type = models.CharField(choices=TASK_TYPES.items(), max_length=127, default="sync/copy")
    
    srcFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='job_srcFs')
    dstFs = models.ForeignKey(Remote, on_delete=models.CASCADE, null=True, related_name='job_dstFs') # There are jobs that may not require a destination (Eg. delete)

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

