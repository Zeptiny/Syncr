import json
import subprocess
from time import sleep
from . import models
import requests
import threading


# Job creation functions
def createJobHandler(type: str, srcFs, dstFs, request, **kwargs) -> None:
    # Start the job
    print(createOnTheFlyRemote(remote=srcFs))
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
    jobObject = models.Job(
        user=request.user,
        **combinedQuery
    )
    jobObject.save()
    
    # Start the auto query thread
    threading.Thread(target=autoQueryRunningJob, args=(jobObject,)).start()
    

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
        print(formattedRemote)
        
        return formattedRemote
        

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
        combinedQuery = queryJob(jobObject.rcloneId)
        print(combinedQuery)
        
        # Update the model with the new information
        # It may not be effient to do this way
        for key, value in combinedQuery.items():
            setattr(jobObject, key, value)
        jobObject.save()
        
        # If the job is finished, break the loop
        if combinedQuery.get("finished"):
            print("Job finished")
            break
        
        print(f"Updated job {jobObject.rcloneId}")
        sleep(1) # Only check every second