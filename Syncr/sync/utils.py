import json
import subprocess
from time import sleep
from . import models
import requests

def queryJobStats(jobId: int) -> dict:
    stats = requests.post("http://127.0.0.1:5572/core/stats", json={
        "group": "job/" + str(jobId)
    })
    stats.raise_for_status()
    
    stats = stats.json()
    # Remove the "transferring" and "eta" key if it exists
    stats.pop("transferring", None)
    stats.pop("eta", None)
    
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

def createJobObject(jobId: int, request) -> int:
    combinedQuery = queryJob(jobId)
    # It only works if the keys are the same in the queries and the model
    jobObject = models.Job(
        user=request.user,
        **combinedQuery
    )
    
    jobObject.save()
    
    return jobObject

def autoQueryRunningJob(jobObject) -> None:
    while(True):
        combinedQuery = queryJob(jobObject.rcloneId)
        
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