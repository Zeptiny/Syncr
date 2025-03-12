from django.db import models
from django.contrib.auth.models import User

class job(models.Model):
    user = models.ForeignKey(User, on_delete=models.CASCADE)
    
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
    # It is NOT saved:
    # eta (Not relevant)
    # elapsedTime (Already has duration)
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

