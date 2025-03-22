from django.utils import timezone
import json
import subprocess
from time import sleep
from . import models
import requests
import threading

# Job creation functions
def createJobHandler(type: str, srcFs, dstFs, user, **kwargs) -> None:
    # Start the job
    if type == "sync/copy":
        job = requests.post("http://127.0.0.1:5572/sync/copy", json={
            "srcFs": createOnTheFlyRemote(remote=srcFs),
            "dstFs": createOnTheFlyRemote(remote=dstFs),
            "_async": "true"
        })
    else:
        raise ValueError("Invalid job type")    
    
    job.raise_for_status()
    jobId = job.json().get("jobid")
    combinedQuery = queryJob(jobId)
    
    # It only works if the keys are the same in the queries and the model
    jobObject = models.Job.objects.create(
        user=user,
        schedule=kwargs.get("schedule"),
        type=type,
        srcFs=srcFs,
        dstFs=dstFs,
        **combinedQuery
    )
    
    # Crate the first statistic model, as we need one "base"
    # we havent even started the query, so its safe to assume all 0 (I think)
    models.jobRunStatistics.objects.create(
        job = jobObject,
        speed = 0,
        speedServerSideCopy = 0,
        speedServerSideMove = 0,
        transferSpeed = 0,
        transferSpeedServerSideCopy = 0,
        transferSpeedServerSideMove = 0
    )
    
    # Start the auto query thread
    threading.Thread(target=autoQueryRunningJob, args=(jobObject,)).start()
    threading.Thread(target=autoQueryRunningJobStats, args=(jobObject,)).start()
    

# Remote formating functions
def createOnTheFlyRemote(remote) -> str:
    # Each config type has it own format
    # Is it efficient? Hell no, but it works
    if remote.type == "s3":
        # Example config:
        #{
        #     "access_key_id": "<REDACTED>",
        #     "secret_access_key": "<REDACTED>",
        #     "region": "auto",
        #     "endpoint": "https://<REDACTED>.r2.cloudflarestorage.com"
        #     "buckets": ["bucket1", "bucket2"]
        #}
        
        
        formattedRemote = (
            f":{remote.type}," +
            ",".join(f"{key}=\"{value}\"" for key, value in remote.config.items()
                     if key != "bucket") +
            f":{remote.config['bucket']}"
        )
        
    elif remote.type == "sftp":
        formattedRemote = (
            f":{remote.type}," +
            ",".join(f"{key}=\"{value}\"" for key, value in remote.config.items()
                     if key != "pass") + 
            f",pass={obscure(remote.config['pass'])}" +
            ":"
        )
        
    # print(f"REMOTE TYPE: {remote.type}")
    print(formattedRemote)
    
    return formattedRemote

def obscure(plainPass):
    password = requests.post("http://127.0.0.1:5572/core/obscure", json={
        "clear": plainPass
    })
    password.raise_for_status()
    
    return password.json().get("obscured")



# Job query functions
def queryJobStats(jobId: int) -> dict:
    stats = requests.post("http://127.0.0.1:5572/core/stats", json={
        "group": "job/" + str(jobId)
    })
    stats.raise_for_status()
    
    stats = stats.json()
    # Remove the "transferring" and "checking" keys
    stats.pop("transferring", None)
    stats.pop("checking", None)
    
    return stats

def queryJobStatus(jobId: int) -> dict:
    status = requests.post("http://127.0.0.1:5572/job/status", json={
        "jobid": jobId
    })
    status.raise_for_status()
    
    status = status.json()
    # Rename id to rcloneId
    status["rcloneId"] = status.pop("id")
    
    return status

def queryJob(jobId: int) -> dict:
    statusQuery = queryJobStatus(jobId)
    statsQuery = queryJobStats(jobId)
    
    combinedQuery = {**statusQuery, **statsQuery}
    
    return combinedQuery

def autoQueryRunningJob(jobObject) -> None:
    while(True):
        startTime = timezone.now()
        
        combinedQuery = queryJob(jobObject.rcloneId)
        # print(combinedQuery)
        
        # Update the model with the new information
        # It may not be effient to do this way
        for key, value in combinedQuery.items():
            setattr(jobObject, key, value)
        jobObject.save()
        
        print(combinedQuery)
        # If the job is finished, break the loop
        if combinedQuery.get("finished"):
            print(f"Job {jobObject.rcloneId} finished")
            break
        
        endTime = timezone.now()
        
        sleepTime = 1 - (endTime - startTime).total_seconds()
        print(f"Sleeptime: {sleepTime} : {jobObject.rcloneId} : Model")
        if sleepTime > 0:
            sleep(sleepTime)
        else:
            print(f"Job {jobObject.rcloneId} took too long to update, skipping sleep")

def autoQueryRunningJobStats(jobObject) -> None:
    # We need those variables to calculate the speed
    # We don't store it in the model as it's irrelevant
    # (<CurrentTransfers> - <LastTransfers>) / 15
    lastTransfers = lastTransfersSSCopy = lastTransfersSSMove = 0
    while(True):
        startTime = timezone.now()
        
        combinedQuery = queryJob(jobObject.rcloneId)
        
        # If the job is finished, break the loop
        if combinedQuery.get("finished"):
            print(f"Job {jobObject.rcloneId} finished")
            break
        
        # We shall only update if the job isn't finished to not fuck up any data
        # Yes, it means we can lose up to 15 seconds of the last data
        lastStatsRun = models.jobRunStatistics.objects.filter(job=jobObject).latest('dateTime')
        models.jobRunStatistics.objects.create(
            job=jobObject,
            
            speed = combinedQuery.get('speed'),
            speedServerSideCopy = ((combinedQuery.get('serverSideCopyBytes') - lastStatsRun.speedServerSideCopy) / 15),
            speedServerSideMove = ((combinedQuery.get('serverSideMoveBytes') - lastStatsRun.speedServerSideMove) / 15),
            
            transferSpeed = (combinedQuery.get('transfers') - lastTransfers) / 15,
            transferSpeedServerSideCopy = ((combinedQuery.get('serverSideCopies') - lastTransfersSSCopy) / 15),
            transferSpeedServerSideMove = ((combinedQuery.get('serverSideMoves') - lastTransfersSSMove) / 15),
        )
        
        # Update the lastTransfers
        lastTransfers = combinedQuery.get('transfers')
        lastTransfersSSCopy = combinedQuery.get('serverSideCopies')
        lastTransfersSSMove = combinedQuery.get('serverSideMoves')
        
        endTime = timezone.now()
        
        sleepTime = 15 - (endTime - startTime).total_seconds()
        print(f"Sleeptime: {sleepTime} : {jobObject.rcloneId} : Stats")
        if sleepTime > 0:
            sleep(sleepTime)
        else:
            print(f"Job {jobObject.rcloneId} took too long to update, skipping sleep")