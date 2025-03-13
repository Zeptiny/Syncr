import json
import subprocess
from time import sleep
from . import models
import requests

# Remote formating functions
def createOnTheFlyRemote(config: json) -> str:
    # Each config type has it own format
    # Is it efficient? Hell no, but it works
    if config['type'] == "s3":
        # Example
        #     "type": "s3",
        #     "parameters": {
        #         "access_key_id": "<REDACTED>",
        #         "secret_access_key": "<REDACTED>",
        #         "region": "auto",
        #         "endpoint": "https://<REDACTED>.r2.cloudflarestorage.com"
        #     },
        #     # Formatter specific options
        #     "bucket": "bucket/path"
        # }
        
        
        formattedRemote = (
            f":{config['type']}," +
            ",".join(f"{key}=\"{value}\"" for key, value in config['parameters'].items() 
                     if key != "bucket" 
                     and key != "endpoint") +
            f",endpoint=\"{config['parameters']['endpoint']}\"" +
            f":{config['bucket']}" # Append bucket and path correctly, endpoint NEEDS to be the last parameter
        )
        
        return formattedRemote
        

# Job query functions
def queryJobStats(jobId: int) -> dict:
    stats = requests.post("http://127.0.0.1:5572/core/stats", json={
        "group": "job/" + str(jobId)
    })
    stats.raise_for_status()
    
    stats = stats.json()
    # Remove the "transferring", "eta" and "checking" keys
    stats.pop("transferring", None)
    stats.pop("checking", None)
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