from django.utils import timezone
import json
import subprocess
from time import sleep
from . import models
import requests
import threading

# Job creation functions
def createJobHandler(type: str, 
                     srcFs_content_type, srcFs_object_id, srcFsPath, 
                     dstFs_content_type, dstFs_object_id, dstFsPath, 
                     options, server, user, contacts, **kwargs) -> None:
    # Start the job
    if type == "sync/copy" or type == "sync/sync" or type == "sync/move":
        payload = {
            "srcFs": createOnTheFlyFsHandler(fs_type=srcFs_content_type.model, 
                                          fs_object_id=srcFs_object_id,
                                          server=server, 
                                          path=srcFsPath),
            "dstFs": createOnTheFlyFsHandler(fs_type=dstFs_content_type.model,
                                          fs_object_id=dstFs_object_id,
                                          server=server, 
                                          path=dstFsPath),
            "_async": "true",
            "_config": {
                **options
            }
        }
    
    else:
        print(f"Received job type: '{type}' (repr: {repr(type)})")
        raise ValueError("Invalid job type")    
    
    job = requests.post(f"http://{server.host}:{server.port}/{type}", json=payload)
    
    job.raise_for_status()
    jobId = job.json().get("jobid")
    combinedQuery = queryJob(jobId, server)
    
    # It only works if the keys are the same in the queries and the model
    jobObject = models.Job.objects.create(
        user=user,
        schedule=kwargs.get("schedule"),
        type=type,
        srcFs_content_type=srcFs_content_type,
        srcFs_object_id=srcFs_object_id,
        srcFsPath=srcFsPath,
        dstFs_content_type=dstFs_content_type,
        dstFs_object_id=dstFs_object_id,
        dstFsPath=dstFsPath,
        server=server,
        options=options,
        **combinedQuery
    )
    jobObject.contacts.set(contacts) # Many to many field
    jobObject.save()
    
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
def createOnTheFlyFsHandler(fs_type, fs_object_id, server, path) -> str:
    if fs_type == "remote":
        remote = models.Remote.objects.get(id=fs_object_id)
        formattedFs = createOnTheFlyRemote(remote, server, path)
        
    elif fs_type == "union":
        union = models.Union.objects.get(id=fs_object_id)
        
        # Properly format the union remote with quoted upstreams
        upstreams = [
            # BUG / TO-DO, the remote path of a remote that is part of a union can only be the root
            createOnTheFlyRemote(remote, server, "/") for remote in union.remotes.all()
        ]
        
        # Ensure there are no empty upstreams
        if not upstreams:
            raise ValueError("Union remote cannot have empty upstreams.")
        
        # Join upstreams with commas and construct the union remote
        formattedFs = f':union,upstreams="{" ".join(upstreams)}":{path}'
        
    else: 
        raise ValueError(f"Unsupported fs type: {fs_type}")
    
    print(formattedFs)
    return formattedFs
    

def createOnTheFlyRemote(remote, server, path) -> str:
    
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
            ",".join(f"{key}=\'{value}\'" for key, value in remote.config.items()
                     if key != "bucket") +
            f":{remote.config['bucket']}{path}"
        )
        
    elif remote.type == "sftp":
        formattedRemote = (
            f":{remote.type}," +
            ",".join(f"{key}=\"{value}\"" for key, value in remote.config.items()
                     if key != "pass") + 
            f",pass=\"{obscure(remote.config['pass'], server)}\"" +
            f":{path}"
        )
        
    else:
        raise ValueError(f"Unsupported remote type: {remote.type}")
    
    return formattedRemote

def obscure(plainPass, server):
    password = requests.post(f"http://{server.host}:{server.port}/core/obscure", json={
        "clear": plainPass
    })
    password.raise_for_status()
    
    return password.json().get("obscured")



# Job query functions
def queryJobStats(jobId: int, server) -> dict:
    stats = requests.post(f"http://{server.host}:{server.port}/core/stats", json={
        "group": "job/" + str(jobId)
    })
    stats.raise_for_status()
    
    stats = stats.json()
    # Remove the "transferring" and "checking" keys
    stats.pop("transferring", None)
    stats.pop("checking", None)
    
    return stats

def queryJobStatus(jobId: int, server) -> dict:
    status = requests.post(f"http://{server.host}:{server.port}/job/status", json={
        "jobid": jobId
    })
    status.raise_for_status()
    
    status = status.json()
    # Rename id to rcloneId
    status["rcloneId"] = status.pop("id")
    
    return status

def queryJob(jobId: int, server) -> dict:
    statusQuery = queryJobStatus(jobId, server)
    statsQuery = queryJobStats(jobId, server)
    
    combinedQuery = {**statusQuery, **statsQuery}
    
    return combinedQuery

def autoQueryRunningJob(jobObject) -> None:
    while(True):
        startTime = timezone.now()
        
        combinedQuery = queryJob(jobObject.rcloneId, jobObject.server)
        # print(combinedQuery)
        
        # Update the model with the new information
        # It may not be effient to do this way
        for key, value in combinedQuery.items():
            setattr(jobObject, key, value)
        jobObject.save()
        
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
    lastTransfers = lastSpeed = lastTransfersSSCopy = lastTransfersSSMove = lastChecks = lastServerSideCopyBytes = lastServerSideMoveBytes = 0
    while(True):
        startTime = timezone.now()
        
        combinedQuery = queryJob(jobObject.rcloneId, jobObject.server)
        
        # If the job is finished, break the loop
        if combinedQuery.get("finished"):
            print(f"Job {jobObject.rcloneId} finished")
            break
        
        # We shall only update if the job isn't finished to not fuck up any data
        # Yes, it means we can lose up to 15 seconds of the last data
        models.jobRunStatistics.objects.create(
            job=jobObject,
            
            speed = ((combinedQuery.get('speed') - lastSpeed) / 15),
            speedServerSideCopy = ((combinedQuery.get('serverSideCopyBytes') - lastServerSideCopyBytes) / 15),
            speedServerSideMove = ((combinedQuery.get('serverSideMoveBytes') - lastServerSideMoveBytes) / 15),
            
            transferSpeed = (combinedQuery.get('transfers') - lastTransfers) / 15,
            transferSpeedServerSideCopy = ((combinedQuery.get('serverSideCopies') - lastTransfersSSCopy) / 15),
            transferSpeedServerSideMove = ((combinedQuery.get('serverSideMoves') - lastTransfersSSMove) / 15),
            
            checks = ((combinedQuery.get('checks') - lastChecks) / 15),
        )
        
        # Update the lastTransfers
        lastSpeed = combinedQuery.get('speed') # We still get the speed this way so we can get the avarage of the last 15 seconds
        lastServerSideCopyBytes = combinedQuery.get('serverSideCopyBytes')
        lastServerSideMoveBytes = combinedQuery.get('serverSideMoveBytes')
        
        lastTransfers = combinedQuery.get('transfers')
        lastTransfersSSCopy = combinedQuery.get('serverSideCopies')
        lastTransfersSSMove = combinedQuery.get('serverSideMoves')
        
        lastChecks = combinedQuery.get('checks')
        
        endTime = timezone.now()
        
        sleepTime = 15 - (endTime - startTime).total_seconds()
        print(f"Sleeptime: {sleepTime} : {jobObject.rcloneId} : Stats")
        if sleepTime > 0:
            sleep(sleepTime)
        else:
            print(f"Job {jobObject.rcloneId} took too long to update, skipping sleep")